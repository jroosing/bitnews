package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

var _ core.SettingsRepo = (*store.SettingsRepo)(nil)

func newSettingsRepo(t *testing.T) (*store.SettingsRepo, *store.DB) {
	t.Helper()
	db, _ := openTemp(t)
	return store.NewSettingsRepo(db), db
}

func TestSettingsRepoLoadOnEmptyDBReturnsDefaults(t *testing.T) {
	repo, _ := newSettingsRepo(t)

	got, err := repo.Load(context.Background())

	require.NoError(t, err)
	assert.Equal(t, core.DefaultSettings(), got)
}

func TestSettingsRepoSaveThenLoadRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo, _ := newSettingsRepo(t)
	want := core.DefaultSettings()
	want.Queues = core.QueueLimits{Usenet: 3, Torrent: 8}
	want.Auth = core.Auth{
		LANBypassCIDRs: []string{"192.168.1.0/24"},
		TrustedProxies: []string{"10.0.0.1/32"},
		AllowedHosts:   []string{"bitnews.lan"},
	}
	want.Paths.Usenet.Complete = "/mnt/usenet"

	require.NoError(t, repo.Save(ctx, want))
	got, err := repo.Load(ctx)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestSettingsRepoSaveOverwritesPreviousValue(t *testing.T) {
	ctx := context.Background()
	repo, _ := newSettingsRepo(t)
	s := core.DefaultSettings()
	require.NoError(t, repo.Save(ctx, s))
	s.Queues.Usenet = 9

	require.NoError(t, repo.Save(ctx, s))

	got, err := repo.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 9, got.Queues.Usenet)
}

func TestSettingsRepoSaveRejectsInvalidSettingsAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	repo, db := newSettingsRepo(t)
	good := core.DefaultSettings()
	require.NoError(t, repo.Save(ctx, good))
	bad := core.DefaultSettings()
	bad.Queues.Usenet = -1

	err := repo.Save(ctx, bad)

	require.ErrorIs(t, err, core.ErrInvalidSettings)
	got, err := repo.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, good, got)
	var n int
	require.NoError(t, db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestSettingsRepoStoresJSONUnderOneKey(t *testing.T) {
	ctx := context.Background()
	repo, db := newSettingsRepo(t)
	require.NoError(t, repo.Save(ctx, core.DefaultSettings()))

	var key, value string
	err := db.Reader().QueryRowContext(ctx, `SELECT key, value FROM settings`).Scan(&key, &value)

	require.NoError(t, err)
	assert.Equal(t, "settings", key)
	assert.JSONEq(t, `{"usenet":1,"torrent":5}`, jsonField(t, value, "queues"))
}

func TestSettingsRepoLoadHonorsCancelledContext(t *testing.T) {
	repo, _ := newSettingsRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.Load(ctx)

	assert.ErrorIs(t, err, context.Canceled)
}

func jsonField(t *testing.T, doc, field string) string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(doc), &m))
	return string(m[field])
}

func TestSettingsRepoLoadFillsFieldsMissingFromStoredJSONWithDefaults(t *testing.T) {
	ctx := context.Background()
	repo, db := newSettingsRepo(t)
	_, err := db.Writer().ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES ('settings', '{"queues":{"usenet":4}}', 0)`)
	require.NoError(t, err)

	got, err := repo.Load(ctx)

	require.NoError(t, err)
	want := core.DefaultSettings()
	want.Queues.Usenet = 4
	assert.Equal(t, want, got)
}
