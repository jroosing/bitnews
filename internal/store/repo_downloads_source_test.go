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

var _ core.DownloadSourceRepo = (*store.DownloadSourceRepo)(nil)

func newSourceRepo(t *testing.T) (*store.DownloadSourceRepo, *store.DB, store.Key) {
	t.Helper()
	db, _ := openTemp(t)
	key := store.GenerateKey()
	return store.NewDownloadSourceRepo(db, key), db, key
}

func aDownload(t *testing.T, db *store.DB) core.ID {
	t.Helper()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	d := core.Download{
		ID: core.NewID(), Name: "x", SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at,
	}
	require.NoError(t, store.NewDownloadRepo(db).Create(context.Background(), d))
	return d.ID
}

func TestDownloadSourceRepoCreateThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newSourceRepo(t)
	id := aDownload(t, db)

	require.NoError(t, repo.Create(ctx, id, "/data/incomplete/"+string(id)+"/x.nzb", "pw!"))
	path, pw, err := repo.Get(ctx, id)

	require.NoError(t, err)
	assert.Equal(t, "/data/incomplete/"+string(id)+"/x.nzb", path)
	assert.Equal(t, "pw!", pw.Reveal())
}

func TestDownloadSourceRepoStoresThePasswordEncryptedWithTheRowAAD(t *testing.T) {
	ctx := context.Background()
	repo, db, key := newSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, id, "/p.nzb", "archive-pass-123"))

	var raw []byte
	require.NoError(t, db.Reader().QueryRow(`SELECT password FROM download_sources WHERE download_id = ?`, string(id)).Scan(&raw))

	assert.NotContains(t, string(raw), "archive-pass-123")
	plain, err := store.Unseal(key, string(raw), store.AAD("download_sources", "password", string(id)))
	require.NoError(t, err)
	assert.Equal(t, "archive-pass-123", plain)
}

func TestDownloadSourceRepoWithoutPasswordStoresNull(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, id, "/p.nzb", ""))

	var isNull bool
	require.NoError(t, db.Reader().QueryRow(`SELECT password IS NULL FROM download_sources WHERE download_id = ?`, string(id)).Scan(&isNull))
	_, pw, err := repo.Get(ctx, id)

	require.NoError(t, err)
	assert.True(t, isNull)
	assert.Empty(t, pw)
}

func TestDownloadSourceRepoGetUnknownIsNotFound(t *testing.T) {
	repo, _, _ := newSourceRepo(t)

	_, _, err := repo.Get(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestDownloadSourceRepoCreateNeedsAnExistingDownloadAndIsUnique(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newSourceRepo(t)
	id := aDownload(t, db)

	assert.Error(t, repo.Create(ctx, core.NewID(), "/p.nzb", ""))
	require.NoError(t, repo.Create(ctx, id, "/p.nzb", ""))
	assert.ErrorIs(t, repo.Create(ctx, id, "/q.nzb", ""), core.ErrConflict)
}

func TestDownloadSourceRepoWrongKeyIsSecretUnavailable(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, id, "/p.nzb", "pw"))

	path, pw, err := store.NewDownloadSourceRepo(db, store.GenerateKey()).Get(ctx, id)

	assert.ErrorIs(t, err, core.ErrSecretUnavailable)
	assert.Equal(t, "/p.nzb", path, "the path is still known")
	assert.Empty(t, pw)
}

func TestDownloadSourceIsDeletedWithItsDownload(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, id, "/p.nzb", "pw"))

	require.NoError(t, store.NewDownloadRepo(db).Delete(ctx, id))

	_, _, err := repo.Get(ctx, id)
	assert.ErrorIs(t, err, core.ErrNotFound)
}
