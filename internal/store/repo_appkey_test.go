package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

var _ core.AppKeyRepo = (*store.AppKeyRepo)(nil)

const plainAppKey = "bitnews_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqr"

func newAppKeyRepo(t *testing.T) (*store.AppKeyRepo, *store.DB, store.Key) {
	t.Helper()
	db, _ := openTemp(t)
	key := store.GenerateKey()
	return store.NewAppKeyRepo(db, key), db, key
}

func rawAppKey(t *testing.T, db *store.DB) string {
	t.Helper()
	var raw string
	require.NoError(t, db.Reader().QueryRow(`SELECT sealed_key FROM app_key WHERE id = 1`).Scan(&raw))
	return raw
}

func TestAppKeyRepoHasNoKeyOnAFreshDatabase(t *testing.T) {
	repo, _, _ := newAppKeyRepo(t)

	_, err := repo.Get(context.Background())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestAppKeyRepoSetThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newAppKeyRepo(t)

	require.NoError(t, repo.Set(ctx, plainAppKey))
	got, err := repo.Get(ctx)

	require.NoError(t, err)
	assert.Equal(t, plainAppKey, got.Reveal())
}

func TestAppKeyRepoStoresTheKeySealedWithTheRowAAD(t *testing.T) {
	ctx := context.Background()
	repo, db, key := newAppKeyRepo(t)
	require.NoError(t, repo.Set(ctx, plainAppKey))

	raw := rawAppKey(t, db)

	assert.NotContains(t, raw, plainAppKey)
	assert.Contains(t, raw, "v1:")
	plain, err := store.Unseal(key, raw, store.AAD("app_key", "sealed_key", "1"))
	require.NoError(t, err)
	assert.Equal(t, plainAppKey, plain)
}

func TestAppKeyRepoSetReplacesTheKeyAndKeepsASingleRow(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newAppKeyRepo(t)
	require.NoError(t, repo.Set(ctx, plainAppKey))

	require.NoError(t, repo.Set(ctx, "bitnews_other"))
	got, err := repo.Get(ctx)

	require.NoError(t, err)
	assert.Equal(t, "bitnews_other", got.Reveal())
	assert.Equal(t, 1, count(t, db.Reader(), "app_key"))
}

func TestAppKeyRepoSealedWithAnotherMasterKeyIsUnreadable(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newAppKeyRepo(t)
	require.NoError(t, repo.Set(ctx, plainAppKey))

	_, err := store.NewAppKeyRepo(db, store.GenerateKey()).Get(ctx)

	assert.ErrorIs(t, err, core.ErrAppKeyUnreadable)
}

func TestMigration0008CreatesAppKeyAndDropsTheOldCredentialTables(t *testing.T) {
	db, _ := openTemp(t)
	var name string
	require.NoError(t, db.Reader().QueryRow(`SELECT name FROM schema_migrations WHERE version = 8`).Scan(&name))
	assert.Equal(t, "app_key", name)

	for _, table := range []string{"api_keys", "adapter_credentials"} {
		var n int
		require.NoError(t, db.Reader().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n))
		assert.Zero(t, n, table)
	}
	_, err := db.Writer().ExecContext(context.Background(), `INSERT INTO app_key (id, sealed_key) VALUES (2, 'x')`)
	assert.Error(t, err, "only the row with id 1 is allowed")
}
