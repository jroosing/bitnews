package torrent_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/torrent"
)

// slowLeech starts a leecher for c that downloads at about rate bytes/s and
// is connected to seeder.
func slowLeech(t *testing.T, c content, seeder *torrent.Engine, rate int64) (*torrent.Engine, string) {
	t.Helper()
	leech := newEngine(t)
	require.NoError(t, leech.SetRateLimits(rate, 0))
	dir := t.TempDir()
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	return leech, dir
}

// keepAddingPeer re-announces addr to torrent h every 200 ms until stopped,
// like a tracker would.
func keepAddingPeer(e *torrent.Engine, h core.InfoHash, addr string) (stop func()) {
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				_ = e.AddPeer(h, addr)
			}
		}
	}()
	return func() { close(done) }
}

func waitStats(t *testing.T, e *torrent.Engine, h core.InfoHash, what string, ok func(core.TorrentStats) bool) core.TorrentStats {
	t.Helper()
	var st core.TorrentStats
	require.Eventually(t, func() bool {
		var err error
		st, err = e.Stats(h)
		return err == nil && ok(st)
	}, 20*time.Second, 20*time.Millisecond, what)
	return st
}

func noEvent(t *testing.T, e *torrent.Engine, kind core.TorrentEventKind, h core.InfoHash, wait time.Duration) {
	t.Helper()
	timeout := time.After(wait)
	for {
		select {
		case ev := <-e.Events():
			if ev.Kind == kind && ev.Hash == h {
				require.FailNow(t, "unexpected event", "%s of %s", kind, h)
			}
		case <-timeout:
			return
		}
	}
}

func TestRelocateWhileSeedingKeepsSeedingWithoutRehash(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Moving.Release", map[string]int{"a.bin": 200_000, "sub/b.bin": 60_000, "zero": 0})
	seeder := seed(t, c)
	// A slow leecher stays connected to the seeder while it moves.
	slow, slowDir := slowLeech(t, c, seeder, 40_000)
	waitStats(t, seeder, c.hash, "the leecher connects", func(s core.TorrentStats) bool { return s.Peers >= 1 && s.Uploaded > 0 })
	before, _ := seeder.Stats(c.hash)
	newDir := filepath.Join(t.TempDir(), "complete", "Moving.Release")

	require.NoError(t, seeder.Relocate(context.Background(), c.hash, newDir))

	c.assertFilesEqual(t, newDir)
	assert.NoDirExists(t, c.seedDir, "the old directory moved")
	assert.Zero(t, seeder.BytesHashed(c.hash), "piece completion spared a rehash")
	st, err := seeder.Stats(c.hash)
	require.NoError(t, err)
	assert.Equal(t, core.TorrentSeeding, st.State)
	assert.GreaterOrEqual(t, st.Uploaded, before.Uploaded, "counters carry over")
	noEvent(t, seeder, core.TorrentFetched, c.hash, 300*time.Millisecond) // already reported once, before the move

	// Seeding resumed: the leecher finishes from the new place. A tracker
	// would keep telling it where the seeder is; the test does the same.
	stop := keepAddingPeer(slow, c.hash, seeder.ListenAddr())
	defer stop()
	waitEvent(t, slow, core.TorrentFetched, c.hash)
	c.assertFilesEqual(t, slowDir)
}

func TestRelocateFailureLeavesTheTorrentSeedingWhereItWas(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Stays.Put", map[string]int{"a.bin": 50_000})
	seeder := seed(t, c)
	existing := t.TempDir() // MoveDir refuses an existing destination

	err := seeder.Relocate(context.Background(), c.hash, existing)

	require.Error(t, err)
	c.assertFilesEqual(t, c.seedDir)
	st, _ := seeder.Stats(c.hash)
	assert.Equal(t, core.TorrentSeeding, st.State)
	assert.Zero(t, seeder.BytesHashed(c.hash), "rollback does not rehash either")
	leech := newEngine(t)
	dir := t.TempDir()
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)
	c.assertFilesEqual(t, dir)
}

