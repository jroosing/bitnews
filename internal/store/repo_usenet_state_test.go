package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

// The progress repo matches runner.ProgressStore and the undeferred repo
// matches runner.VolumeStore structurally (store must not import the
// runner); the daemon package asserts both. The report repo uses a store
// type, which the daemon adapts to runner.FetchDetail.

func TestUsenetProgressSaveThenLoadRoundTrips(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewUsenetProgressRepo(db)
	id := aDownload(t, db)

	require.NoError(t, repo.Save(ctx, id, []byte("GNRP\x01blob")))
	got, err := repo.Load(ctx, id)

	require.NoError(t, err)
	assert.Equal(t, []byte("GNRP\x01blob"), got)
}

func TestUsenetProgressLoadOfAMissingRowIsNotFound(t *testing.T) {
	db, _ := openTemp(t)
	repo := store.NewUsenetProgressRepo(db)

	_, err := repo.Load(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestUsenetProgressSaveReplacesTheBlob(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewUsenetProgressRepo(db)
	id := aDownload(t, db)
	require.NoError(t, repo.Save(ctx, id, []byte("one")))

	require.NoError(t, repo.Save(ctx, id, []byte("two")))
	got, err := repo.Load(ctx, id)

	require.NoError(t, err)
	assert.Equal(t, []byte("two"), got)
	assert.Equal(t, 1, count(t, db.Reader(), "usenet_progress"))
}

func TestUsenetProgressDeleteForgetsTheBlobAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewUsenetProgressRepo(db)
	id := aDownload(t, db)
	require.NoError(t, repo.Save(ctx, id, []byte("x")))

	require.NoError(t, repo.Delete(ctx, id))
	require.NoError(t, repo.Delete(ctx, id))
	require.NoError(t, repo.Delete(ctx, core.NewID()))

	_, err := repo.Load(ctx, id)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestUsenetProgressRefusesBlobsAboveTheCap(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewUsenetProgressRepo(db)
	id := aDownload(t, db)

	require.NoError(t, repo.Save(ctx, id, bytes.Repeat([]byte{1}, store.MaxUsenetProgressBlob)))
	err := repo.Save(ctx, id, bytes.Repeat([]byte{1}, store.MaxUsenetProgressBlob+1))

	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrBlobTooLarge)
	got, lerr := repo.Load(ctx, id)
	require.NoError(t, lerr)
	assert.Len(t, got, store.MaxUsenetProgressBlob, "the previous blob stays")
}

func TestUsenetProgressSaveForAnUnknownDownloadFails(t *testing.T) {
	db, _ := openTemp(t)
	repo := store.NewUsenetProgressRepo(db)

	err := repo.Save(context.Background(), core.NewID(), []byte("x"))

	require.Error(t, err)
}

func TestUsenetProgressGoesWithTheDownload(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewUsenetProgressRepo(db)
	id := aDownload(t, db)
	require.NoError(t, repo.Save(ctx, id, []byte("x")))

	require.NoError(t, store.NewDownloadRepo(db).Delete(ctx, id))

	assert.Equal(t, 0, count(t, db.Reader(), "usenet_progress"))
}

func TestUsenetReportsRoundTripReplaceAndCascade(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewUsenetReportRepo(db)
	id := aDownload(t, db)

	_, err := repo.LoadReport(ctx, id)
	require.ErrorIs(t, err, core.ErrNotFound)

	want := store.UsenetReport{MissingArticles: 3, MissingBytes: 3_000_000, DamagedArticles: 2, DamagedBytes: 1_234, MissingFiles: 1}
	require.NoError(t, repo.SaveReport(ctx, id, want))
	got, err := repo.LoadReport(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	require.NoError(t, repo.SaveReport(ctx, id, store.UsenetReport{}))
	got, err = repo.LoadReport(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.UsenetReport{}, got)
	assert.Equal(t, 1, count(t, db.Reader(), "usenet_reports"))

	require.NoError(t, store.NewDownloadRepo(db).Delete(ctx, id))
	assert.Equal(t, 0, count(t, db.Reader(), "usenet_reports"))
}

func TestUndeferredWantThenMarkFetched(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewUsenetUndeferredRepo(db)
	id := aDownload(t, db)

	got, err := repo.Undeferred(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got)

	require.NoError(t, repo.Want(ctx, id, []int{4, 2}))
	require.NoError(t, repo.Want(ctx, id, []int{2, 7}), "wanting again is harmless")
	got, err = repo.Undeferred(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, map[int]bool{2: false, 4: false, 7: false}, got)

	require.NoError(t, repo.MarkFetched(ctx, id, []int{2, 4}))
	got, err = repo.Undeferred(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, map[int]bool{2: true, 4: true, 7: false}, got)

	require.NoError(t, repo.Want(ctx, id, []int{2}))
	got, _ = repo.Undeferred(ctx, id)
	assert.True(t, got[2], "wanting a fetched volume does not make it pending again")
}

func TestUndeferredGoesWithTheDownloadAndRejectsStrangers(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	repo := store.NewUsenetUndeferredRepo(db)
	id := aDownload(t, db)
	require.NoError(t, repo.Want(ctx, id, []int{1}))

	require.Error(t, repo.Want(ctx, core.NewID(), []int{1}))
	require.NoError(t, store.NewDownloadRepo(db).Delete(ctx, id))

	assert.Equal(t, 0, count(t, db.Reader(), "usenet_undeferred"))
}

func TestMigration0003CreatesTheRunnerTables(t *testing.T) {
	db, _ := openTemp(t)
	for _, table := range []string{"usenet_progress", "usenet_reports", "usenet_undeferred"} {
		assert.Equal(t, 0, count(t, db.Reader(), table), table)
	}
	var name string
	require.NoError(t, db.Reader().QueryRow(`SELECT name FROM schema_migrations WHERE version = 3`).Scan(&name))
	assert.Equal(t, "runner_state", name)
}
