package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/core/coretest"
)

var showEntries = []coretest.TorrentEntry{
	{Path: []string{"ep1.mkv"}, Size: 20000},
	{Path: []string{"extras", "readme.txt"}, Size: 5},
}

const magnetHash = "0123456789abcdef0123456789abcdef01234567"

func magnetLink(passkey string) string {
	return "magnet:?xt=urn:btih:" + magnetHash + "&dn=Some.Magnet&tr=http%3A%2F%2Ftracker.example%2Fannounce%3Fpasskey%3D" + passkey
}

// torrentRoot is the torrent incomplete directory of an env made by newAddEnv.
func (e *env) torrentRoot() string { return e.settings.Current().Paths.Torrent.Incomplete }

func (e *env) uploadTorrent(key string, data []byte, extra ...formPart) *httptest.ResponseRecorder {
	e.t.Helper()
	parts := append([]formPart{{"torrent", "release.torrent", string(data)}}, extra...)
	body, ct := multipartBody(e.t, parts...)
	return e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct))
}

func jsonMagnet(magnet, extra string) string {
	return `{"magnet":"` + magnet + `"` + extra + `}`
}

type errorFull struct {
	Error struct {
		Code         string `json:"code"`
		Message      string `json:"message"`
		ExistingID   string `json:"existing_id"`
		ExistingName string `json:"existing_name"`
	} `json:"error"`
}

func TestUploadTorrentCreatesAQueuedTorrentDownloadAndStoresTheFile(t *testing.T) {
	t.Parallel()
	e, usenetRoot := newAddEnv(t)
	key := e.apiKey()
	events := e.subscribe()
	data, hash := coretest.BuildTorrent("Some.Show.S01", showEntries)

	rec := e.uploadTorrent(key, data)

	got := e.addedDownload(rec)
	assert.Equal(t, "queued", got.State)
	assert.Equal(t, "torrent", got.SourceKind)
	assert.Equal(t, "Some.Show.S01", got.Name)
	assert.Equal(t, "normal", got.Priority)
	assert.False(t, got.Paused)
	assert.Equal(t, 0, got.QueuePosition)
	assert.EqualValues(t, 20005, got.TotalBytes)

	dir := filepath.Join(e.torrentRoot(), got.ID)
	path := filepath.Join(dir, "Some.Show.S01.torrent")
	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, stored)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, []string{dir, path}, leftovers(t, e.torrentRoot()), "no temp files remain")
	assert.Empty(t, leftovers(t, usenetRoot))

	src, err := e.torrents.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, hash, src.InfoHash)
	assert.Equal(t, path, src.TorrentPath)
	evs := events()
	require.Len(t, evs, 1)
	added, ok := evs[0].(core.DownloadAdded)
	require.True(t, ok)
	assert.Equal(t, core.ID(got.ID), added.Download.ID)
}

func TestUploadTorrentHonorsFormFieldsInAnyOrder(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, e.cats.Create(context.Background(), cat))
	data, _ := coretest.BuildTorrent("Inner", showEntries)
	body, ct := multipartBody(t,
		formPart{"name", "", "My Show"},
		formPart{"category_id", "", string(cat.ID)},
		formPart{"torrent", "x.torrent", string(data)},
		formPart{"priority", "", "high"},
		formPart{"paused", "", "true"},
	)

	got := e.addedDownload(e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct)))

	assert.Equal(t, "My Show", got.Name)
	require.NotNil(t, got.CategoryID)
	assert.Equal(t, string(cat.ID), *got.CategoryID)
	assert.Equal(t, "high", got.Priority)
	assert.True(t, got.Paused)
	assert.Equal(t, "queued", got.State)
}

