package app_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/core/coretest"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

type torrentSettings struct{ usenet, torrent string }

func (s torrentSettings) Current() core.Settings {
	st := core.DefaultSettings()
	st.Paths.Usenet.Incomplete = s.usenet
	st.Paths.Torrent.Incomplete = s.torrent
	return st
}

type torrentRig struct {
	*stack
	svc      *app.AddService
	sources  *store.TorrentSourceRepo
	key      store.Key
	usenet   string
	torrents string
}

func newTorrentRig(t *testing.T, opts ...app.Option) *torrentRig {
	t.Helper()
	s := newStack(t)
	r := &torrentRig{stack: s, key: store.GenerateKey()}
	base := t.TempDir()
	r.usenet, r.torrents = filepath.Join(base, "usenet"), filepath.Join(base, "torrents")
	r.sources = store.NewTorrentSourceRepo(s.db, r.key)
	ds := app.NewDownloadService(s.downloads, s.cats, s.bus)
	opts = append([]app.Option{app.WithClock(s.clock.Now), app.WithTorrentSources(r.sources)}, opts...)
	r.svc = app.NewAddService(ds, store.NewDownloadSourceRepo(s.db, r.key), torrentSettings{r.usenet, r.torrents}, opts...)
	return r
}

var showFiles = []coretest.TorrentEntry{
	{Path: []string{"ep1.mkv"}, Size: 20000},
	{Path: []string{"extras", "readme.txt"}, Size: 5},
}

func TestAddTorrentFromAFileStoresAQueuedDownloadAndItsSource(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	data, hash := coretest.BuildTorrent("Some.Show.S01", showFiles)

	st, err := r.svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	d, err := r.svc.AddTorrent(ctx, st, app.AddRequest{Priority: core.DownloadPriorityHigh})

	require.NoError(t, err)
	assert.Equal(t, core.SourceTorrent, d.SourceKind)
	assert.Equal(t, core.StateQueued, d.State)
	assert.Equal(t, "Some.Show.S01", d.Name)
	assert.EqualValues(t, 20005, d.TotalBytes)
	assert.Equal(t, core.DownloadPriorityHigh, d.Priority)
	stored, err := r.downloads.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, stored, d)

	src, err := r.sources.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, hash, src.InfoHash)
	assert.Empty(t, src.Magnet)
	dir := filepath.Join(r.torrents, string(d.ID))
	assert.Equal(t, filepath.Join(dir, "Some.Show.S01.torrent"), src.TorrentPath)
	onDisk, err := os.ReadFile(src.TorrentPath)
	require.NoError(t, err)
	assert.Equal(t, data, onDisk)
	fi, err := os.Stat(src.TorrentPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	assert.Equal(t, []string{dir, src.TorrentPath}, entries(t, r.torrents))
	assert.Empty(t, entries(t, r.usenet), "nothing in the usenet root")

	evs := r.events()
	require.Len(t, evs, 1)
	assert.Equal(t, core.DownloadAdded{Download: stored}, evs[0])
}

const hexHash = "0123456789abcdef0123456789abcdef01234567"

func magnetURI(passkey string) string {
	return "magnet:?xt=urn:btih:" + hexHash + "&dn=Some.Magnet.Release&tr=http%3A%2F%2Ftracker.example%2Fannounce%3Fpasskey%3D" + passkey
}

