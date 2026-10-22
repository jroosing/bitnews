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

var _ core.AdapterCredentialRepo = (*store.AdapterCredentialRepo)(nil)

func newAdapterRepo(t *testing.T) *store.AdapterCredentialRepo {
	t.Helper()
	db, _ := openTemp(t)
	return store.NewAdapterCredentialRepo(db)
}

func sabCred(name string, seed byte) core.AdapterCredential {
	return core.AdapterCredential{
		ID: core.NewID(), Kind: core.AdapterSAB, Name: name,
		SecretHash: core.TokenHash{seed}, CreatedAt: t0,
	}
}

func qbitCred(name, username string, seed byte) core.AdapterCredential {
	c := sabCred(name, seed)
	c.Kind, c.Username = core.AdapterQBit, username
	return c
}

func TestAdapterCredentialRepoCreateThenGetByHashRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo := newAdapterRepo(t)
	sab, qbit := sabCred("Sonarr", 1), qbitCred("Radarr", "radarr", 2)
	require.NoError(t, repo.Create(ctx, sab))
	require.NoError(t, repo.Create(ctx, qbit))

	gotSab, err := repo.GetByHash(ctx, sab.SecretHash)
	require.NoError(t, err)
	gotQbit, err := repo.GetByHash(ctx, qbit.SecretHash)
	require.NoError(t, err)

	assert.Equal(t, sab, gotSab)
	assert.Equal(t, qbit, gotQbit)
	assert.True(t, gotSab.LastUsedAt.IsZero(), "never used yet")
}

func TestAdapterCredentialRepoGetByUnknownHashIsNotFound(t *testing.T) {
	_, err := newAdapterRepo(t).GetByHash(context.Background(), core.TokenHash{7})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestAdapterCredentialRepoGetByUsernameFindsOnlyThatKind(t *testing.T) {
	ctx := context.Background()
	repo := newAdapterRepo(t)
	want := qbitCred("Radarr", "radarr", 2)
	require.NoError(t, repo.Create(ctx, want))

	got, err := repo.GetByUsername(ctx, core.AdapterQBit, "radarr")
	_, wrongKind := repo.GetByUsername(ctx, core.AdapterSAB, "radarr")
	_, unknown := repo.GetByUsername(ctx, core.AdapterQBit, "sonarr")

	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.ErrorIs(t, wrongKind, core.ErrNotFound)
	assert.ErrorIs(t, unknown, core.ErrNotFound)
}

func TestAdapterCredentialRepoCreateConflicts(t *testing.T) {
	ctx := context.Background()
	repo := newAdapterRepo(t)
	require.NoError(t, repo.Create(ctx, qbitCred("a", "radarr", 1)))

	assert.ErrorIs(t, repo.Create(ctx, sabCred("dup hash", 1)), core.ErrConflict)
	assert.ErrorIs(t, repo.Create(ctx, qbitCred("dup user", "radarr", 2)), core.ErrConflict)
}

func TestAdapterCredentialRepoSABCredentialsMayShareNames(t *testing.T) {
	ctx := context.Background()
	repo := newAdapterRepo(t)

	require.NoError(t, repo.Create(ctx, sabCred("Sonarr", 1)))
	require.NoError(t, repo.Create(ctx, sabCred("Sonarr", 2)))
}

func TestAdapterCredentialRepoSchemaRejectsMalformedRows(t *testing.T) {
	ctx := context.Background()
	repo := newAdapterRepo(t)
	badKind := sabCred("a", 1)
	badKind.Kind = "adapter_ftp"
	sabWithUser := sabCred("b", 2)
	sabWithUser.Username = "sonarr"
	qbitNoUser := qbitCred("c", "", 3)

	for name, c := range map[string]core.AdapterCredential{
		"unknown kind": badKind, "sab with username": sabWithUser, "qbit without username": qbitNoUser,
	} {
		assert.Error(t, repo.Create(ctx, c), name)
	}
}

func TestAdapterCredentialRepoListReturnsOldestFirst(t *testing.T) {
	ctx := context.Background()
	repo := newAdapterRepo(t)
	second, first := sabCred("second", 2), sabCred("first", 1)
	second.CreatedAt = t0.Add(time.Hour)
	require.NoError(t, repo.Create(ctx, second))
	require.NoError(t, repo.Create(ctx, first))

	got, err := repo.List(ctx)

	require.NoError(t, err)
	assert.Equal(t, []core.AdapterCredential{first, second}, got)
}

func TestAdapterCredentialRepoTouchLastUsedStoresTime(t *testing.T) {
	ctx := context.Background()
	repo := newAdapterRepo(t)
	c := sabCred("a", 1)
	require.NoError(t, repo.Create(ctx, c))

	require.NoError(t, repo.TouchLastUsed(ctx, c.ID, t0.Add(time.Minute)))

	got, err := repo.GetByHash(ctx, c.SecretHash)
	require.NoError(t, err)
	assert.Equal(t, t0.Add(time.Minute), got.LastUsedAt)
	assert.ErrorIs(t, repo.TouchLastUsed(ctx, core.NewID(), t0), core.ErrNotFound)
}

func TestAdapterCredentialRepoDeleteRevokes(t *testing.T) {
	ctx := context.Background()
	repo := newAdapterRepo(t)
	c := qbitCred("a", "radarr", 1)
	require.NoError(t, repo.Create(ctx, c))

	require.NoError(t, repo.Delete(ctx, c.ID))

	_, err := repo.GetByHash(ctx, c.SecretHash)
	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.ErrorIs(t, repo.Delete(ctx, c.ID), core.ErrNotFound)
	require.NoError(t, repo.Create(ctx, qbitCred("again", "radarr", 2)), "the username is free again")
}
