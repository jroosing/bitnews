package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

var _ core.ServerRepo = (*store.ServerRepo)(nil)

const plainPassword = "s3cret-Pa55word"

func newServerRepo(t *testing.T) (*store.ServerRepo, *store.DB, store.Key) {
	t.Helper()
	db, _ := openTemp(t)
	key := store.GenerateKey()
	return store.NewServerRepo(db, key), db, key
}

func aServer(name string) core.Server {
	return core.Server{
		ID: core.NewID(), Name: name, Host: name + ".example.com", Port: 563,
		Username: "joe", Password: core.Secret(plainPassword), MaxConnections: 20,
		RetentionDays: 3000, Priority: 1, Enabled: true,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 123_000_000, time.UTC),
	}
}

func rawPassword(t *testing.T, db *store.DB, id core.ID) string {
	t.Helper()
	var raw string
	require.NoError(t, db.Reader().QueryRow(`SELECT password FROM servers WHERE id = ?`, string(id)).Scan(&raw))
	return raw
}

func TestServerRepoCreateThenGetRoundTripsIncludingPassword(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newServerRepo(t)
	want := aServer("a")

	require.NoError(t, repo.Create(ctx, want))
	got, err := repo.Get(ctx, want.ID)

	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, plainPassword, got.Password.Reveal())
}

func TestServerRepoStoresThePasswordEncryptedWithTheRowAAD(t *testing.T) {
	ctx := context.Background()
	repo, db, key := newServerRepo(t)
	s := aServer("a")
	require.NoError(t, repo.Create(ctx, s))

	raw := rawPassword(t, db, s.ID)

	assert.NotContains(t, raw, plainPassword)
	assert.True(t, strings.HasPrefix(raw, "v1:"), raw)
	plain, err := store.Unseal(key, raw, store.AAD("servers", "password", string(s.ID)))
	require.NoError(t, err)
	assert.Equal(t, plainPassword, plain)
}

func TestServerRepoStoresAnEmptyPasswordAsEmpty(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newServerRepo(t)
	s := aServer("a")
	s.Username, s.Password = "", ""
	require.NoError(t, repo.Create(ctx, s))

	got, err := repo.Get(ctx, s.ID)

	require.NoError(t, err)
	assert.Empty(t, got.Password)
	assert.False(t, got.PasswordUnavailable)
	assert.Empty(t, rawPassword(t, db, s.ID))
}

func TestServerRepoWithAnotherKeyReturnsServersWithPasswordUnavailable(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newServerRepo(t)
	a, b := aServer("a"), aServer("b")
	noPassword := aServer("c")
	noPassword.Password = ""
	for _, s := range []core.Server{a, b, noPassword} {
		require.NoError(t, repo.Create(ctx, s))
	}
	wrongKey := store.NewServerRepo(db, store.GenerateKey())

	list, err := wrongKey.List(ctx)
	got, getErr := wrongKey.Get(ctx, a.ID)

	require.NoError(t, err, "one undecryptable password must not fail the whole list")
	require.NoError(t, getErr)
	require.Len(t, list, 3)
	for _, s := range list {
		if s.ID == noPassword.ID {
			assert.False(t, s.PasswordUnavailable)
			continue
		}
		assert.True(t, s.PasswordUnavailable, s.Name)
		assert.Empty(t, s.Password, s.Name)
		assert.Equal(t, "joe", s.Username)
	}
	assert.True(t, got.PasswordUnavailable)
}

func TestServerRepoCiphertextSwappedBetweenRowsIsUnavailable(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newServerRepo(t)
	a, b := aServer("a"), aServer("b")
	require.NoError(t, repo.Create(ctx, a))
	require.NoError(t, repo.Create(ctx, b))
	_, err := db.Writer().Exec(`UPDATE servers SET password = (SELECT password FROM servers WHERE id = ?) WHERE id = ?`, string(a.ID), string(b.ID))
	require.NoError(t, err)

	got, err := repo.Get(ctx, b.ID)

	require.NoError(t, err)
	assert.True(t, got.PasswordUnavailable)
}

