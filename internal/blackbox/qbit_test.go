package blackbox_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/torrent/torrenttest"
)

// The qBittorrent calls Sonarr makes before it lists torrents: login with the
// app key, then version and an empty info list.
func TestQBitLoginThenVersionAndEmptyInfo(t *testing.T) {
	d := startDaemon(t, prepareConfigDir(t))
	defer d.stop()

	bad := d.request(http.MethodPost, "/api/v2/auth/login", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}, "username=bitnews&password=nope")
	assert.Equal(t, http.StatusOK, bad.status, bad.body)
	assert.Equal(t, "Fails.", bad.body)

	noSid := d.request(http.MethodGet, "/api/v2/app/version", nil, "")
	assert.Equal(t, http.StatusForbidden, noSid.status, noSid.body)

	ok := d.request(http.MethodPost, "/api/v2/auth/login", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}, "username=bitnews&password="+url.QueryEscape(d.cfg.AppKey))
	require.Equal(t, http.StatusOK, ok.status, ok.body)
	require.Equal(t, "Ok.", ok.body)
	sid := ok.header.Get("Set-Cookie")
	require.Contains(t, sid, "SID=")

	ver := d.request(http.MethodGet, "/api/v2/app/version", map[string]string{"Cookie": sid}, "")
	require.Equal(t, http.StatusOK, ver.status, ver.body)
	assert.Equal(t, "v4.6.7", ver.body)

	api := d.request(http.MethodGet, "/api/v2/app/webapiVersion", map[string]string{"Cookie": sid}, "")
	require.Equal(t, http.StatusOK, api.status, api.body)
	assert.Equal(t, "2.9.3", api.body)

	info := d.request(http.MethodGet, "/api/v2/torrents/info", map[string]string{"Cookie": sid}, "")
	require.Equal(t, http.StatusOK, info.status, info.body)
	assert.JSONEq(t, "[]", info.body)
}

func TestQBitAddPauseDelete(t *testing.T) {
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	s := core.DefaultSettings()
	s.Paths.Usenet = core.Roots{Incomplete: roots.usenetIncomplete, Complete: roots.complete}
	s.Paths.Torrent = core.Roots{Incomplete: roots.torrentIncomplete, Complete: roots.torrentComplete}
	s.Queues.MinFree = 1 << 20
	s.Queues.Torrent = 2
	s.Torrent.DisableDHT = true
	saveSettings(t, cfg, s)

	d := startDaemon(t, cfg)
	defer d.stop()
	sid := qbitLogin(t, d)

	const hash = "0123456789abcdef0123456789abcdef01234567"
	magnet := "magnet:?xt=urn:btih:" + hash + "&dn=QbitShow"
	add := d.request(http.MethodPost, "/api/v2/torrents/add", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Cookie":       sid,
	}, "urls="+url.QueryEscape(magnet))
	require.Equal(t, http.StatusOK, add.status, add.body)
	require.Equal(t, "Ok.", add.body)

	items := qbitInfo(t, d, sid)
	require.Len(t, items, 1)
	assert.Equal(t, hash, items[0]["hash"])
	assert.Equal(t, "QbitShow", items[0]["name"])
	assert.Equal(t, "metaDL", items[0]["state"])

	pause := d.request(http.MethodPost, "/api/v2/torrents/pause", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Cookie":       sid,
	}, "hashes="+hash)
	require.Equal(t, http.StatusOK, pause.status, pause.body)
	require.True(t, pause.body == "" || pause.body == "Ok.", pause.body)
	items = qbitInfo(t, d, sid)
	require.Len(t, items, 1)
	assert.Equal(t, "pausedDL", items[0]["state"])

	del := d.request(http.MethodPost, "/api/v2/torrents/delete", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Cookie":       sid,
	}, "hashes="+hash+"&deleteFiles=true")
	require.Equal(t, http.StatusOK, del.status, del.body)
	require.True(t, del.body == "" || del.body == "Ok.", del.body)
	require.Empty(t, qbitInfo(t, d, sid))
}