func TestAddMagnetCreatesAFetchingMetadataDownloadAndSealsTheLink(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()
	events := e.subscribe()
	link := magnetLink("MAGNETSECRET")

	rec := e.addURL(key, jsonMagnet(link, ""))

	got := e.addedDownload(rec)
	assert.Equal(t, "fetching_metadata", got.State)
	assert.Equal(t, "torrent", got.SourceKind)
	assert.Equal(t, "Some.Magnet", got.Name)
	assert.Zero(t, got.TotalBytes)
	assert.NotContains(t, rec.Body.String(), "MAGNETSECRET", "the link is not echoed")
	assert.Empty(t, leftovers(t, e.torrentRoot()), "a magnet writes no files")

	src, err := e.torrents.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, link, src.Magnet.Reveal())
	assert.Equal(t, magnetHash, src.InfoHash.String())
	var raw string
	require.NoError(t, e.db.Reader().QueryRow(`SELECT magnet FROM torrent_sources WHERE download_id = ?`, got.ID).Scan(&raw))
	assert.True(t, strings.HasPrefix(raw, "v1:"), "sealed at rest, got %q", raw)
	assert.NotContains(t, raw, "MAGNETSECRET")
	for _, table := range []string{"downloads", "torrent_sources"} {
		dump := dumpTable(t, e, table)
		assert.NotContains(t, dump, "MAGNETSECRET", table)
	}
	assert.Len(t, events(), 1)
}

func dumpTable(t *testing.T, e *env, table string) string {
	t.Helper()
	rows, err := e.db.Reader().Query(`SELECT * FROM ` + table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		for _, v := range vals {
			b.WriteString(strings.ToValidUTF8(toString(v), "?"))
			b.WriteString("|")
		}
	}
	return b.String()
}

func toString(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	}
	return ""
}

func TestAddMagnetHonorsTheJSONFields(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()
	cat := core.Category{ID: core.NewID(), Name: "movies"}
	require.NoError(t, e.cats.Create(context.Background(), cat))

	got := e.addedDownload(e.addURL(key, jsonMagnet(magnetLink("k"), `,"name":"Some Movie","category_id":"`+string(cat.ID)+`","priority":"low","paused":true`)))

	assert.Equal(t, "Some Movie", got.Name)
	assert.Equal(t, "low", got.Priority)
	assert.True(t, got.Paused)
	assert.Equal(t, "fetching_metadata", got.State)
	require.NotNil(t, got.CategoryID)
	assert.Equal(t, string(cat.ID), *got.CategoryID)
}

func TestAddByURLFetchesATorrentAndStoresIt(t *testing.T) {
	t.Parallel()
	data, hash := coretest.BuildTorrent("Fetched.Release", showEntries)
	ix := newIndexer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(data)
	})
	e, _ := newAddEnv(t)
	key := e.apiKey()

	got := e.addedDownload(e.addURL(key, jsonURL(ix.URL+"/dl?apikey="+urlSecret, "")))

	assert.Equal(t, "torrent", got.SourceKind)
	assert.Equal(t, "queued", got.State)
	assert.Equal(t, "Fetched.Release", got.Name)
	src, err := e.torrents.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, hash, src.InfoHash)
	stored, err := os.ReadFile(src.TorrentPath)
	require.NoError(t, err)
	assert.Equal(t, data, stored)
}

func TestAddByURLFollowsARedirectToAMagnetLinkOnce(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "/dl?apikey="+urlSecret, http.StatusFound)
			return
		}
		w.Header().Set("Location", magnetLink("REDIRECTSECRET"))
		w.WriteHeader(http.StatusFound)
	})
	e, _ := newAddEnv(t)
	key := e.apiKey()

	direct := e.addURL(key, jsonURL(ix.URL+"/dl?apikey="+urlSecret, ""))
	got := e.addedDownload(direct)

	assert.Equal(t, "fetching_metadata", got.State)
	assert.Equal(t, "Some.Magnet", got.Name)
	src, err := e.torrents.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, magnetLink("REDIRECTSECRET"), src.Magnet.Reveal(), "the link is kept exactly as the server sent it")
	assert.Equal(t, 1, ix.hits(), "the magnet link is not fetched")
	assert.NotContains(t, e.log.String(), "REDIRECTSECRET")
	assert.NotContains(t, e.log.String(), urlSecret)

	hopped := e.addURL(key, jsonURL(ix.URL+"/hop", ""))
	assert.Equal(t, http.StatusConflict, hopped.Code, "the same hash again, reached through a hop")
	assert.Equal(t, "duplicate_torrent", errorOf(t, hopped))
}

