package torrent_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

// A long interval keeps the background flusher out of the way; tests call
// Flush themselves.
const manualFlush = time.Hour

func openDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newCompletion(t *testing.T, db *store.DB, interval time.Duration) *torrent.SQLitePieceCompletion {
	t.Helper()
	pc := torrent.NewSQLitePieceCompletion(store.NewTorrentPieceRepo(db), torrent.WithFlushInterval(interval))
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

func key(b byte, index int) metainfo.PieceKey {
	var h metainfo.Hash
	for i := range h {
		h[i] = b
	}
	return metainfo.PieceKey{InfoHash: h, Index: index}
}

var _ storage.PieceCompletion = (*torrent.SQLitePieceCompletion)(nil)

func TestPieceCompletionUnknownPieceIsNotOk(t *testing.T) {
	pc := newCompletion(t, openDB(t, filepath.Join(t.TempDir(), "db")), manualFlush)

	c, err := pc.Get(key(1, 0))

	require.NoError(t, err)
	assert.False(t, c.Ok, "an unknown piece must be hashed, not assumed missing")
	assert.False(t, c.Complete)
}

func TestPieceCompletionSetThenGet(t *testing.T) {
	pc := newCompletion(t, openDB(t, filepath.Join(t.TempDir(), "db")), manualFlush)

	require.NoError(t, pc.Set(key(1, 5), true))
	require.NoError(t, pc.Set(key(1, 9), true))
	require.NoError(t, pc.Set(key(1, 9), false))

	for index, want := range map[int]bool{5: true, 9: false, 4: false, 100: false} {
		c, err := pc.Get(key(1, index))
		require.NoError(t, err)
		assert.Equal(t, want, c.Complete, "piece %d", index)
	}
	c9, _ := pc.Get(key(1, 9))
	assert.True(t, c9.Ok, "a piece marked not complete is known, or anacrolix ignores it and stalls")
	c4, _ := pc.Get(key(1, 4))
	assert.False(t, c4.Ok, "a piece never marked is unknown and gets hashed")
	c, _ := pc.Get(key(2, 5))
	assert.False(t, c.Ok, "another torrent is unaffected")
}

func TestPieceCompletionPersistsAcrossReopenAfterFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := openDB(t, path)
	pc := newCompletion(t, db, manualFlush)
	require.NoError(t, pc.Set(key(1, 0), true))
	require.NoError(t, pc.Set(key(1, 15), true))
	require.NoError(t, pc.Flush())
	require.NoError(t, pc.Close())
	require.NoError(t, db.Close())

	pc2 := newCompletion(t, openDB(t, path), manualFlush)

	for index, want := range map[int]bool{0: true, 15: true, 7: false} {
		c, err := pc2.Get(key(1, index))
		require.NoError(t, err)
		assert.Equal(t, want, c.Complete, "piece %d", index)
		assert.Equal(t, want, c.Ok, "piece %d: after a restart only verified pieces are known", index)
	}
}

func TestPieceCompletionCloseFlushesPendingMarks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := openDB(t, path)
	pc := torrent.NewSQLitePieceCompletion(store.NewTorrentPieceRepo(db), torrent.WithFlushInterval(manualFlush))
	require.NoError(t, pc.Set(key(3, 2), true))

	require.NoError(t, pc.Close())

	got, err := store.NewTorrentPieceRepo(db).Load(context.Background(), core.InfoHash(key(3, 0).InfoHash))
	require.NoError(t, err)
	assert.Equal(t, store.TorrentPieces{PieceCount: 3, Bitmap: []byte{0b100}}, got)
}

