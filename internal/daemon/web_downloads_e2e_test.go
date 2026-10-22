package daemon_test

import (
	"bytes"
	"crypto/sha1"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/core/coretest"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
)

// htmxPost sends a multipart or form post like htmx does: CSRF header,
// HX-Request and the same-origin Origin.
func (w *webClient) htmxPost(path string, ct string, body io.Reader, csrf string) (*http.Response, string) {
	w.t.Helper()
	req, err := http.NewRequest("POST", w.base+path, body)
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", w.base)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := w.c.Do(req)
	require.NoError(w.t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(w.t, err)
	_ = resp.Body.Close()
	return resp, string(b)
}

func (w *webClient) htmxForm(path string, form url.Values, csrf string) (*http.Response, string) {
	return w.htmxPost(path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), csrf)
}

func (w *webClient) login() string {
	w.t.Helper()
	resp, body := w.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"correct horse battery staple"}})
	require.Equal(w.t, http.StatusSeeOther, resp.StatusCode, body)
	return w.csrf()
}

// The whole story of M6-04 over real HTTP: add an NZB in the dialog, see it
// Ready in the history with its output path, open its detail page, see a
// Failed one's failure card, and remove with and without the files.
func TestE2EWebAddHistoryDetailAndRemove(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	w := newWebClient(t, e.ts.URL)
	csrf := w.login()

	// Add an NZB through the "New download" dialog.
	fxOK := nntptest.BuildFixture(t, map[string][]byte{"ok.bin": payload(30_000, 1)}, 10_000)
	fxOK.Install(srv.Server)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "ui-added.nzb")
	require.NoError(t, err)
	_, err = fw.Write(fxOK.NZB)
	require.NoError(t, err)
	require.NoError(t, mw.WriteField("name", "UI.Added.Release"))
	require.NoError(t, mw.WriteField("priority", "normal"))
	require.NoError(t, mw.Close())
	resp, body := w.htmxPost("/downloads/add", mw.FormDataContentType(), &buf, csrf)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, resp.Header.Get("HX-Trigger"), "download-added")

	_, queue := w.do("GET", "/queue", nil)
	assert.Contains(t, queue, "UI.Added.Release", "it appears in the queue")
	var list struct {
		Items []dl `json:"items"`
	}
	e.call("GET", "/api/v1/downloads", nil, http.StatusOK, &list)
	require.Len(t, list.Items, 1)
	added := list.Items[0]
	ready := e.waitState(added.ID, core.StateReady)

	// Ready: in the history with its output path; the detail page loads.
	resp, history := w.do("GET", "/history", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, history, "UI.Added.Release")
	assert.Contains(t, history, ready.OutputPath)
	resp, detail := w.do("GET", "/downloads/"+string(ready.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, detail, ready.OutputPath)
	assert.Contains(t, detail, "Every article arrived intact")
	assert.NotContains(t, detail, `hx-trigger="every 3s"`, "a Ready download is not polled")

	// A damaged release beyond repair: the failure card says why.
	g := loadGolden(t, "explicit_uniform")
	fxBad := nntptest.BuildFixture(t, g.files(), g.Manifest.SliceSize)
	fxBad.Install(srv.Server)
	u1 := fixtureIDs(t, fxBad, "u1.bin")
	srv.Missing(u1[3:25]...)
	failed := e.waitState(e.addNZB("Hopeless.Release", fxBad.NZB, "").ID, core.StateFailed)
	resp, detail = w.do("GET", "/downloads/"+string(failed.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, detail, "Failed: Not enough repair data")
	assert.Contains(t, detail, "Sonarr and Radarr will usually grab another release")
	_, history = w.do("GET", "/history?state=failed", nil)
	assert.Contains(t, history, "Hopeless.Release")
	assert.NotContains(t, history, "UI.Added.Release")

	// Remove the Failed one keeping files; remove the Ready one with them.
	resp, body = w.htmxForm("/downloads/"+string(failed.ID)+"/delete", url.Values{"delete_data": {"0"}, "from": {"history"}}, csrf)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = w.htmxForm("/downloads/"+string(ready.ID)+"/delete", url.Values{"delete_data": {"1"}, "from": {"history"}}, csrf)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NoDirExists(t, ready.OutputPath)
	assert.DirExists(t, filepath.Dir(ready.OutputPath), "the complete root stays")
	resp, _ = w.do("GET", "/downloads/"+string(ready.ID), nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	_, history = w.do("GET", "/history", nil)
	assert.NotContains(t, history, "Hopeless.Release")
	assert.NotContains(t, history, "UI.Added.Release")
}

// A torrent added in the dialog shows its engine facts on the detail page, and
// the share limit form reaches the torrent runner.
func TestE2EWebTorrentDetailAndShareLimit(t *testing.T) {
	e := newE2E(t)
	e.torrents = true
	fake := coretest.NewFakeTorrentEngine()
	e.engine = fake
	e.start()
	w := newWebClient(t, e.ts.URL)
	csrf := w.login()

	data, _ := coretest.BuildTorrent("Detail.Torrent", []coretest.TorrentEntry{
		{Path: []string{"a.mkv"}, Size: 20000}, {Path: []string{"sub", "b.txt"}, Size: 5},
	})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "detail.torrent")
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	resp, body := w.htmxPost("/downloads/add", mw.FormDataContentType(), &buf, csrf)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var list struct {
		Items []dl `json:"items"`
	}
	e.call("GET", "/api/v1/downloads", nil, http.StatusOK, &list)
	require.Len(t, list.Items, 1)
	id := list.Items[0].ID

	hash := core.InfoHash(sha1.Sum(data)) // the fake engine's hash of a metainfo
	require.Eventually(t, func() bool {
		for _, c := range fake.Calls() {
			if c.Method == "Add" && c.Hash == hash {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "the runner adds the torrent to the engine")
	fake.SetStats(hash, core.TorrentStats{
		Name: "Detail.Torrent", State: core.TorrentDownloading, Size: 20005, DoneBytes: 100, Peers: 4, Seeds: 2,
		Files: []core.TorrentFile{{Path: "a.mkv", Size: 20000}, {Path: "sub/b.txt", Size: 5}},
	})
	require.Eventually(t, func() bool {
		_, page := w.do("GET", "/downloads/"+string(id), nil)
		return strings.Contains(page, "sub/b.txt")
	}, 10*time.Second, 50*time.Millisecond)
	_, page := w.do("GET", "/downloads/"+string(id), nil)
	assert.Contains(t, page, "Global default: no limit")

	resp, body = w.htmxForm("/downloads/"+string(id)+"/share-limit", url.Values{"mode": {"custom"}, "ratio": {"1.5"}, "minutes": {"60"}}, csrf)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	_, page = w.do("GET", "/downloads/"+string(id), nil)
	assert.Contains(t, page, "Own limit: ratio 1.5 or seed for 1 h 0 min")

	resp, body = w.htmxForm("/downloads/"+string(id)+"/share-limit", url.Values{"mode": {"default"}}, csrf)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	_, page = w.do("GET", "/downloads/"+string(id), nil)
	assert.Contains(t, page, "Global default: no limit")
}