func TestAddByURLStillStoresAnNZBWhenTorrentsAreEnabled(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, serveNZB(nzbXML(metaName)))
	e, root := newAddEnv(t)
	key := e.apiKey()

	got := e.addedDownload(e.addURL(key, jsonURL(ix.URL, "")))

	assert.Equal(t, "nzb", got.SourceKind)
	assert.FileExists(t, filepath.Join(root, got.ID, "Meta.Release.Name.nzb"))
	assert.Empty(t, leftovers(t, e.torrentRoot()))
}

func TestAddByURLOfABrokenTorrentIs400AndLeavesNothing(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("d4:infoi5ee")) })
	e, _ := newAddEnv(t)
	key := e.apiKey()

	rec := e.addURL(key, jsonURL(ix.URL, ""))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid_torrent", errorOf(t, rec))
	assert.Empty(t, leftovers(t, e.torrentRoot()))
}

func TestAddByURLOfAHostileTorrentLeavesNothingOnDisk(t *testing.T) {
	t.Parallel()
	data, _ := coretest.BuildTorrent("root", []coretest.TorrentEntry{{Path: []string{"..", "..", "evil"}, Size: 1}})
	ix := newIndexer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	e, _ := newAddEnv(t)
	key := e.apiKey()

	rec := e.addURL(key, jsonURL(ix.URL, ""))

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "invalid_torrent", errorOf(t, rec))
	assert.Empty(t, leftovers(t, e.torrentRoot()))
}

func TestAddByURLRedactsTheKeyInLogsForTorrentFetchesToo(t *testing.T) {
	t.Parallel()
	data, _ := coretest.BuildTorrent("x", showEntries)
	ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		_, _ = w.Write(data)
	})
	e, _ := newAddEnv(t)
	key := e.apiKey()

	ok := e.addURL(key, jsonURL(ix.URL+"/dl?apikey="+urlSecret+"&t=get", ""))
	bad := e.addURL(key, jsonURL(ix.URL+"/bad?apikey="+urlSecret, ""))

	assert.Equal(t, http.StatusCreated, ok.Code)
	require.Equal(t, http.StatusBadGateway, bad.Code)
	assert.Equal(t, "fetch_failed", errorOf(t, bad))
	assert.NotContains(t, bad.Body.String(), urlSecret)
	logs := e.log.String()
	assert.NotContains(t, logs, urlSecret)
	assert.Contains(t, logs, "torrent fetched")
	assert.Contains(t, logs, "apikey=")
}

func TestUploadTorrentOverTheLimitIs413AndLeavesNothing(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()
	data, _ := coretest.BuildTorrent("x", showEntries)
	data = append(data, make([]byte, core.MaxMetainfoSize)...)

	rec := e.uploadTorrent(key, data)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "torrent_too_large", errorOf(t, rec))
	assert.Empty(t, leftovers(t, e.torrentRoot()))
}

func TestUploadOfAnInvalidTorrentIs400AndLeavesNothing(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()
	for name, data := range map[string]string{
		"not bencode": "this is not a torrent",
		"empty":       "",
		"an nzb":      nzbXML(""),
		"no info":     "d8:announce3:fooe",
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.uploadTorrent(key, []byte(data))

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, "invalid_torrent", errorOf(t, rec))
			assert.Empty(t, leftovers(t, e.torrentRoot()))
		})
	}
}

