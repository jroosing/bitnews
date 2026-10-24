package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

var _ core.AppKeyRepo = (*store.AppKeyRepo)(nil)

const plainAppKey = "bitnews_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqr"

func newAppKeyRepo(t *testing.T) (*store.AppKeyRepo, *store.DB) {
	t.Helper()
	db, _ := openTemp(t)
	return store.NewAppKeyRepo(db), db
}

func rawAppKey(t *testing.T, db *store.DB) string {
	t.Helper()
	var raw string
	require.NoError(t, db.Reader().QueryRow(`SELECT key FROM app_key WHERE id = 1`).Scan(&raw))
	return raw
}

func TestAppKeyRepoHasNoKeyOnAFreshDatabase(t *testing.T) {
	repo, _ := newAppKeyRepo(t)

	_, err := repo.Get(context.Background())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestAppKeyRepoSetThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAppKeyRepo(t)

	require.NoError(t, repo.Set(ctx, plainAppKey))
	got, err := repo.Get(ctx)

	require.NoError(t, err)
	assert.Equal(t, plainAppKey, got.Reveal())
}

func TestAppKeyRepoStoresTheKeyAsPlainText(t *testing.T) {
	ctx := context.Background()
	repo, db := newAppKeyRepo(t)
	require.NoError(t, repo.Set(ctx, plainAppKey))

	assert.Equal(t, plainAppKey, rawAppKey(t, db))
}

func TestAppKeyRepoSetReplacesTheKeyAndKeepsASingleRow(t *testing.T) {
	ctx := context.Background()
	repo, db := newAppKeyRepo(t)
	require.NoError(t, repo.Set(ctx, plainAppKey))

	require.NoError(t, repo.Set(ctx, "bitnews_other"))
	got, err := repo.Get(ctx)

	require.NoError(t, err)
	assert.Equal(t, "bitnews_other", got.Reveal())
	assert.Equal(t, 1, count(t, db.Reader(), "app_key"))
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
	_, err := db.Writer().ExecContext(context.Background(), `INSERT INTO app_key (id, key, updated_at) VALUES (2, 'x', 0)`)
	assert.Error(t, err, "only the row with id 1 is allowed")
	_, err = db.Writer().ExecContext(context.Background(), `INSERT INTO app_key (id, key, updated_at) VALUES (1, '', 0)`)
	assert.Error(t, err, "an empty key is refused")
}
