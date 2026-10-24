package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

var _ core.UserRepo = (*store.UserRepo)(nil)

func newUserRepo(t *testing.T) *store.UserRepo {
	t.Helper()
	db, _ := openTemp(t)
	return store.NewUserRepo(db)
}

func newUser(name string) core.User {
	return core.User{
		ID:           core.NewID(),
		Username:     name,
		PasswordHash: []byte("$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA"),
		CreatedAt:    time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestUserRepoCreateThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo := newUserRepo(t)
	want := newUser("admin")

	require.NoError(t, repo.Create(ctx, want))
	got, err := repo.Get(ctx, want.ID)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestUserRepoGetUnknownIDIsNotFound(t *testing.T) {
	_, err := newUserRepo(t).Get(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestUserRepoGetByUsernameFindsUser(t *testing.T) {
	ctx := context.Background()
	repo := newUserRepo(t)
	want := newUser("admin")
	require.NoError(t, repo.Create(ctx, want))

	got, err := repo.GetByUsername(ctx, "admin")

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestUserRepoGetByUnknownUsernameIsNotFound(t *testing.T) {
	_, err := newUserRepo(t).GetByUsername(context.Background(), "nobody")

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestUserRepoUsernamesAreCaseSensitive(t *testing.T) {
	ctx := context.Background()
	repo := newUserRepo(t)
	require.NoError(t, repo.Create(ctx, newUser("admin")))

	require.NoError(t, repo.Create(ctx, newUser("Admin")), "exact-match uniqueness")
	_, err := repo.GetByUsername(ctx, "ADMIN")

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestUserRepoCreateDuplicateUsernameIsConflict(t *testing.T) {
	ctx := context.Background()
	repo := newUserRepo(t)
	require.NoError(t, repo.Create(ctx, newUser("admin")))

	err := repo.Create(ctx, newUser("admin"))

	assert.ErrorIs(t, err, core.ErrConflict)
	n, err := repo.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestUserRepoCreateDuplicateIDIsConflict(t *testing.T) {
	ctx := context.Background()
	repo := newUserRepo(t)
	u := newUser("a")
	require.NoError(t, repo.Create(ctx, u))
	u.Username = "b"

	assert.ErrorIs(t, repo.Create(ctx, u), core.ErrConflict)
}

func TestUserRepoCountTracksUsers(t *testing.T) {
	ctx := context.Background()
	repo := newUserRepo(t)

	n, err := repo.Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)

	require.NoError(t, repo.Create(ctx, newUser("a")))
	require.NoError(t, repo.Create(ctx, newUser("b")))
	n, err = repo.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestUserRepoStoresCreatedAtAsUTCMilliseconds(t *testing.T) {
	ctx := context.Background()
	repo := newUserRepo(t)
	u := newUser("admin")
	u.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.FixedZone("x", 3600))
	require.NoError(t, repo.Create(ctx, u))

	got, err := repo.Get(ctx, u.ID)

	require.NoError(t, err)
	assert.Equal(t, time.UnixMilli(u.CreatedAt.UnixMilli()).UTC(), got.CreatedAt)
	assert.Equal(t, time.UTC, got.CreatedAt.Location())
}

func TestUserRepoUpdatePasswordHashReplacesHash(t *testing.T) {
	ctx := context.Background()
	repo := newUserRepo(t)
	u := newUser("admin")
	require.NoError(t, repo.Create(ctx, u))

	require.NoError(t, repo.UpdatePasswordHash(ctx, u.ID, []byte("$argon2id$new")))

	got, err := repo.Get(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, []byte("$argon2id$new"), got.PasswordHash)
	assert.Equal(t, u.Username, got.Username)
	assert.Equal(t, u.CreatedAt, got.CreatedAt)
}

func TestUserRepoUpdatePasswordHashUnknownIDIsNotFound(t *testing.T) {
	err := newUserRepo(t).UpdatePasswordHash(context.Background(), core.NewID(), []byte("x"))

	assert.ErrorIs(t, err, core.ErrNotFound)
}