func TestUploadOfAHostileTorrentIs400AndNothingIsLeftOnDisk(t *testing.T) {
	t.Parallel()
	e, usenetRoot := newAddEnv(t)
	key := e.apiKey()
	for name, entries := range map[string][]coretest.TorrentEntry{
		"traversal":      {{Path: []string{"..", "..", "evil"}, Size: 1}},
		"case collision": {{Path: []string{"A"}, Size: 1}, {Path: []string{"a"}, Size: 1}},
		"file is a dir":  {{Path: []string{"a"}, Size: 1}, {Path: []string{"a", "b"}, Size: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := coretest.BuildTorrent("root", entries)
			rec := e.uploadTorrent(key, data)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, "invalid_torrent", errorOf(t, rec))
			assert.Empty(t, leftovers(t, e.torrentRoot()))
			assert.Empty(t, leftovers(t, usenetRoot))
		})
	}
	list, _, err := e.downloads.List(context.Background(), core.DownloadQuery{})
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestAddDuplicateTorrentIs409WithTheExistingDownload(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()
	data, hash := coretest.BuildTorrent("First.Copy", showEntries)
	first := e.addedDownload(e.uploadTorrent(key, data))
	events := e.subscribe()
	before := leftovers(t, e.torrentRoot())

	again := e.uploadTorrent(key, data, formPart{"name", "", "Second"})
	viaMagnet := e.addURL(key, jsonMagnet("magnet:?xt=urn:btih:"+hash.String()+"&tr=http://t/a?passkey=LEAK", ""))

	for _, rec := range []*httptest.ResponseRecorder{again, viaMagnet} {
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		var body errorFull
		decode(t, rec, &body)
		assert.Equal(t, "duplicate_torrent", body.Error.Code)
		assert.Equal(t, first.ID, body.Error.ExistingID)
		assert.Equal(t, "First.Copy", body.Error.ExistingName)
		assert.NotContains(t, rec.Body.String(), "LEAK")
	}
	assert.Equal(t, before, leftovers(t, e.torrentRoot()), "the duplicates left no files")
	assert.Empty(t, events())
	list, _, err := e.downloads.List(context.Background(), core.DownloadQuery{})
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestTorrentQueuePositionsAreIndependentOfTheNZBQueue(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()
	nzb1 := e.addedDownload(e.upload(key, nzbXML("")))
	nzb2 := e.addedDownload(e.upload(key, nzbXML("")))

	t1, _ := coretest.BuildTorrent("one", showEntries)
	t2, _ := coretest.BuildTorrent("two", showEntries[:1])
	got1 := e.addedDownload(e.uploadTorrent(key, t1))
	got2 := e.addedDownload(e.uploadTorrent(key, t2))
	got3 := e.addedDownload(e.addURL(key, jsonMagnet(magnetLink("k"), "")))

	assert.Equal(t, []int{0, 1}, []int{nzb1.QueuePosition, nzb2.QueuePosition})
	assert.Equal(t, []int{0, 1, 2}, []int{got1.QueuePosition, got2.QueuePosition, got3.QueuePosition})
}

func TestAddTorrentRejectsInvalidFieldsWithoutLeftovers(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()
	data, _ := coretest.BuildTorrent("x", showEntries)
	for name, tc := range map[string]struct {
		extra []formPart
		code  string
	}{
		"unknown category": {[]formPart{{"category_id", "", "nope"}}, "unknown_category"},
		"bad priority":     {[]formPart{{"priority", "", "urgent"}}, "invalid_priority"},
		"bad paused":       {[]formPart{{"paused", "", "maybe"}}, "invalid_paused"},
		"unknown field":    {[]formPart{{"magnet", "", magnetLink("k")}}, "unknown_field"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.uploadTorrent(key, data, tc.extra...)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
			assert.Empty(t, leftovers(t, e.torrentRoot()))
		})
	}
	for name, tc := range map[string]struct{ body, code string }{
		"magnet unknown category": {jsonMagnet(magnetLink("k"), `,"category_id":"nope"`), "unknown_category"},
		"magnet bad priority":     {jsonMagnet(magnetLink("k"), `,"priority":"urgent"`), "invalid_priority"},
		"bad magnet":              {jsonMagnet("magnet:?xt=urn:btih:short&tr=http://t/a?passkey=LEAKME", ""), "invalid_magnet"},
		"not a magnet":            {jsonMagnet("http://example.com/a.torrent", ""), "invalid_magnet"},
		"both sources":            {`{"url":"http://example.com/a","magnet":"` + magnetLink("k") + `"}`, "ambiguous_source"},
		"no source":               {`{"name":"x"}`, "missing_url"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.addURL(key, tc.body)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
			assert.NotContains(t, rec.Body.String(), "LEAKME")
		})
	}
	assert.NotContains(t, e.log.String(), "LEAKME")
	list, _, err := e.downloads.List(context.Background(), core.DownloadQuery{})
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestUploadWithTwoSourcesIs400(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.apiKey()
	data, _ := coretest.BuildTorrent("x", showEntries)
	body, ct := multipartBody(t, formPart{"torrent", "a.torrent", string(data)}, formPart{"nzb", "a.nzb", nzbXML("")})

	rec := e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "multiple_sources", errorOf(t, rec))
	assert.Empty(t, leftovers(t, e.torrentRoot()))
	assert.Empty(t, leftovers(t, root))
}