func TestAddMagnetStoresAFetchingMetadataDownloadWithASealedLinkAndNoFiles(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)

	st, err := r.svc.StageMagnet(ctx, " "+magnetURI("SECRETKEY")+"\n")
	require.NoError(t, err)
	assert.True(t, st.IsMagnet())
	d, err := r.svc.AddTorrent(ctx, st, app.AddRequest{})

	require.NoError(t, err)
	assert.Equal(t, core.StateFetchingMetadata, d.State)
	assert.Equal(t, core.SourceTorrent, d.SourceKind)
	assert.Equal(t, "Some.Magnet.Release", d.Name)
	assert.Zero(t, d.TotalBytes, "unknown until the metadata arrives")
	src, err := r.sources.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, magnetURI("SECRETKEY"), src.Magnet.Reveal())
	assert.Empty(t, src.TorrentPath)
	assert.Equal(t, hexHash, src.InfoHash.String())
	var raw string
	require.NoError(t, r.db.Reader().QueryRow(`SELECT magnet FROM torrent_sources WHERE download_id = ?`, string(d.ID)).Scan(&raw))
	assert.NotContains(t, raw, "SECRETKEY")
	assert.Empty(t, entries(t, r.torrents), "a magnet writes nothing")
	evs := r.events()
	require.Len(t, evs, 1)
	assert.Equal(t, core.DownloadAdded{Download: d}, evs[0])
}

func TestStageMagnetRefusesBadLinksWithoutEchoingThem(t *testing.T) {
	r := newTorrentRig(t)

	for _, uri := range []string{"", "http://example.com/x.torrent", "magnet:?xt=urn:btih:short&tr=http://t/a?passkey=LEAKME", "magnet:?dn=x"} {
		_, err := r.svc.StageMagnet(context.Background(), uri)

		var appErr *app.Error
		require.ErrorAs(t, err, &appErr, uri)
		assert.Equal(t, "invalid_magnet", appErr.Code)
		assert.ErrorIs(t, err, app.ErrInvalidInput)
		assert.NotContains(t, err.Error(), "LEAKME")
	}
}

func TestAddTorrentNameComesFromTheRequestThenTheTorrentThenTheFileNameThenTheID(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	named, _ := coretest.BuildTorrent("Inner.Name", showFiles)
	noName, _ := coretest.BuildTorrent("-", []coretest.TorrentEntry{{Path: []string{"a.bin"}, Size: 3}})
	cases := []struct {
		data []byte
		req  app.AddRequest
		want func(core.ID) string
	}{
		{named, app.AddRequest{Name: "Requested", FileName: "up.torrent"}, func(core.ID) string { return "Requested" }},
		{named, app.AddRequest{FileName: "up.torrent"}, func(core.ID) string { return "Inner.Name" }},
		{noName, app.AddRequest{FileName: "Upload.Name.TORRENT"}, func(core.ID) string { return "Upload.Name" }},
		{noName, app.AddRequest{}, func(id core.ID) string { return "torrent-" + string(id) }},
	}
	for _, c := range cases {
		st, err := r.svc.StageTorrent(ctx, bytes.NewReader(c.data))
		require.NoError(t, err)
		d, err := r.svc.AddTorrent(ctx, st, c.req)
		require.NoError(t, err)
		assert.Equal(t, c.want(d.ID), d.Name)
		// the next loop adds the same torrent again: drop this one
		require.NoError(t, r.downloads.Delete(ctx, d.ID))
	}
}

func TestAddTorrentAppliesCategoryPriorityAndPausedLikeNZBs(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	require.NoError(t, r.cats.Create(ctx, core.Category{ID: "cat1", Name: "tv"}))
	data, _ := coretest.BuildTorrent("x", showFiles)

	st, err := r.svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	d, err := r.svc.AddTorrent(ctx, st, app.AddRequest{CategoryID: "cat1", Priority: core.DownloadPriorityLow, Paused: true})

	require.NoError(t, err)
	assert.Equal(t, core.ID("cat1"), d.CategoryID)
	assert.Equal(t, core.DownloadPriorityLow, d.Priority)
	assert.True(t, d.Paused)
	assert.Equal(t, core.StateQueued, d.State)

	st, err = r.svc.StageMagnet(ctx, "magnet:?xt=urn:btih:"+strings.Repeat("1", 40))
	require.NoError(t, err)
	_, err = r.svc.AddTorrent(ctx, st, app.AddRequest{CategoryID: "nope"})
	var appErr *app.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "unknown_category", appErr.Code)
	_, err = r.svc.AddTorrent(ctx, mustMagnet(t, r, strings.Repeat("2", 40)), app.AddRequest{Priority: "urgent"})
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "invalid_priority", appErr.Code)
}

