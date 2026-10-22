package daemon_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
)

// sab calls /sabnzbd/api and decodes the JSON answer.
func (e *e2e) sab(key, query string) map[string]any {
	e.t.Helper()
	q, err := url.ParseQuery(query)
	require.NoError(e.t, err)
	q.Set("output", "json")
	if key != "" {
		q.Set("apikey", key)
	}
	resp, err := e.client.Get(e.ts.URL + "/sabnzbd/api?" + q.Encode())
	require.NoError(e.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	require.NoError(e.t, json.Unmarshal(raw, &out), string(raw))
	return out
}

// sabAddFile uploads the NZB like Sonarr's addfile and returns the nzo_id.
func (e *e2e) sabAddFile(key, name string, nzb []byte) string {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(e.t, mw.WriteField("apikey", key))
	fw, err := mw.CreateFormFile("name", name+".nzb")
	require.NoError(e.t, err)
	_, err = fw.Write(nzb)
	require.NoError(e.t, err)
	require.NoError(e.t, mw.Close())
	resp, err := e.client.Post(e.ts.URL+"/sabnzbd/api?mode=addfile&output=json&cat=tv&nzbname="+url.QueryEscape(name),
		mw.FormDataContentType(), &buf)
	require.NoError(e.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
	}
	require.NoError(e.t, json.Unmarshal(raw, &out), string(raw))
	require.True(e.t, out.Status, string(raw))
	require.Len(e.t, out.NzoIDs, 1)
	return out.NzoIDs[0]
}

func slotStatuses(t *testing.T, resp map[string]any, section string) map[string]string {
	t.Helper()
	sec, ok := resp[section].(map[string]any)
	require.True(t, ok, "%v", resp)
	out := map[string]string{}
	slots, _ := sec["slots"].([]any)
	for _, s := range slots {
		m := s.(map[string]any)
		out[m["nzo_id"].(string)], _ = m["status"].(string)
	}
	return out
}

func TestE2ESABnzbdAdapterDrivesADownloadFromAddfileToCompletedAndDelete(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	srv.SetLatency(5 * time.Millisecond)
	data := payload(200_000, 6)
	fx := nntptest.BuildFixture(t, map[string][]byte{"episode.mkv": data}, 5_000) // 40 Articles
	fx.Install(srv.Server)
	key := e.apiKey

	nzo := e.sabAddFile(key, "Show.S01E01", fx.NZB)
	id := core.ID(strings.TrimPrefix(nzo, "SABnzbd_nzo_"))

	// The queue shows it while it is fetched.
	require.Eventually(t, func() bool {
		_, ok := slotStatuses(t, e.sab(key, "mode=queue"), "queue")[nzo]
		return ok
	}, 10*time.Second, 5*time.Millisecond, "the Download appears in the queue")

	// History says Completed exactly when the Download is Ready. The state
	// is read before and after the history call, because it moves on in
	// between: Ready before means Completed, Completed means Ready after.
	require.Eventually(t, func() bool {
		before := e.get(id).State
		st, ok := slotStatuses(t, e.sab(key, "mode=history"), "history")[nzo]
		if before == core.StateReady {
			assert.Equal(t, "Completed", st, "a Ready Download is Completed")
		}
		switch {
		case !ok:
			return false
		case st == "Completed":
			assert.Equal(t, core.StateReady, e.get(id).State, "Completed only when Ready")
			return true
		}
		return false
	}, 20*time.Second, 5*time.Millisecond)
	assert.NotContains(t, slotStatuses(t, e.sab(key, "mode=queue"), "queue"), nzo, "a finished Download left the queue")
	done := e.get(id)
	assert.Equal(t, data, readTree(t, done.OutputPath)["episode.mkv"])

	del := e.sab(key, "mode=history&name=delete&del_files=1&value="+url.QueryEscape(nzo))
	assert.Equal(t, true, del["status"])
	assert.NotContains(t, slotStatuses(t, e.sab(key, "mode=history"), "history"), nzo)
	assert.NoDirExists(t, done.OutputPath, "del_files=1 removes the output")
	e.call("GET", "/api/v1/downloads/"+string(id), nil, http.StatusNotFound, nil)
}

func TestE2EOneKeyOpensTheOwnAPISABnzbdAndQBitAndAWrongOneNone(t *testing.T) {
	e := newE2E(t)
	e.start()
	key := e.apiKey

	assert.Equal(t, map[string]any{"status": false, "error": "API Key Incorrect"}, e.sab("bitnews_not-the-key", "mode=queue"))
	assert.Equal(t, map[string]any{"status": false, "error": "API Key Required"}, e.sab("", "mode=queue"))
	assert.Contains(t, e.sab(key, "mode=get_cats"), "categories")
	e.call("GET", "/api/v1/downloads", nil, http.StatusOK, nil)
	q := &qbitClient{t: t, e: e}
	assert.Equal(t, "Fails.", q.login("bitnews", "bitnews_not-the-key"))
	assert.Equal(t, "Fails.", q.login("sonarr", key), "the username is fixed")
	require.Equal(t, "Ok.", q.login("bitnews", key))
	status, _ := q.get("app/version")
	assert.Equal(t, http.StatusOK, status)
	for _, hdr := range []string{"bitnews_not-the-key", ""} {
		req, err := http.NewRequest("GET", e.ts.URL+"/api/v1/downloads", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+hdr)
		resp, err := e.client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}
}

func TestE2ETheStoredKeyIsSealedAndSurvivesARestart(t *testing.T) {
	e := newE2E(t)
	e.start()
	e.stop()

	d := e.build()
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)

	resp, err := e.client.Get(ts.URL + "/sabnzbd/api?mode=get_cats&output=json&apikey=" + e.apiKey)
	require.NoError(t, err)
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Contains(t, string(raw), "categories", "the restarted daemon decrypted the same key")
	var sealed string
	require.NoError(t, e.db.Reader().QueryRow(`SELECT sealed_key FROM app_key`).Scan(&sealed))
	assert.NotContains(t, sealed, e.apiKey)
	assert.NotContains(t, e.logs.String(), e.apiKey, "the key is never logged")
}
