//go:build e2e

package daemon_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/torrent/torrenttest"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
)

// These tests drive the qBittorrent adapter over real HTTP, through the whole
// daemon with the real torrent engine, the way Sonarr and Radarr would.

// qbitClient is a minimal qBittorrent WebUI client.
type qbitClient struct {
	t    *testing.T
	e    *e2e
	sid  string
	user string
	pass string
}

func (e *e2e) newQBit() *qbitClient {
	e.t.Helper()
	return &qbitClient{t: e.t, e: e, user: auth.QBitUsername, pass: e.apiKey}
}

func (c *qbitClient) do(method, path string, body io.Reader, contentType string) (int, string, []*http.Cookie) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.e.ts.URL+"/api/v2/"+path, body)
	require.NoError(c.t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.sid != "" {
		req.AddCookie(&http.Cookie{Name: "SID", Value: c.sid})
	}
	resp, err := c.e.client.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return resp.StatusCode, string(raw), resp.Cookies()
}

func (c *qbitClient) login(user, pass string) string {
	c.t.Helper()
	status, body, cookies := c.do("POST", "auth/login", strings.NewReader(url.Values{"username": {user}, "password": {pass}}.Encode()),
		"application/x-www-form-urlencoded")
	require.Equal(c.t, http.StatusOK, status)
	for _, ck := range cookies {
		if ck.Name == "SID" {
			c.sid = ck.Value
		}
	}
	return body
}

func (c *qbitClient) form(path string, v url.Values) (int, string) {
	c.t.Helper()
	status, body, _ := c.do("POST", path, strings.NewReader(v.Encode()), "application/x-www-form-urlencoded")
	return status, body
}

func (c *qbitClient) get(path string) (int, string) {
	c.t.Helper()
	status, body, _ := c.do("GET", path, nil, "")
	return status, body
}