func mustMagnet(t *testing.T, r *torrentRig, hash string) *app.StagedTorrent {
	t.Helper()
	st, err := r.svc.StageMagnet(context.Background(), "magnet:?xt=urn:btih:"+hash)
	require.NoError(t, err)
	return st
}

func TestTorrentQueuePositionsAreIndependentOfTheUsenetQueue(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	for range 3 {
		r.addDownload(t, "nzb", 0) // three Queued NZB Downloads
	}
	nzb, err := r.svc.Stage(ctx, strings.NewReader(tinyNZB))
	require.NoError(t, err)
	nd, err := r.svc.Add(ctx, nzb, app.AddRequest{})
	require.NoError(t, err)
	require.Positive(t, nd.QueuePosition)

	var positions []int
	for _, h := range []string{"1", "2", "3"} {
		d, err := r.svc.AddTorrent(ctx, mustMagnet(t, r, strings.Repeat(h, 40)), app.AddRequest{})
		require.NoError(t, err)
		positions = append(positions, d.QueuePosition)
	}

	assert.Equal(t, []int{0, 1, 2}, positions)
	nd2, err := r.downloads.Get(ctx, nd.ID)
	require.NoError(t, err)
	assert.Equal(t, nd.QueuePosition, nd2.QueuePosition)
}

func TestAddTorrentWithAKnownInfoHashIsADuplicateAndLeavesNothing(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	data, hash := coretest.BuildTorrent("First", showFiles)
	st, err := r.svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	first, err := r.svc.AddTorrent(ctx, st, app.AddRequest{})
	require.NoError(t, err)
	r.events()
	before := entries(t, r.torrents)

	again, err := r.svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	_, err = r.svc.AddTorrent(ctx, again, app.AddRequest{Name: "Second"})

	var dup app.ErrDuplicate
	require.ErrorAs(t, err, &dup)
	assert.Equal(t, first.ID, dup.ExistingID)
	assert.Equal(t, "First", dup.Name)
	assert.ErrorIs(t, err, core.ErrConflict)
	assert.Equal(t, before, entries(t, r.torrents), "the second copy's files are gone")
	all, _, lerr := r.downloads.List(ctx, core.DownloadQuery{})
	require.NoError(t, lerr)
	assert.Len(t, all, 1)
	assert.Empty(t, r.events())

	// A magnet with the same hash is a duplicate too.
	_, err = r.svc.AddTorrent(ctx, mustMagnet(t, r, hash.String()), app.AddRequest{})
	require.ErrorAs(t, err, &dup)
}

func TestStageTorrentRefusesHostileAndBrokenFilesAndLeavesNothingOnDisk(t *testing.T) {
	r := newTorrentRig(t)
	traversal, _ := coretest.BuildTorrent("root", []coretest.TorrentEntry{{Path: []string{"..", "..", "evil"}, Size: 1}})
	colliding, _ := coretest.BuildTorrent("root", []coretest.TorrentEntry{{Path: []string{"A"}, Size: 1}, {Path: []string{"a"}, Size: 1}})
	cases := map[string][]byte{
		"path traversal": traversal,
		"colliding":      colliding,
		"not bencode":    []byte("<nzb></nzb>"),
		"empty":          nil,
		"no info":        []byte("d8:announce3:fooe"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.svc.StageTorrent(context.Background(), bytes.NewReader(data))

			var appErr *app.Error
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, "invalid_torrent", appErr.Code)
			assert.ErrorIs(t, err, app.ErrInvalidInput)
			assert.Empty(t, entries(t, r.torrents))
		})
	}
}