func TestRelocateRefusesBadTargets(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Bad.Targets", map[string]int{"a.bin": 5_000})
	seeder := seed(t, c)
	ctx := context.Background()

	assert.Error(t, seeder.Relocate(ctx, c.hash, "relative/dir"))
	assert.Error(t, seeder.Relocate(ctx, c.hash, filepath.Join(c.seedDir, "inside")), "inside the source")
	assert.NoError(t, seeder.Relocate(ctx, c.hash, c.seedDir), "same place is a no-op")
	c.assertFilesEqual(t, c.seedDir)
	st, _ := seeder.Stats(c.hash)
	assert.Equal(t, core.TorrentSeeding, st.State)
}

func TestRelocatePreservesAPausedState(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Paused.Move", map[string]int{"a.bin": 5_000})
	seeder := seed(t, c)
	require.NoError(t, seeder.Pause(c.hash))

	require.NoError(t, seeder.Relocate(context.Background(), c.hash, filepath.Join(t.TempDir(), "x")))

	st, _ := seeder.Stats(c.hash)
	assert.Equal(t, core.TorrentPaused, st.State)
}

func TestPausedTorrentDoesNotDownloadUntilResumed(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Pause.Me", map[string]int{"a.bin": 80_000})
	seeder := seed(t, c)
	leech := newEngine(t)
	dir := t.TempDir()
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir, Paused: true})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))

	time.Sleep(700 * time.Millisecond)
	st, _ := leech.Stats(h)
	assert.Equal(t, core.TorrentPaused, st.State)
	assert.Zero(t, st.DoneBytes)
	assert.Zero(t, st.Peers)

	require.NoError(t, leech.Resume(h))
	waitEvent(t, leech, core.TorrentFetched, h)
	c.assertFilesEqual(t, dir)
	st, _ = leech.Stats(h)
	assert.Equal(t, core.TorrentSeeding, st.State)
}

func TestPausedSeederStopsUploadingUntilResumed(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Pause.Seeder", map[string]int{"a.bin": 80_000})
	seeder := seed(t, c)
	require.NoError(t, seeder.Pause(c.hash))
	st, _ := seeder.Stats(c.hash)
	assert.Equal(t, core.TorrentPaused, st.State)

	leech := newEngine(t)
	dir := t.TempDir()
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	time.Sleep(700 * time.Millisecond)
	ls, _ := leech.Stats(h)
	assert.Zero(t, ls.DoneBytes, "nothing comes from a paused seeder")

	require.NoError(t, seeder.Resume(c.hash))
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)
	st, _ = seeder.Stats(c.hash)
	assert.Equal(t, core.TorrentSeeding, st.State)
}

func TestRemoveKeepsFilesUnlessAsked(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Keep.Files", map[string]int{"a.bin": 5_000})
	e := seed(t, c)

	require.NoError(t, e.Remove(context.Background(), c.hash, false))

	c.assertFilesEqual(t, c.seedDir)
	_, err := e.Stats(c.hash)
	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.ErrorIs(t, e.Remove(context.Background(), c.hash, false), core.ErrNotFound)
}