func TestPieceCompletionBackgroundFlushWritesWithinTheInterval(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	pc := newCompletion(t, db, 20*time.Millisecond)
	require.NoError(t, pc.Set(key(4, 1), true))

	repo := store.NewTorrentPieceRepo(db)
	require.Eventually(t, func() bool {
		p, err := repo.Load(context.Background(), core.InfoHash(key(4, 0).InfoHash))
		return err == nil && p.Bitmap[0]&0b10 != 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestPieceCompletionDefaultFlushIntervalIsTwoSeconds(t *testing.T) {
	assert.Equal(t, 2*time.Second, torrent.DefaultFlushInterval)
}

func TestPieceCompletionCrashLosesOnlyUnflushedMarksAndNeverCorrupts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := openDB(t, path)
	pc := torrent.NewSQLitePieceCompletion(store.NewTorrentPieceRepo(db), torrent.WithFlushInterval(manualFlush))
	require.NoError(t, pc.Set(key(5, 0), true))
	require.NoError(t, pc.Set(key(5, 1), true))
	require.NoError(t, pc.Flush())
	// These marks are in memory only when the process "crashes": the
	// completion is dropped without Close.
	require.NoError(t, pc.Set(key(5, 2), true))
	require.NoError(t, pc.Set(key(6, 0), true))
	require.NoError(t, db.Close())

	pc2 := newCompletion(t, openDB(t, path), manualFlush)

	for index, want := range map[int]bool{0: true, 1: true, 2: false} {
		c, err := pc2.Get(key(5, index))
		require.NoError(t, err)
		assert.Equal(t, want, c.Complete, "piece %d", index)
	}
	c, err := pc2.Get(key(6, 0))
	require.NoError(t, err)
	assert.False(t, c.Complete, "a torrent that never flushed has no row")
	// The rows that exist are consistent: a store Load validates them.
	p, err := store.NewTorrentPieceRepo(openDB(t, path)).Load(context.Background(), core.InfoHash(key(5, 0).InfoHash))
	require.NoError(t, err)
	assert.Equal(t, 2, p.PieceCount)
}

func TestPieceCompletionRegisterSetsTheExactPieceCount(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	pc := newCompletion(t, db, manualFlush)
	h := core.InfoHash(key(7, 0).InfoHash)

	require.NoError(t, pc.Register(h, 20))
	require.NoError(t, pc.Set(key(7, 3), true))
	require.NoError(t, pc.Flush())

	p, err := store.NewTorrentPieceRepo(db).Load(context.Background(), h)
	require.NoError(t, err)
	assert.Equal(t, 20, p.PieceCount)
	assert.Equal(t, []byte{0b1000, 0, 0}, p.Bitmap)
}

func TestPieceCompletionRejectsPieceCountsOverTheCap(t *testing.T) {
	pc := newCompletion(t, openDB(t, filepath.Join(t.TempDir(), "db")), manualFlush)

	assert.ErrorIs(t, pc.Register(core.InfoHash{1}, store.MaxTorrentPieces+1), store.ErrInvalidPieces)
	assert.ErrorIs(t, pc.Set(key(1, store.MaxTorrentPieces), true), store.ErrInvalidPieces)
	assert.ErrorIs(t, pc.Set(key(1, -1), true), store.ErrInvalidPieces)
	require.NoError(t, pc.Set(key(1, store.MaxTorrentPieces-1), true))
}

func TestPieceCompletionForgetDeletesTheRowAndTheMemory(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	pc := newCompletion(t, db, manualFlush)
	h := core.InfoHash(key(8, 0).InfoHash)
	require.NoError(t, pc.Set(key(8, 1), true))
	require.NoError(t, pc.Flush())

	require.NoError(t, pc.Forget(context.Background(), h))
	require.NoError(t, pc.Flush())

	_, err := store.NewTorrentPieceRepo(db).Load(context.Background(), h)
	assert.ErrorIs(t, err, core.ErrNotFound)
	c, _ := pc.Get(key(8, 1))
	assert.False(t, c.Complete)
}

func TestPieceCompletionCorruptRowIsTreatedAsUnknown(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	h := core.InfoHash(key(9, 0).InfoHash)
	// Rebuild the table without the CHECKs to plant a row a future version
	// might have written.
	_, err := db.Writer().Exec(`DROP TABLE torrent_pieces`)
	require.NoError(t, err)
	_, err = db.Writer().Exec(`CREATE TABLE torrent_pieces (info_hash TEXT PRIMARY KEY, piece_count INTEGER NOT NULL, bitmap BLOB NOT NULL, updated_at INTEGER NOT NULL) STRICT`)
	require.NoError(t, err)
	_, err = db.Writer().Exec(`INSERT INTO torrent_pieces VALUES (?, 100, x'ff', 0)`, h.String())
	require.NoError(t, err)
	pc := newCompletion(t, db, manualFlush)

	c, err := pc.Get(key(9, 0))

	require.NoError(t, err)
	assert.False(t, c.Ok, "an unreadable row means rehash, never trust")
}

func TestPieceCompletionIsSafeForConcurrentUse(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	pc := newCompletion(t, db, 5*time.Millisecond)
	const workers, pieces = 8, 300

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			h := byte(w%3 + 1) // several workers share a torrent
			for i := range pieces {
				if (i+w)%pieces == 0 {
					continue
				}
				assert.NoError(t, pc.Set(key(h, i), true))
				_, err := pc.Get(key(h, i))
				assert.NoError(t, err)
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			assert.NoError(t, pc.Flush())
		}
	})
	wg.Wait()
	require.NoError(t, pc.Close())

	pc2 := newCompletion(t, db, manualFlush)
	for h := byte(1); h <= 3; h++ {
		complete := 0
		for i := range pieces {
			if c, _ := pc2.Get(key(h, i)); c.Complete {
				complete++
			}
		}
		assert.Greater(t, complete, pieces-10, "torrent %d", h)
	}
}

func TestPieceCompletionCloseTwiceIsFine(t *testing.T) {
	pc := newCompletion(t, openDB(t, filepath.Join(t.TempDir(), "db")), manualFlush)
	require.NoError(t, pc.Close())
	require.NoError(t, pc.Close())
	assert.Error(t, pc.Set(key(1, 0), true), "use after Close is an error, not a silent loss")
}