// failingTorrentSources fails Create.
type failingTorrentSources struct {
	core.TorrentSourceRepo
}

func (failingTorrentSources) Create(context.Context, core.TorrentSource) error {
	return errors.New("disk on fire")
}

func TestAddTorrentWhenTheSourceCannotBeStoredLeavesNothing(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	broken := newEnv(t, withAddOptions(app.WithTorrentSources(failingTorrentSources{e.torrents})))
	s := broken.settings.Current()
	s.Paths.Torrent.Incomplete = e.torrentRoot()
	require.NoError(t, broken.settings.Update(context.Background(), s))
	key := broken.apiKey()
	events := broken.subscribe()
	data, _ := coretest.BuildTorrent("x", showEntries)

	file := broken.uploadTorrent(key, data)
	magnet := broken.addURL(key, jsonMagnet(magnetLink("k"), ""))

	assert.Equal(t, http.StatusInternalServerError, file.Code)
	assert.Equal(t, http.StatusInternalServerError, magnet.Code)
	assert.Equal(t, "internal", errorOf(t, file))
	assert.Empty(t, leftovers(t, e.torrentRoot()))
	assert.Empty(t, events())
	list, _, err := broken.downloads.List(context.Background(), core.DownloadQuery{})
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestAddTorrentNeedsTheKeyOrASessionWithCSRF(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	sess := e.login()
	data, _ := coretest.BuildTorrent("x", showEntries)
	body, ct := multipartBody(t, formPart{"torrent", "x.torrent", string(data)})
	ctMod := withHeader("Content-Type", ct)
	magnet := jsonMagnet(magnetLink("k"), "")

	anonymous := e.do("POST", "/api/v1/downloads", body, ctMod)
	anonymousMagnet := e.do("POST", "/api/v1/downloads", magnet)
	noToken := e.do("POST", "/api/v1/downloads", body, withCookie(sess.cookie), ctMod)
	noTokenMagnet := e.do("POST", "/api/v1/downloads", magnet, withCookie(sess.cookie))
	withToken := e.do("POST", "/api/v1/downloads", body, append(sess.mods(), ctMod)...)
	magnetWithToken := e.do("POST", "/api/v1/downloads", jsonMagnet("magnet:?xt=urn:btih:"+strings.Repeat("2", 40), ""), sess.mods()...)

	assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
	assert.Equal(t, http.StatusUnauthorized, anonymousMagnet.Code)
	for _, rec := range []*httptest.ResponseRecorder{noToken, noTokenMagnet} {
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, "csrf_token_invalid", errorOf(t, rec))
	}
	assert.Equal(t, http.StatusCreated, withToken.Code, withToken.Body.String())
	assert.Equal(t, http.StatusCreated, magnetWithToken.Code, magnetWithToken.Body.String())
	assert.Len(t, leftovers(t, e.torrentRoot()), 2, "only the accepted request left a download dir")
}

func TestAddTorrentWithoutTorrentSupportIs503(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withConfig(func(c *api.Config) {
		c.Adder = app.NewAddService(c.Downloads, e0Sources{}, c.Settings)
	}))
	key := e.apiKey()
	data, _ := coretest.BuildTorrent("x", showEntries)

	file := e.uploadTorrent(key, data)
	magnet := e.addURL(key, jsonMagnet(magnetLink("k"), ""))

	for _, rec := range []*httptest.ResponseRecorder{file, magnet} {
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		assert.Equal(t, "torrents_unavailable", errorOf(t, rec))
	}
}

// e0Sources is a DownloadSourceRepo that is never used.
type e0Sources struct{ core.DownloadSourceRepo }