func TestStageTorrentRefusesAFileOverTheCap(t *testing.T) {
	r := newTorrentRig(t)
	data, _ := coretest.BuildTorrent("x", showFiles)
	data = append(data, make([]byte, core.MaxMetainfoSize)...)

	_, err := r.svc.StageTorrent(context.Background(), bytes.NewReader(data))

	var appErr *app.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "torrent_too_large", appErr.Code)
	assert.ErrorIs(t, err, app.ErrTooLarge)
	assert.Empty(t, entries(t, r.torrents))
}

func TestTorrentAddsNeedTorrentSources(t *testing.T) {
	s := newStack(t)
	svc := app.NewAddService(app.NewDownloadService(s.downloads, s.cats, s.bus), failingSources{}, torrentSettings{"/u", t.TempDir()})
	data, _ := coretest.BuildTorrent("x", showFiles)

	_, err1 := svc.StageTorrent(context.Background(), bytes.NewReader(data))
	_, err2 := svc.StageMagnet(context.Background(), magnetURI("k"))

	for _, err := range []error{err1, err2} {
		var appErr *app.Error
		require.ErrorAs(t, err, &appErr)
		assert.Equal(t, "torrents_unavailable", appErr.Code)
		assert.ErrorIs(t, err, app.ErrUnavailable)
	}
}

func TestDiscardRemovesAStagedTorrentAndIsIdempotent(t *testing.T) {
	r := newTorrentRig(t)
	data, _ := coretest.BuildTorrent("x", showFiles)
	st, err := r.svc.StageTorrent(context.Background(), bytes.NewReader(data))
	require.NoError(t, err)
	require.NotEmpty(t, entries(t, r.torrents))

	st.Discard()
	st.Discard()

	assert.Empty(t, entries(t, r.torrents))
	(*app.StagedTorrent)(nil).Discard()
}

// failingTorrentSources fails Create.
type failingTorrentSources struct {
	core.TorrentSourceRepo
	err error
}

func (f failingTorrentSources) Create(context.Context, core.TorrentSource) error { return f.err }

func TestAddTorrentRemovesTheDownloadAndFilesWhenTheSourceCannotBeStored(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	ds := app.NewDownloadService(r.downloads, r.cats, r.bus)
	svc := app.NewAddService(ds, nil, torrentSettings{r.usenet, r.torrents},
		app.WithTorrentSources(failingTorrentSources{r.sources, errors.New("disk on fire")}))
	data, _ := coretest.BuildTorrent("x", showFiles)
	st, err := svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	_, err = svc.AddTorrent(ctx, st, app.AddRequest{})

	require.Error(t, err)
	list, _, lerr := r.downloads.List(ctx, core.DownloadQuery{})
	require.NoError(t, lerr)
	assert.Empty(t, list, "no half-added Download")
	assert.Empty(t, entries(t, r.torrents))
	assert.Empty(t, r.events())
}

func TestAddTorrentTheQueueTransitionFailingRemovesEverything(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	ds := app.NewDownloadService(failingTransition{r.downloads}, r.cats, r.bus)
	svc := app.NewAddService(ds, nil, torrentSettings{r.usenet, r.torrents}, app.WithTorrentSources(r.sources))
	data, _ := coretest.BuildTorrent("x", showFiles)
	st, err := svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	_, err = svc.AddTorrent(ctx, st, app.AddRequest{})

	require.Error(t, err)
	list, _, lerr := r.downloads.List(ctx, core.DownloadQuery{})
	require.NoError(t, lerr)
	assert.Empty(t, list)
	var n int
	require.NoError(t, r.db.Reader().QueryRow(`SELECT count(*) FROM torrent_sources`).Scan(&n))
	assert.Zero(t, n, "the source went with the Download")
	assert.Empty(t, entries(t, r.torrents))
	assert.Empty(t, r.events())
}