func TestServerRepoListOrdersByPriorityThenName(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newServerRepo(t)
	for _, tc := range []struct {
		name string
		prio int
	}{{"zeta", 0}, {"alpha", 1}, {"beta", 0}, {"gamma", 1}, {"omega", 5}} {
		s := aServer(tc.name)
		s.Priority = tc.prio
		require.NoError(t, repo.Create(ctx, s))
	}

	list, err := repo.List(ctx)

	require.NoError(t, err)
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{"beta", "zeta", "alpha", "gamma", "omega"}, names)
}

func TestServerRepoDuplicateNameIsConflict(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newServerRepo(t)
	require.NoError(t, repo.Create(ctx, aServer("a")))
	dup := aServer("b")
	dup.Name = "a"

	assert.ErrorIs(t, repo.Create(ctx, dup), core.ErrConflict)
}

func TestServerRepoCreateRejectsInvalidServerWithoutWriting(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newServerRepo(t)
	bad := aServer("a")
	bad.Host = "nntps://news.example.com"

	require.ErrorIs(t, repo.Create(ctx, bad), core.ErrInvalidServerHost)
	_, err := repo.Get(ctx, bad.ID)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestServerRepoUpdateStoresFieldsAndAReSealedPassword(t *testing.T) {
	ctx := context.Background()
	repo, db, key := newServerRepo(t)
	s := aServer("a")
	require.NoError(t, repo.Create(ctx, s))
	before := rawPassword(t, db, s.ID)
	s.Name, s.Host, s.Port, s.Username = "renamed", "other.example.org", 443, "ann"
	s.Password, s.MaxConnections, s.RetentionDays, s.Priority, s.Enabled = "new-secret", 5, 0, 2, false
	s.CreatedAt = s.CreatedAt.Add(time.Hour) // immutable

	require.NoError(t, repo.Update(ctx, s))
	got, err := repo.Get(ctx, s.ID)

	require.NoError(t, err)
	s.CreatedAt = s.CreatedAt.Add(-time.Hour)
	assert.Equal(t, s, got)
	after := rawPassword(t, db, s.ID)
	assert.NotEqual(t, before, after)
	plain, err := store.Unseal(key, after, store.AAD("servers", "password", string(s.ID)))
	require.NoError(t, err)
	assert.Equal(t, "new-secret", plain)
}

func TestServerRepoUpdateWithPasswordUnavailableLeavesTheStoredPasswordAlone(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newServerRepo(t)
	s := aServer("a")
	require.NoError(t, repo.Create(ctx, s))
	before := rawPassword(t, db, s.ID)
	s.Name, s.Password, s.PasswordUnavailable = "renamed", "", true

	require.NoError(t, repo.Update(ctx, s))

	assert.Equal(t, before, rawPassword(t, db, s.ID))
	got, err := repo.Get(ctx, s.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, plainPassword, got.Password.Reveal())
}

func TestServerRepoUpdateUnknownIDAndDuplicateName(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newServerRepo(t)
	a, b := aServer("a"), aServer("b")
	require.NoError(t, repo.Create(ctx, a))
	require.NoError(t, repo.Create(ctx, b))

	assert.ErrorIs(t, repo.Update(ctx, aServer("ghost")), core.ErrNotFound)
	b.Name = "a"
	assert.ErrorIs(t, repo.Update(ctx, b), core.ErrConflict)
}

func TestServerRepoDeleteRemovesTheServer(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newServerRepo(t)
	s := aServer("a")
	require.NoError(t, repo.Create(ctx, s))

	require.NoError(t, repo.Delete(ctx, s.ID))

	_, err := repo.Get(ctx, s.ID)
	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.ErrorIs(t, repo.Delete(ctx, s.ID), core.ErrNotFound)
}

func TestServerRepoListIsEmptyNotAnError(t *testing.T) {
	repo, _, _ := newServerRepo(t)

	list, err := repo.List(context.Background())

	require.NoError(t, err)
	assert.Empty(t, list)
}
