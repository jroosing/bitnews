package blackbox_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tinyNZB is one article. Enough for addfile to queue a Download. No NNTP
// server is configured, so the test does not wait for Ready.
const tinyNZB = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><head><meta type="name">Meta.Name</meta></head>
<file poster="a@b" date="1" subject="&quot;a.rar&quot; yEnc (1/1)"><groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="10240" number="1">a@b</segment></segments></file></nzb>`

func sabGet(d *daemon, query string) response {
	d.t.Helper()
	return d.get("/sabnzbd/api?"+query, nil)
}

func sabAddFile(d *daemon, key, name string, nzb []byte) string {
	d.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("name", name+".nzb")
	require.NoError(d.t, err)
	_, err = fw.Write(nzb)
	require.NoError(d.t, err)
	require.NoError(d.t, mw.Close())
	r := d.request(http.MethodPost, "/sabnzbd/api?mode=addfile&output=json&apikey="+url.QueryEscape(key)+"&nzbname="+url.QueryEscape(name), map[string]string{
		"Content-Type": mw.FormDataContentType(),
	}, buf.String())
	require.Equal(d.t, http.StatusOK, r.status, r.body)
	var out struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
		Error  string   `json:"error"`
	}
	require.NoError(d.t, json.Unmarshal([]byte(r.body), &out), r.body)
	require.True(d.t, out.Status, r.body)
	require.Len(d.t, out.NzoIDs, 1)
	require.True(d.t, strings.HasPrefix(out.NzoIDs[0], "SABnzbd_nzo_"))
	return out.NzoIDs[0]
}

func sabHas(d *daemon, key, mode, nzo string) bool {
	d.t.Helper()
	r := sabGet(d, "mode="+mode+"&apikey="+url.QueryEscape(key))
	var body map[string]any
	require.NoError(d.t, json.Unmarshal([]byte(r.body), &body), r.body)
	sec, _ := body[mode].(map[string]any)
	slots, _ := sec["slots"].([]any)
	for _, s := range slots {
		m, _ := s.(map[string]any)
		if m["nzo_id"] == nzo {
			return true
		}
	}
	return false
}

func TestSABnzbdVersionNeedsNoKey(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	r := sabGet(d, "mode=version")

	assert.Equal(t, http.StatusOK, r.status)
	assert.JSONEq(t, `{"version":"4.5.0"}`, r.body)
}

func TestSABnzbdQueueRequiresTheAppKey(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	missing := sabGet(d, "mode=queue")
	wrong := sabGet(d, "mode=queue&apikey=bitnews_not-the-key")

	assert.Equal(t, http.StatusOK, missing.status)
	assert.JSONEq(t, `{"status":false,"error":"API Key Required"}`, missing.body)
	assert.Equal(t, http.StatusOK, wrong.status)
	assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, wrong.body)
}

func TestSABnzbdGetCatsQueueAndHistoryWithTheAppKey(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	key := url.QueryEscape(cfg.AppKey)

	cats := sabGet(d, "mode=get_cats&apikey="+key)
	queue := sabGet(d, "mode=queue&apikey="+key)
	history := sabGet(d, "mode=history&apikey="+key)
	retry := sabGet(d, "mode=retry&apikey="+key)

	assert.Equal(t, http.StatusOK, cats.status)
	var catBody struct {
		Categories []string `json:"categories"`
	}
	require.NoError(t, json.Unmarshal([]byte(cats.body), &catBody))
	require.NotEmpty(t, catBody.Categories)
	assert.Equal(t, "*", catBody.Categories[0])

	var queueBody struct {
		Queue struct {
			Status    string `json:"status"`
			NoOfSlots int    `json:"noofslots"`
		} `json:"queue"`
	}
	require.NoError(t, json.Unmarshal([]byte(queue.body), &queueBody))
	assert.Equal(t, "Idle", queueBody.Queue.Status)
	assert.Equal(t, 0, queueBody.Queue.NoOfSlots)

	var historyBody struct {
		History struct {
			NoOfSlots int `json:"noofslots"`
		} `json:"history"`
	}
	require.NoError(t, json.Unmarshal([]byte(history.body), &historyBody))
	assert.Equal(t, 0, historyBody.History.NoOfSlots)

	assert.JSONEq(t, `{"status":false,"error":"retry is not supported"}`, retry.body)
}

func TestSABnzbdAddfileAppearsThenDeletes(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	root := t.TempDir()
	inc := filepath.Join(root, "incomplete")
	comp := filepath.Join(root, "complete")
	require.NoError(t, os.Mkdir(inc, 0o700))
	require.NoError(t, os.Mkdir(comp, 0o700))
	r := putWith(t, d, func(m map[string]any) {
		u := section(m, "paths", "usenet")
		u["incomplete"] = inc
		u["complete"] = comp
	})
	require.Equal(t, http.StatusOK, r.status, r.body)

	nzo := sabAddFile(d, cfg.AppKey, "Show.S01E01", []byte(tinyNZB))
	var list string
	eventually(t, 15*time.Second, "nzo in queue or history", func() bool {
		if sabHas(d, cfg.AppKey, "queue", nzo) {
			list = "queue"
			return true
		}
		if sabHas(d, cfg.AppKey, "history", nzo) {
			list = "history"
			return true
		}
		return false
	})

	q := "mode=" + list + "&name=delete&value=" + url.QueryEscape(nzo) + "&apikey=" + url.QueryEscape(cfg.AppKey)
	if list == "history" {
		q += "&del_files=1"
	}
	deleted := sabGet(d, q)
	assert.Equal(t, http.StatusOK, deleted.status)
	var ack struct {
		Status bool `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(deleted.body), &ack), deleted.body)
	assert.True(t, ack.Status, deleted.body)

	eventually(t, 15*time.Second, "nzo left queue and history", func() bool {
		return !sabHas(d, cfg.AppKey, "queue", nzo) && !sabHas(d, cfg.AppKey, "history", nzo)
	})
}