func TestAddTorrentTheDownloadCannotBeStoredRemovesTheFiles(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	ds := app.NewDownloadService(failingDownloads{DownloadRepo: r.downloads, err: errors.New("db gone")}, r.cats, r.bus)
	svc := app.NewAddService(ds, nil, torrentSettings{r.usenet, r.torrents}, app.WithTorrentSources(r.sources))
	data, _ := coretest.BuildTorrent("x", showFiles)
	st, err := svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	_, err = svc.AddTorrent(ctx, st, app.AddRequest{})

	require.Error(t, err)
	assert.Empty(t, entries(t, r.torrents))
}

// hookedTorrentSources runs before on every Create, then stores for real.
type hookedTorrentSources struct {
	core.TorrentSourceRepo
	before func(core.TorrentSource)
}

func (h hookedTorrentSources) Create(ctx context.Context, s core.TorrentSource) error {
	h.before(s)
	return h.TorrentSourceRepo.Create(ctx, s)
}

func TestAddTorrentNeverShowsAQueuedDownloadBeforeItsSourceIsStored(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	var seen core.State
	hooked := hookedTorrentSources{TorrentSourceRepo: r.sources, before: func(s core.TorrentSource) {
		got, err := r.downloads.Get(ctx, s.DownloadID)
		require.NoError(t, err, "the Download row exists before the source (foreign key)")
		seen = got.State
	}}
	svc := app.NewAddService(app.NewDownloadService(r.downloads, r.cats, r.bus), nil, torrentSettings{r.usenet, r.torrents}, app.WithTorrentSources(hooked))
	st, err := svc.StageMagnet(ctx, magnetURI("k"))
	require.NoError(t, err)

	_, err = svc.AddTorrent(ctx, st, app.AddRequest{})

	require.NoError(t, err)
	assert.Equal(t, core.StateAdded, seen)
}

func TestAddTorrentNeverNamesTheMetainfoLikeTheTorrentsOwnData(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	// The request name makes the metainfo "evil.bin.torrent", which is also
	// the torrent's own top-level file.
	data, _ := coretest.BuildTorrent("-", []coretest.TorrentEntry{{Path: []string{"evil.bin.torrent"}, Size: 1}, {Path: []string{"x"}, Size: 1}})
	st, err := r.svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	d, err := r.svc.AddTorrent(ctx, st, app.AddRequest{Name: "evil.bin"})

	require.NoError(t, err)
	src, err := r.sources.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, ".bitnews-metainfo.torrent", filepath.Base(src.TorrentPath))
	assert.FileExists(t, src.TorrentPath)
}

func TestAddTorrentCleansHostileDisplayNames(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	data, _ := coretest.BuildTorrent("evil\u202enam\x00e\n", showFiles)
	st, err := r.svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	d, err := r.svc.AddTorrent(ctx, st, app.AddRequest{})

	require.NoError(t, err)
	assert.Equal(t, "evilname", d.Name)
}

// source is a fake indexer.
func source(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchStagesATorrentAnNZBOrAMagnetByWhatTheServerSends(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	data, hash := coretest.BuildTorrent("Fetched.Torrent", showFiles)
	srv := source(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/torrent":
			_, _ = w.Write(data)
		case "/nzb":
			_, _ = io.WriteString(w, tinyNZB)
		case "/magnet":
			http.Redirect(w, req, magnetURI("PASSKEY"), http.StatusFound)
		case "/hop":
			http.Redirect(w, req, "/magnet", http.StatusFound)
		default:
			http.NotFound(w, req)
		}
	})

	tor, err := r.svc.Fetch(ctx, srv.URL+"/torrent")
	require.NoError(t, err)
	require.NotNil(t, tor.Torrent)
	assert.Nil(t, tor.NZB)
	assert.Equal(t, hash, tor.Torrent.InfoHash())
	assert.False(t, tor.Torrent.IsMagnet())
	tor.Discard()

	nzb, err := r.svc.Fetch(ctx, srv.URL+"/nzb")
	require.NoError(t, err)
	require.NotNil(t, nzb.NZB)
	assert.Nil(t, nzb.Torrent)
	nzb.Discard()

	for _, path := range []string{"/magnet", "/hop"} {
		mag, err := r.svc.Fetch(ctx, srv.URL+path)
		require.NoError(t, err, path)
		require.NotNil(t, mag.Torrent, path)
		assert.True(t, mag.Torrent.IsMagnet())
		assert.Equal(t, hexHash, mag.Torrent.InfoHash().String())
		mag.Discard()
	}
	assert.Empty(t, entries(t, r.torrents))
	assert.Empty(t, entries(t, r.usenet))
}