func TestQBitPreferencesCategoriesAndProperties(t *testing.T) {
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	s := core.DefaultSettings()
	s.Paths.Usenet = core.Roots{Incomplete: roots.usenetIncomplete, Complete: roots.complete}
	s.Paths.Torrent = core.Roots{Incomplete: roots.torrentIncomplete, Complete: roots.torrentComplete}
	s.Queues.MinFree = 1 << 20
	s.Queues.Torrent = 2
	s.Torrent.DisableDHT = true
	saveSettings(t, cfg, s)

	d := startDaemon(t, cfg)
	defer d.stop()
	sid := qbitLogin(t, d)
	h := map[string]string{"Cookie": sid}

	prefs := d.request(http.MethodGet, "/api/v2/app/preferences", h, "")
	require.Equal(t, http.StatusOK, prefs.status, prefs.body)
	var p map[string]any
	require.NoError(t, json.Unmarshal([]byte(prefs.body), &p), prefs.body)
	assert.Equal(t, roots.torrentComplete, p["save_path"])
	assert.Equal(t, false, p["dht"])
	assert.Equal(t, true, p["queueing_enabled"])
	assert.EqualValues(t, 2, p["max_active_downloads"])

	set := d.request(http.MethodPost, "/api/v2/app/setPreferences", h, "json={}")
	require.Equal(t, http.StatusOK, set.status, set.body)

	empty := d.request(http.MethodPost, "/api/v2/torrents/createCategory", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Cookie":       sid,
	}, "category=")
	assert.Equal(t, http.StatusBadRequest, empty.status, empty.body)

	made := d.request(http.MethodPost, "/api/v2/torrents/createCategory", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Cookie":       sid,
	}, "category=QbitTV")
	require.Equal(t, http.StatusOK, made.status, made.body)
	require.True(t, made.body == "" || made.body == "Ok.", made.body)

	cats := d.request(http.MethodGet, "/api/v2/torrents/categories", h, "")
	require.Equal(t, http.StatusOK, cats.status, cats.body)
	var cm map[string]map[string]any
	require.NoError(t, json.Unmarshal([]byte(cats.body), &cm), cats.body)
	require.Contains(t, cm, "qbittv")
	assert.Equal(t, "qbittv", cm["qbittv"]["name"])

	const hash = "0123456789abcdef0123456789abcdef01234567"
	add := d.request(http.MethodPost, "/api/v2/torrents/add", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Cookie":       sid,
	}, "urls="+url.QueryEscape("magnet:?xt=urn:btih:"+hash+"&dn=QbitShow"))
	require.Equal(t, "Ok.", add.body, add.body)

	missing := d.request(http.MethodGet, "/api/v2/torrents/properties?hash=zz", h, "")
	assert.Equal(t, http.StatusNotFound, missing.status, missing.body)

	props := d.request(http.MethodGet, "/api/v2/torrents/properties?hash="+hash, h, "")
	require.Equal(t, http.StatusOK, props.status, props.body)
	var prop map[string]any
	require.NoError(t, json.Unmarshal([]byte(props.body), &prop), props.body)
	assert.Equal(t, hash, prop["hash"])

	files := d.request(http.MethodGet, "/api/v2/torrents/files?hash="+hash, h, "")
	require.Equal(t, http.StatusOK, files.status, files.body)
	var fl []any
	require.NoError(t, json.Unmarshal([]byte(files.body), &fl), files.body)
	assert.Empty(t, fl)

	tags := d.request(http.MethodGet, "/api/v2/torrents/tags", h, "")
	require.Equal(t, http.StatusOK, tags.status, tags.body)
	assert.JSONEq(t, "[]", tags.body)

	form := func(path, body string) response {
		t.Helper()
		return d.request(http.MethodPost, path, map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"Cookie":       sid,
		}, body)
	}
	missingCat := form("/api/v2/torrents/setCategory", "hashes="+hash+"&category=missing")
	assert.Equal(t, http.StatusConflict, missingCat.status, missingCat.body)

	setCat := form("/api/v2/torrents/setCategory", "hashes="+hash+"&category=QbitTV")
	require.Equal(t, http.StatusOK, setCat.status, setCat.body)
	var catID string
	for _, it := range itemsOf(t, d.apiGet("/api/v1/downloads?limit=50")) {
		if it["name"] == "QbitShow" {
			catID, _ = it["category_id"].(string)
		}
	}
	require.NotEmpty(t, catID)
	items := qbitInfo(t, d, sid)
	require.NotEmpty(t, items)
	assert.Equal(t, "qbittv", items[0]["category"])

	sync := d.request(http.MethodGet, "/api/v2/sync/maindata?rid=4", h, "")
	require.Equal(t, http.StatusOK, sync.status, sync.body)
	var md map[string]any
	require.NoError(t, json.Unmarshal([]byte(sync.body), &md), sync.body)
	assert.EqualValues(t, 5, md["rid"])
	assert.Equal(t, true, md["full_update"])
	torrents, _ := md["torrents"].(map[string]any)
	require.Contains(t, torrents, hash)

	force := form("/api/v2/torrents/setForceStart", "hashes="+hash+"&value=true")
	require.Equal(t, http.StatusOK, force.status, force.body)
	var priority string
	for _, it := range itemsOf(t, d.apiGet("/api/v1/downloads?limit=50")) {
		if it["name"] == "QbitShow" {
			priority, _ = it["priority"].(string)
		}
	}
	assert.Equal(t, "force", priority)

	badLimit := form("/api/v2/torrents/setShareLimits", "hashes="+hash+"&seedingTimeLimit=10")
	assert.Equal(t, http.StatusBadRequest, badLimit.status, badLimit.body)
	okLimit := form("/api/v2/torrents/setShareLimits", "hashes="+hash+"&ratioLimit=1.5&seedingTimeLimit=10")
	require.Equal(t, http.StatusOK, okLimit.status, okLimit.body)

	top := form("/api/v2/torrents/topPrio", "hashes="+hash)
	require.Equal(t, http.StatusOK, top.status, top.body)
}