// qbitTorrent is the part of a torrents/info entry the tests read.
type qbitTorrent struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	Category     string  `json:"category"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	Size         int64   `json:"size"`
	AmountLeft   int64   `json:"amount_left"`
	CompletionOn int64   `json:"completion_on"`
	RatioLimit   float64 `json:"ratio_limit"`
	SeedingLimit int64   `json:"seeding_time_limit"`
}

func (c *qbitClient) info(query string) []qbitTorrent {
	c.t.Helper()
	status, body := c.get("torrents/info" + query)
	require.Equal(c.t, http.StatusOK, status, body)
	var out []qbitTorrent
	require.NoError(c.t, json.Unmarshal([]byte(body), &out), body)
	return out
}

// addTorrentFile uploads a .torrent as torrents/add does.
func (c *qbitClient) addFile(raw []byte, fields map[string]string) (int, string) {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(c.t, mw.WriteField(k, v))
	}
	pw, err := mw.CreateFormFile("torrents", "x.torrent")
	require.NoError(c.t, err)
	_, err = pw.Write(raw)
	require.NoError(c.t, err)
	require.NoError(c.t, mw.Close())
	status, body, _ := c.do("POST", "torrents/add", &buf, mw.FormDataContentType())
	return status, body
}

var uploadStates = map[string]bool{"uploading": true, "stalledUP": true, "pausedUP": true, "stoppedUP": true, "forcedUP": true, "queuedUP": true}

// waitComplete polls torrents/info until the torrent looks complete and
// checks the invariant on every answer: whenever the adapter shows progress
// 1 or an upload state, the Download is Ready. (Ready is final, so reading it
// after the answer is sound.) It returns the states it saw.
func (c *qbitClient) waitComplete(hash string, id core.ID) (qbitTorrent, []string) {
	c.t.Helper()
	var seen []string
	var last qbitTorrent
	require.Eventually(c.t, func() bool {
		list := c.info("?hashes=" + hash)
		if len(list) != 1 {
			return false
		}
		last = list[0]
		if len(seen) == 0 || seen[len(seen)-1] != last.State {
			seen = append(seen, last.State)
		}
		if last.State == "checkingUP" {
			assert.Less(c.t, last.Progress, 1.0, "the import hold-off: checkingUP is never complete")
		}
		looksComplete := last.Progress >= 1 || uploadStates[last.State]
		if looksComplete {
			assert.Equal(c.t, core.StateReady, c.e.get(id).State, "the adapter showed %s at progress %v before the Download was Ready", last.State, last.Progress)
		}
		return looksComplete
	}, 30*time.Second, 5*time.Millisecond, "never looked complete; saw %v", seen)
	return last, seen
}

func TestQBitLoginIsTheFixedUsernameAndTheKeyOnly(t *testing.T) {
	e := newTorrentE2E(t)
	e.start()
	c := e.newQBit()

	assert.Equal(t, "Fails.", c.login("bitnews", "wrong"))
	assert.Equal(t, "Fails.", c.login("nobody", c.pass))
	status, _ := c.get("torrents/info")
	assert.Equal(t, http.StatusForbidden, status, "no session, no access")

	// Wait out the per-IP backoff of the failed logins before the good one.
	good := &qbitClient{t: t, e: e}
	require.Eventually(t, func() bool { return good.login(c.user, c.pass) == "Ok." }, 20*time.Second, 200*time.Millisecond)
	require.NotEmpty(t, good.sid)
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/downloads", nil)
	req.AddCookie(&http.Cookie{Name: "SID", Value: good.sid})
	resp, err := e.client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the adapter session is not an own-API session")
}

func TestQBitMagnetEndToEnd(t *testing.T) {
	e := newTorrentE2E(t)
	c0 := torrenttest.MakeRandom(t, "Show.S01.1080p", map[string]int{"ep1.mkv": 90_000, "ep2.mkv": 70_000})
	seeder := torrenttest.Seed(t, c0)
	e.start()
	q := e.newQBit()
	require.Equal(t, "Ok.", q.login(q.user, q.pass))
	hash := c0.Hash.String()

	status, body := q.form("torrents/add", url.Values{"urls": {c0.Magnet(torrenttest.Addr(seeder))}, "category": {"tv"}})
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "Ok.", body)

	// The Download exists in the own API and the adapter lists it by hash.
	var id core.ID
	require.Eventually(t, func() bool {
		var page struct {
			Items []dl `json:"items"`
		}
		e.call("GET", "/api/v1/downloads?kind=torrent", nil, http.StatusOK, &page)
		if len(page.Items) == 1 {
			id = page.Items[0].ID
		}
		return id != ""
	}, 5*time.Second, 10*time.Millisecond)
	final, seen := q.waitComplete(hash, id)
	t.Logf("states seen by the media manager: %v", seen)

	ready := e.get(id)
	require.Equal(t, core.StateReady, ready.State)
	assert.Contains(t, []string{"uploading", "stalledUP"}, final.State)
	assert.EqualValues(t, 1, final.Progress)
	assert.Equal(t, "Show.S01.1080p", final.Name)
	assert.Equal(t, "tv", final.Category)
	assert.Equal(t, c0.Size(), final.Size)
	assert.Zero(t, final.AmountLeft)
	assert.Equal(t, ready.OutputPath, final.ContentPath, "the media manager imports from the Download's output")
	rec := e.torrentRecord(id)
	assert.Equal(t, filepath.Dir(rec.Location), final.SavePath, "save_path is the directory that holds the Download directory")
	assert.DirExists(t, final.ContentPath)
	assert.Equal(t, -2.0, final.RatioLimit, "the global limit is reported as -2")

	// The files.
	status, body = q.get("torrents/files?hash=" + hash)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "Show.S01.1080p/ep1.mkv")

	// Share limits round trip through the info.
	status, _ = q.form("torrents/setShareLimits", url.Values{"hashes": {hash}, "ratioLimit": {"2.5"}, "seedingTimeLimit": {"120"}, "inactiveSeedingTimeLimit": {"-2"}})
	require.Equal(t, http.StatusOK, status)
	got := q.info("?hashes=" + hash)[0]
	assert.Equal(t, 2.5, got.RatioLimit)
	assert.EqualValues(t, 120, got.SeedingLimit)
	status, _ = q.form("torrents/setShareLimits", url.Values{"hashes": {hash}, "ratioLimit": {"-2"}, "seedingTimeLimit": {"-2"}})
	require.Equal(t, http.StatusOK, status)
	got = q.info("?hashes=" + hash)[0]
	assert.Equal(t, -2.0, got.RatioLimit)
	assert.EqualValues(t, -2, got.SeedingLimit)
	status, _ = q.form("torrents/setShareLimits", url.Values{"hashes": {hash}, "ratioLimit": {"-1"}, "seedingTimeLimit": {"-1"}})
	require.Equal(t, http.StatusOK, status)
	got = q.info("?hashes=" + hash)[0]
	assert.Equal(t, -1.0, got.RatioLimit)

	// Deleting without the files removes the Download and keeps the data.
	data := ready.OutputPath
	status, _ = q.form("torrents/delete", url.Values{"hashes": {hash}, "deleteFiles": {"false"}})
	require.Equal(t, http.StatusOK, status)
	assert.Empty(t, q.info(""), "gone from the adapter")
	e.call("GET", "/api/v1/downloads/"+string(id), nil, http.StatusNotFound, nil)
	assert.DirExists(t, data, "deleteFiles=false keeps the data")
	c0.AssertFilesEqual(t, rec.Location)
	require.Eventually(t, func() bool { _, err := e.realEngine().Stats(c0.Hash); return err != nil },
		5*time.Second, 10*time.Millisecond, "the engine let go of the torrent")
}

func TestQBitTorrentFileDeleteWithFilesAndDuplicates(t *testing.T) {
	e := newTorrentE2E(t)
	c0 := torrenttest.MakeRandom(t, "Movie.2026", map[string]int{"movie.mkv": 80_000})
	seeder := torrenttest.Seed(t, c0)
	e.start()
	q := e.newQBit()
	require.Equal(t, "Ok.", q.login(q.user, q.pass))
	hash := c0.Hash.String()

	status, body := q.addFile(c0.Torrent, map[string]string{"category": "movies"})
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "Ok.", body)
	require.Eventually(t, func() bool { return e.realEngine().AddPeers(c0.Hash, torrenttest.Addr(seeder)) == nil },
		10*time.Second, 10*time.Millisecond)
	var id core.ID
	require.Eventually(t, func() bool {
		list := q.info("?hashes=" + hash)
		if len(list) == 1 {
			var page struct {
				Items []dl `json:"items"`
			}
			e.call("GET", "/api/v1/downloads?kind=torrent", nil, http.StatusOK, &page)
			id = page.Items[0].ID
		}
		return id != ""
	}, 5*time.Second, 10*time.Millisecond)
	q.waitComplete(hash, id)

	// The same torrent again is refused, as a duplicate.
	status, body = q.addFile(c0.Torrent, nil)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Fails.", body)
	status, body = q.form("torrents/add", url.Values{"urls": {c0.Magnet()}})
	assert.Equal(t, "Fails.", body)
	assert.Equal(t, http.StatusOK, status)

	// Something that is not a torrent fails without a trace.
	status, body = q.addFile([]byte("this is not bencode"), nil)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Fails.", body)

	// delete with the files removes the whole Download directory.
	rec := e.torrentRecord(id)
	status, _ = q.form("torrents/delete", url.Values{"hashes": {hash}, "deleteFiles": {"true"}})
	require.Equal(t, http.StatusOK, status)
	assert.NoDirExists(t, rec.Location)
	assert.Empty(t, q.info(""))
}

func TestQBitPausedAddWaitsAndResumeStartsIt(t *testing.T) {
	e := newTorrentE2E(t)
	c0 := torrenttest.MakeRandom(t, "Paused.One", map[string]int{"f.bin": 30_000})
	seeder := torrenttest.Seed(t, c0)
	e.start()
	q := e.newQBit()
	require.Equal(t, "Ok.", q.login(q.user, q.pass))
	hash := c0.Hash.String()

	status, body := q.form("torrents/add", url.Values{"urls": {c0.Magnet(torrenttest.Addr(seeder))}, "paused": {"true"}})
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "Ok.", body)

	require.Eventually(t, func() bool { return len(q.info("?hashes="+hash)) == 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	got := q.info("?hashes=" + hash)[0]
	assert.Equal(t, "pausedDL", got.State)
	assert.Zero(t, got.Progress)

	status, _ = q.form("torrents/resume", url.Values{"hashes": {hash}})
	require.Equal(t, http.StatusOK, status)
	var page struct {
		Items []dl `json:"items"`
	}
	e.call("GET", "/api/v1/downloads?kind=torrent", nil, http.StatusOK, &page)
	require.Len(t, page.Items, 1)
	q.waitComplete(hash, page.Items[0].ID)
}

func TestQBitSetShareLimitsPausesASeedingTorrentAtOnce(t *testing.T) {
	e := newTorrentE2E(t)
	c0 := torrenttest.MakeRandom(t, "Limited.One", map[string]int{"f.bin": 40_000})
	seeder := torrenttest.Seed(t, c0)
	e.start()
	q := e.newQBit()
	require.Equal(t, "Ok.", q.login(q.user, q.pass))
	hash := c0.Hash.String()
	status, _ := q.form("torrents/add", url.Values{"urls": {c0.Magnet(torrenttest.Addr(seeder))}})
	require.Equal(t, http.StatusOK, status)
	var page struct {
		Items []dl `json:"items"`
	}
	require.Eventually(t, func() bool {
		e.call("GET", "/api/v1/downloads?kind=torrent", nil, http.StatusOK, &page)
		return len(page.Items) == 1
	}, 5*time.Second, 10*time.Millisecond)
	q.waitComplete(hash, page.Items[0].ID)
	require.NoError(t, seeder.Close())
	leech(t, c0, torrenttest.Addr(e.realEngine())) // one full copy uploaded

	// A ratio of 0.5 is already reached: the torrent stops, as pausedUP.
	status, _ = q.form("torrents/setShareLimits", url.Values{"hashes": {hash}, "ratioLimit": {"0.5"}, "seedingTimeLimit": {"-1"}})
	require.Equal(t, http.StatusOK, status)

	require.Eventually(t, func() bool { return q.info("?hashes=" + hash)[0].State == "pausedUP" },
		10*time.Second, 20*time.Millisecond, "paused by the Share Limit")
	assert.Equal(t, core.StateReady, e.get(page.Items[0].ID).State)
}

func TestQBitIsTorrentOnlyAndNeverShowsNZBs(t *testing.T) {
	e := newTorrentE2E(t)
	e.start()
	q := e.newQBit()
	require.Equal(t, "Ok.", q.login(q.user, q.pass))
	fx := nntptest.BuildFixture(t, map[string][]byte{"a.bin": payload(5000, 3)}, 4096)
	nzbDL := e.addNZB("An.NZB", fx.NZB, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		_, _ = w.Write(fx.NZB)
	}))
	t.Cleanup(srv.Close)

	assert.Empty(t, q.info(""), "an NZB Download is not a torrent")
	status, body := q.form("torrents/add", url.Values{"urls": {srv.URL + "/release.nzb"}})
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Fails.", body, "the qBit API is torrent-only")

	var page struct {
		Items []dl `json:"items"`
	}
	e.call("GET", "/api/v1/downloads", nil, http.StatusOK, &page)
	require.Len(t, page.Items, 1, "the refused NZB left nothing behind")
	assert.Equal(t, nzbDL.ID, page.Items[0].ID)
}