func TestRemoveWithDataDeletesOnlyTheTorrentsOwnFiles(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Delete.Me", map[string]int{"a.bin": 30_000, "sub/deep/b.bin": 5_000, "zero": 0})
	e := seed(t, c)
	// Things the torrent does not own, next to and inside its directories.
	foreignTop := filepath.Join(c.seedDir, "foreign.txt")
	foreignInside := filepath.Join(c.seedDir, c.name, "sub", "keep.nfo")
	require.NoError(t, os.WriteFile(foreignTop, []byte("mine"), 0o644))
	require.NoError(t, os.WriteFile(foreignInside, []byte("mine"), 0o644))

	require.NoError(t, e.Remove(context.Background(), c.hash, true))

	assert.NoFileExists(t, filepath.Join(c.seedDir, c.name, "a.bin"))
	assert.NoFileExists(t, filepath.Join(c.seedDir, c.name, "sub", "deep", "b.bin"))
	assert.NoFileExists(t, filepath.Join(c.seedDir, c.name, "zero"))
	assert.NoDirExists(t, filepath.Join(c.seedDir, c.name, "sub", "deep"), "emptied directories go too")
	assert.FileExists(t, foreignTop, "never anything the torrent does not list")
	assert.FileExists(t, foreignInside)
	assert.DirExists(t, c.seedDir, "the storage dir itself stays")
}

func TestRemoveForgetsPieceCompletion(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Forget.Me", map[string]int{"a.bin": 40_000})
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	repo := store.NewTorrentPieceRepo(db)
	e := newEngine(t, func(cfg *torrent.Config) { cfg.PieceStore = repo; cfg.FlushInterval = 20 * time.Millisecond })
	_, err := e.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: c.seedDir})
	require.NoError(t, err)
	waitEvent(t, e, core.TorrentFetched, c.hash)
	require.Eventually(t, func() bool { _, err := repo.Load(context.Background(), c.hash); return err == nil }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, e.Remove(context.Background(), c.hash, false))

	_, err = repo.Load(context.Background(), c.hash)
	assert.ErrorIs(t, err, core.ErrNotFound)
	time.Sleep(100 * time.Millisecond) // a flush after removal must not bring the row back
	_, err = repo.Load(context.Background(), c.hash)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestRestartedEngineDoesNotRehashACompleteTorrent(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Restart", map[string]int{"a.bin": 150_000, "b.bin": 33_000})
	seeder := seed(t, c)
	dbPath := filepath.Join(t.TempDir(), "db")
	dir := t.TempDir()
	run := func(path string) (*torrent.Engine, core.InfoHash) {
		db := openDB(t, path)
		e := newEngine(t, func(cfg *torrent.Config) { cfg.PieceStore = store.NewTorrentPieceRepo(db) })
		h, err := e.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
		require.NoError(t, err)
		return e, h
	}

	first, h := run(dbPath)
	require.NoError(t, first.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, first, core.TorrentFetched, h)
	assert.Zero(t, first.BytesHashed(h)-int64(183_000), "the download hashed each byte once") // sanity of the counter
	require.NoError(t, first.Close())                                                         // flushes piece completion

	second, h := run(dbPath)
	waitEvent(t, second, core.TorrentFetched, h)
	assert.Zero(t, second.BytesHashed(h), "after a restart nothing is verified again")
	st, _ := second.Stats(h)
	assert.Equal(t, core.TorrentSeeding, st.State)

	// Control: without the stored completion the same files are hashed.
	third, h := run(filepath.Join(t.TempDir(), "other-db"))
	waitEvent(t, third, core.TorrentFetched, h)
	assert.EqualValues(t, 183_000, third.BytesHashed(h), "the counter does see hashing when completion is lost")
}

func TestEventsNeverBlockTheEngine(t *testing.T) {
	t.Parallel()
	e := newEngine(t, func(cfg *torrent.Config) { cfg.EventBuffer = 1 })
	for i := range 5 { // five Fetched events into a buffer of one that nobody reads
		c := makeContent(t, "Event"+string(rune('A'+i)), map[string]int{"f": 1000 + i})
		_, err := e.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: c.seedDir})
		require.NoError(t, err)
		waitStats(t, e, c.hash, "seeding", func(s core.TorrentStats) bool { return s.State == core.TorrentSeeding })
	}

	done := make(chan error, 1)
	go func() { done <- e.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "Close hung")
	}
	n := 0
	for range e.Events() { // closed after Close
		n++
	}
	assert.Equal(t, 1, n, "the buffer holds one event; the rest were dropped")
}