func TestFetchRefusesWhatFetchNZBRefuses(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	big := bytes.Repeat([]byte("d"), core.MaxMetainfoSize+10)
	srv := source(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/404":
			http.NotFound(w, req)
		case "/file":
			http.Redirect(w, req, "file:///etc/passwd", http.StatusFound)
		case "/ftp":
			http.Redirect(w, req, "ftp://example.com/a.torrent", http.StatusFound)
		case "/big":
			_, _ = w.Write(big)
		case "/bad-magnet":
			http.Redirect(w, req, "magnet:?xt=urn:btih:nope", http.StatusFound)
		case "/html":
			_, _ = io.WriteString(w, "<html>log in</html>")
		}
	})

	for path, want := range map[string]string{
		"/404": "fetch_failed", "/file": "fetch_failed", "/ftp": "fetch_failed",
		"/big": "torrent_too_large", "/bad-magnet": "invalid_magnet", "/html": "invalid_nzb",
	} {
		_, err := r.svc.Fetch(ctx, srv.URL+path)

		var appErr *app.Error
		require.ErrorAs(t, err, &appErr, path)
		assert.Equal(t, want, appErr.Code, path)
	}
	_, err := r.svc.Fetch(ctx, "ftp://example.com/x")
	var appErr *app.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "invalid_url", appErr.Code)
	assert.Empty(t, entries(t, r.torrents))
	assert.Empty(t, entries(t, r.usenet))
}

func TestFetchDoesNotFollowAMagnetRedirectAsAnHTTPRequest(t *testing.T) {
	r := newTorrentRig(t)
	var hits atomic.Int32
	srv := source(t, func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		http.Redirect(w, req, magnetURI("k"), http.StatusSeeOther)
	})

	got, err := r.svc.Fetch(context.Background(), srv.URL)

	require.NoError(t, err)
	got.Discard()
	assert.EqualValues(t, 1, hits.Load())
}

func TestFetchRedactsTheURLInLogsAndErrors(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	r := newTorrentRig(t, app.WithLogger(log))
	srv := source(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/bad" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		http.Redirect(w, req, magnetURI("MAGNETKEY"), http.StatusFound)
	})

	ok, err := r.svc.Fetch(context.Background(), srv.URL+"/get?apikey=URLSECRET")
	require.NoError(t, err)
	ok.Discard()
	_, err = r.svc.Fetch(context.Background(), srv.URL+"/bad?apikey=URLSECRET")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "URLSECRET")
	assert.NotContains(t, logs.String(), "URLSECRET")
	assert.NotContains(t, logs.String(), "MAGNETKEY", "the magnet link is never logged")
	assert.Contains(t, logs.String(), "fetch")
}

func TestFetchOfATorrentNeedsTorrentSourcesButAnNZBDoesNot(t *testing.T) {
	s := newStack(t)
	data, _ := coretest.BuildTorrent("x", showFiles)
	srv := source(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/nzb" {
			_, _ = io.WriteString(w, tinyNZB)
			return
		}
		_, _ = w.Write(data)
	})
	svc := app.NewAddService(app.NewDownloadService(s.downloads, s.cats, s.bus), failingSources{}, torrentSettings{t.TempDir(), t.TempDir()})

	got, err := svc.Fetch(context.Background(), srv.URL+"/nzb")
	require.NoError(t, err)
	got.Discard()
	_, err = svc.Fetch(context.Background(), srv.URL+"/torrent")

	assert.ErrorIs(t, err, app.ErrUnavailable)
}