func TestQBitAddTorrentFile(t *testing.T) {
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	s := core.DefaultSettings()
	s.Paths.Usenet = core.Roots{Incomplete: roots.usenetIncomplete, Complete: roots.complete}
	s.Paths.Torrent = core.Roots{Incomplete: roots.torrentIncomplete, Complete: roots.torrentComplete}
	s.Queues.MinFree = 1 << 20
	s.Torrent.DisableDHT = true
	saveSettings(t, cfg, s)

	content := torrenttest.MakeRandom(t, "Movie.2026", map[string]int{"clip.mkv": 8_000})
	d := startDaemon(t, cfg)
	defer d.stop()
	sid := qbitLogin(t, d)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("category", "Films"))
	fw, err := mw.CreateFormFile("torrents", "Movie.2026.torrent")
	require.NoError(t, err)
	_, err = fw.Write(content.Torrent)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	add := d.request(http.MethodPost, "/api/v2/torrents/add", map[string]string{
		"Content-Type": mw.FormDataContentType(),
		"Cookie":       sid,
	}, buf.String())
	require.Equal(t, http.StatusOK, add.status, add.body)
	require.Equal(t, "Ok.", add.body)

	items := qbitInfo(t, d, sid)
	require.Len(t, items, 1)
	assert.Equal(t, content.Hash.String(), items[0]["hash"])
	assert.Equal(t, "queuedDL", items[0]["state"])
	assert.Equal(t, "films", items[0]["category"])
}

func qbitLogin(t *testing.T, d *daemon) string {
	t.Helper()
	ok := d.request(http.MethodPost, "/api/v2/auth/login", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}, "username=bitnews&password="+url.QueryEscape(d.cfg.AppKey))
	require.Equal(t, http.StatusOK, ok.status, ok.body)
	require.Equal(t, "Ok.", ok.body)
	return ok.header.Get("Set-Cookie")
}

func qbitInfo(t *testing.T, d *daemon, sid string) []map[string]any {
	t.Helper()
	r := d.request(http.MethodGet, "/api/v2/torrents/info", map[string]string{"Cookie": sid}, "")
	require.Equal(t, http.StatusOK, r.status, r.body)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &items), r.body)
	return items
}
