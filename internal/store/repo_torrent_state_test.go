package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

var _ core.TorrentRecordRepo = (*store.TorrentStateRepo)(nil)

//go:fix inline
func ptr[T any](v T) *T { return new(v) }

func TestTorrentStateUpsertThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewTorrentStateRepo(db)
	id := aDownload(t, db)
	want := core.TorrentRecord{
		DownloadID: id, Uploaded: 123456789, SeedingTime: 90 * time.Minute,
		RatioLimit: new(2.5), SeedingTimeLimit: ptr(48 * time.Hour),
		Moved: true, Location: "/data/torrents/tv/Show",
	}

	require.NoError(t, repo.Upsert(ctx, want))
	got, err := repo.Get(ctx, id)

	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), got.UpdatedAt, time.Minute)
	got.UpdatedAt = time.Time{}
	assert.Equal(t, want, got)
}

func TestTorrentStateNullLimitsMeanDefaultAndNegativeMeansUnlimited(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewTorrentStateRepo(db)
	def, unl := aDownload(t, db), aDownload(t, db)

	require.NoError(t, repo.Upsert(ctx, core.TorrentRecord{DownloadID: def}))
	require.NoError(t, repo.Upsert(ctx, core.TorrentRecord{DownloadID: unl, RatioLimit: new(-1.0), SeedingTimeLimit: ptr(-time.Second)}))
	gotDef, err := repo.Get(ctx, def)
	require.NoError(t, err)
	gotUnl, err := repo.Get(ctx, unl)
	require.NoError(t, err)

	assert.Nil(t, gotDef.RatioLimit)
	assert.Nil(t, gotDef.SeedingTimeLimit)
	assert.False(t, gotDef.Moved)
	assert.Empty(t, gotDef.Location)
	assert.Equal(t, new(-1.0), gotUnl.RatioLimit)
	assert.Equal(t, ptr(-time.Second), gotUnl.SeedingTimeLimit)
}

func TestTorrentStateUpsertReplacesTheRecord(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewTorrentStateRepo(db)
	id := aDownload(t, db)
	require.NoError(t, repo.Upsert(ctx, core.TorrentRecord{DownloadID: id, Uploaded: 1, RatioLimit: new(1.0), Location: "/a"}))

	require.NoError(t, repo.Upsert(ctx, core.TorrentRecord{DownloadID: id, Uploaded: 2}))
	got, err := repo.Get(ctx, id)

	require.NoError(t, err)
	assert.EqualValues(t, 2, got.Uploaded)
	assert.Nil(t, got.RatioLimit)
	assert.Empty(t, got.Location)
	assert.Equal(t, 1, count(t, db.Reader(), "torrent_state"))
}

func TestTorrentStateGetOfAMissingRowIsNotFound(t *testing.T) {
	db, _ := openTemp(t)

	_, err := store.NewTorrentStateRepo(db).Get(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTorrentStateUpsertOfAnUnknownDownloadFails(t *testing.T) {
	db, _ := openTemp(t)

	err := store.NewTorrentStateRepo(db).Upsert(context.Background(), core.TorrentRecord{DownloadID: core.NewID()})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTorrentStateRejectsNegativeCounters(t *testing.T) {
	db, _ := openTemp(t)
	id := aDownload(t, db)

	err := store.NewTorrentStateRepo(db).Upsert(context.Background(), core.TorrentRecord{DownloadID: id, Uploaded: -1})

	assert.Error(t, err)
}

func TestTorrentStateDeleteForgetsTheRecordAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewTorrentStateRepo(db)
	id := aDownload(t, db)
	require.NoError(t, repo.Upsert(ctx, core.TorrentRecord{DownloadID: id}))

	require.NoError(t, repo.Delete(ctx, id))
	require.NoError(t, repo.Delete(ctx, id))
	require.NoError(t, repo.Delete(ctx, core.NewID()))

	_, err := repo.Get(ctx, id)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTorrentStateGoesAwayWithItsDownload(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewTorrentStateRepo(db)
	id := aDownload(t, db)
	require.NoError(t, repo.Upsert(ctx, core.TorrentRecord{DownloadID: id, Uploaded: 5}))

	require.NoError(t, store.NewDownloadRepo(db).Delete(ctx, id))

	assert.Equal(t, 0, count(t, db.Reader(), "torrent_state"))
}

func TestMigration0007CreatesTorrentStateAndKeepsTheVersionsContiguous(t *testing.T) {
	db, _ := openTemp(t)
	var name string
	require.NoError(t, db.Reader().QueryRow(`SELECT name FROM schema_migrations WHERE version = 7`).Scan(&name))
	assert.Equal(t, "torrent_state", name)
	assert.Equal(t, 0, count(t, db.Reader(), "torrent_state"))

	rows, err := db.Reader().Query(`SELECT version FROM schema_migrations ORDER BY version`)
	require.NoError(t, err)
	defer rows.Close()
	want := 1
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		assert.Equal(t, want, v)
		want++
	}
	require.NoError(t, rows.Err())
}
