package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

func testHash(b byte) core.InfoHash {
	var h core.InfoHash
	for i := range h {
		h[i] = b
	}
	return h
}

func TestTorrentPieceRepoSavesAndLoadsABitmap(t *testing.T) {
	ctx := context.Background()
	repo := store.NewTorrentPieceRepo(openDefault(t))
	want := store.TorrentPieces{PieceCount: 10, Bitmap: []byte{0b10100101, 0b01}}

	require.NoError(t, repo.Save(ctx, testHash(1), want))
	got, err := repo.Load(ctx, testHash(1))

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestTorrentPieceRepoLoadOfUnknownHashIsNotFound(t *testing.T) {
	repo := store.NewTorrentPieceRepo(openDefault(t))

	_, err := repo.Load(context.Background(), testHash(1))

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTorrentPieceRepoSetAndGetSinglePieces(t *testing.T) {
	ctx := context.Background()
	repo := store.NewTorrentPieceRepo(openDefault(t))
	h := testHash(2)

	require.NoError(t, repo.Set(ctx, h, 20, 0, true))
	require.NoError(t, repo.Set(ctx, h, 20, 19, true))
	require.NoError(t, repo.Set(ctx, h, 20, 8, true))
	require.NoError(t, repo.Set(ctx, h, 20, 8, false))

	for i, want := range map[int]bool{0: true, 8: false, 19: true, 5: false} {
		got, err := repo.Get(ctx, h, i)
		require.NoError(t, err)
		assert.Equal(t, want, got, "piece %d", i)
	}
	_, err := repo.Get(ctx, h, 20)
	assert.ErrorIs(t, err, core.ErrNotFound)
	_, err = repo.Get(ctx, testHash(9), 0)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTorrentPieceRepoSetRejectsAPieceCountChange(t *testing.T) {
	ctx := context.Background()
	repo := store.NewTorrentPieceRepo(openDefault(t))
	require.NoError(t, repo.Set(ctx, testHash(2), 20, 0, true))

	err := repo.Set(ctx, testHash(2), 21, 0, true)

	assert.ErrorIs(t, err, store.ErrInvalidPieces)
	err = repo.Set(ctx, testHash(2), 20, 20, true)
	assert.ErrorIs(t, err, store.ErrInvalidPieces)
}

func TestTorrentPieceRepoSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t)
	repo := store.NewTorrentPieceRepo(db)
	want := store.TorrentPieces{PieceCount: 3, Bitmap: []byte{0b101}}
	require.NoError(t, repo.Save(ctx, testHash(3), want))
	require.NoError(t, db.Close())

	db2, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db2.Close() })
	got, err := store.NewTorrentPieceRepo(db2).Load(ctx, testHash(3))

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestTorrentPieceRepoSaveOverwrites(t *testing.T) {
	ctx := context.Background()
	repo := store.NewTorrentPieceRepo(openDefault(t))
	require.NoError(t, repo.Save(ctx, testHash(1), store.TorrentPieces{PieceCount: 8, Bitmap: []byte{0x0f}}))
	require.NoError(t, repo.Save(ctx, testHash(1), store.TorrentPieces{PieceCount: 16, Bitmap: []byte{0xff, 0xff}}))

	got, err := repo.Load(ctx, testHash(1))

	require.NoError(t, err)
	assert.Equal(t, store.TorrentPieces{PieceCount: 16, Bitmap: []byte{0xff, 0xff}}, got)
}

func TestTorrentPieceRepoDeleteAndList(t *testing.T) {
	ctx := context.Background()
	repo := store.NewTorrentPieceRepo(openDefault(t))
	for _, b := range []byte{5, 1, 3} {
		require.NoError(t, repo.Save(ctx, testHash(b), store.TorrentPieces{}))
	}

	got, err := repo.ListInfoHashes(ctx)
	require.NoError(t, err)
	assert.Equal(t, []core.InfoHash{testHash(1), testHash(3), testHash(5)}, got)

	require.NoError(t, repo.Delete(ctx, testHash(3)))
	require.NoError(t, repo.Delete(ctx, testHash(3)), "deleting twice is fine")
	got, err = repo.ListInfoHashes(ctx)
	require.NoError(t, err)
	assert.Equal(t, []core.InfoHash{testHash(1), testHash(5)}, got)
}

func TestTorrentPieceRepoSaveRejectsInconsistentBitmaps(t *testing.T) {
	ctx := context.Background()
	repo := store.NewTorrentPieceRepo(openDefault(t))

	for name, p := range map[string]store.TorrentPieces{
		"bitmap too short":   {PieceCount: 9, Bitmap: []byte{0}},
		"bitmap too long":    {PieceCount: 8, Bitmap: []byte{0, 0}},
		"negative count":     {PieceCount: -1},
		"over the piece cap": {PieceCount: store.MaxTorrentPieces + 1, Bitmap: make([]byte, store.MaxTorrentPieces/8+1)},
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, repo.Save(ctx, testHash(1), p), store.ErrInvalidPieces)
		})
	}
	require.NoError(t, repo.Save(ctx, testHash(1), store.TorrentPieces{PieceCount: store.MaxTorrentPieces, Bitmap: make([]byte, store.MaxTorrentPieces/8)}))
}

func TestTorrentPiecesTableRejectsCorruptRowsItself(t *testing.T) {
	db := openDefault(t)
	h := testHash(1).String()

	for name, stmt := range map[string]string{
		"bitmap size mismatch": `INSERT INTO torrent_pieces VALUES ('` + h + `', 100, x'00', 0)`,
		"piece count over cap": `INSERT INTO torrent_pieces VALUES ('` + h + `', 5000000, zeroblob(625000), 0)`,
		"short hash":           `INSERT INTO torrent_pieces VALUES ('abcd', 0, x'', 0)`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := db.Writer().Exec(stmt)
			assert.Error(t, err)
		})
	}
}

func TestTorrentPieceRepoLoadReportsACorruptStoredRow(t *testing.T) {
	// The CHECK constraints forbid a bad row, so simulate one written by a
	// future version by dropping them through a rebuilt table.
	ctx := context.Background()
	db := openDefault(t)
	exec(t, db, `DROP TABLE torrent_pieces`)
	exec(t, db, `CREATE TABLE torrent_pieces (info_hash TEXT PRIMARY KEY, piece_count INTEGER NOT NULL, bitmap BLOB NOT NULL, updated_at INTEGER NOT NULL) STRICT`)
	exec(t, db, `INSERT INTO torrent_pieces VALUES ('`+testHash(1).String()+`', 100, x'00', 0)`)

	_, err := store.NewTorrentPieceRepo(db).Load(ctx, testHash(1))

	assert.ErrorIs(t, err, store.ErrInvalidPieces)
}

func TestMigrationFiveCreatesTorrentPieces(t *testing.T) {
	db := openDefault(t)

	got := applied(t, db.Reader())

	require.GreaterOrEqual(t, len(got), 5)
	assert.Equal(t, appliedRow{5, "torrent_pieces"}, got[4])
	assert.Equal(t, 0, count(t, db.Reader(), "torrent_pieces"))
}
