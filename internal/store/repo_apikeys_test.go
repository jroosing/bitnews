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

var _ core.APIKeyRepo = (*store.APIKeyRepo)(nil)

func newAPIKeyRepo(t *testing.T) *store.APIKeyRepo {
	t.Helper()
	db, _ := openTemp(t)
	return store.NewAPIKeyRepo(db)
}

func newKey(name string, seed byte) core.APIKey {
	return core.APIKey{
		ID:        core.NewID(),
		Name:      name,
		Scope:     core.APIKeyScopeRead,
		Hash:      core.TokenHash{seed},
		CreatedAt: t0,
	}
}

func TestAPIKeyRepoCreateThenGetByHashRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo := newAPIKeyRepo(t)
	want := newKey("script", 1)

	require.NoError(t, repo.Create(ctx, want))
	got, err := repo.GetByHash(ctx, want.Hash)

	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.True(t, got.LastUsedAt.IsZero(), "never used yet")
}

func TestAPIKeyRepoGetByUnknownHashIsNotFound(t *testing.T) {
	_, err := newAPIKeyRepo(t).GetByHash(context.Background(), core.TokenHash{7})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestAPIKeyRepoCreateDuplicateHashIsConflict(t *testing.T) {
	ctx := context.Background()
	repo := newAPIKeyRepo(t)
	require.NoError(t, repo.Create(ctx, newKey("a", 1)))

	err := repo.Create(ctx, newKey("b", 1))

	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestAPIKeyRepoCreateRejectsUnknownScope(t *testing.T) {
	k := newKey("a", 1)
	k.Scope = "admin"

	err := newAPIKeyRepo(t).Create(context.Background(), k)

	assert.Error(t, err)
}

func TestAPIKeyRepoListReturnsKeysOldestFirst(t *testing.T) {
	ctx := context.Background()
	repo := newAPIKeyRepo(t)
	second, first := newKey("second", 2), newKey("first", 1)
	second.CreatedAt = t0.Add(time.Hour)
	require.NoError(t, repo.Create(ctx, second))
	require.NoError(t, repo.Create(ctx, first))

	got, err := repo.List(ctx)

	require.NoError(t, err)
	assert.Equal(t, []core.APIKey{first, second}, got)
}

func TestAPIKeyRepoTouchLastUsedStoresTime(t *testing.T) {
	ctx := context.Background()
	repo := newAPIKeyRepo(t)
	k := newKey("a", 1)
	require.NoError(t, repo.Create(ctx, k))

	require.NoError(t, repo.TouchLastUsed(ctx, k.ID, t0.Add(time.Minute)))

	got, err := repo.GetByHash(ctx, k.Hash)
	require.NoError(t, err)
	assert.Equal(t, t0.Add(time.Minute), got.LastUsedAt)
}

func TestAPIKeyRepoTouchLastUsedUnknownIDIsNotFound(t *testing.T) {
	err := newAPIKeyRepo(t).TouchLastUsed(context.Background(), core.NewID(), t0)

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestAPIKeyRepoDeleteRevokesKey(t *testing.T) {
	ctx := context.Background()
	repo := newAPIKeyRepo(t)
	k := newKey("a", 1)
	require.NoError(t, repo.Create(ctx, k))

	require.NoError(t, repo.Delete(ctx, k.ID))

	_, err := repo.GetByHash(ctx, k.Hash)
	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.ErrorIs(t, repo.Delete(ctx, k.ID), core.ErrNotFound)
}
