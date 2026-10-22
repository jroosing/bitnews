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

var _ core.SessionRepo = (*store.SessionRepo)(nil)

type sessionFixture struct {
	sessions *store.SessionRepo
	user     core.User
}

func newSessionFixture(t *testing.T) sessionFixture {
	t.Helper()
	db, _ := openTemp(t)
	users := store.NewUserRepo(db)
	u := newUser("admin")
	require.NoError(t, users.Create(context.Background(), u))
	return sessionFixture{sessions: store.NewSessionRepo(db), user: u}
}

func (f sessionFixture) session(seed byte, created, seen time.Time) core.Session {
	var h core.TokenHash
	h[0] = seed
	return core.Session{TokenHash: h, UserID: f.user.ID, CreatedAt: created, LastSeenAt: seen}
}

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestSessionRepoCreateThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	f := newSessionFixture(t)
	want := f.session(1, t0, t0.Add(time.Minute))

	require.NoError(t, f.sessions.Create(ctx, want))
	got, err := f.sessions.Get(ctx, want.TokenHash)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestSessionRepoGetUnknownHashIsNotFound(t *testing.T) {
	f := newSessionFixture(t)

	_, err := f.sessions.Get(context.Background(), core.TokenHash{9})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestSessionRepoCreateDuplicateHashIsConflict(t *testing.T) {
	ctx := context.Background()
	f := newSessionFixture(t)
	require.NoError(t, f.sessions.Create(ctx, f.session(1, t0, t0)))

	err := f.sessions.Create(ctx, f.session(1, t0, t0))

	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestSessionRepoTouchUpdatesOnlyLastSeen(t *testing.T) {
	ctx := context.Background()
	f := newSessionFixture(t)
	s := f.session(1, t0, t0)
	require.NoError(t, f.sessions.Create(ctx, s))

	require.NoError(t, f.sessions.Touch(ctx, s.TokenHash, t0.Add(time.Hour)))

	got, err := f.sessions.Get(ctx, s.TokenHash)
	require.NoError(t, err)
	assert.Equal(t, t0.Add(time.Hour), got.LastSeenAt)
	assert.Equal(t, t0, got.CreatedAt)
}

func TestSessionRepoTouchUnknownHashIsNotFound(t *testing.T) {
	f := newSessionFixture(t)

	err := f.sessions.Touch(context.Background(), core.TokenHash{9}, t0)

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestSessionRepoDeleteRemovesSession(t *testing.T) {
	ctx := context.Background()
	f := newSessionFixture(t)
	s := f.session(1, t0, t0)
	require.NoError(t, f.sessions.Create(ctx, s))

	require.NoError(t, f.sessions.Delete(ctx, s.TokenHash))

	_, err := f.sessions.Get(ctx, s.TokenHash)
	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.ErrorIs(t, f.sessions.Delete(ctx, s.TokenHash), core.ErrNotFound)
}

func TestSessionRepoDeleteExpiredRemovesIdleAndAbsoluteExpired(t *testing.T) {
	ctx := context.Background()
	f := newSessionFixture(t)
	old, fresh := t0, t0.Add(48*time.Hour)
	keep := f.session(1, fresh, fresh)
	idle := f.session(2, fresh, old) // seen long ago
	abs := f.session(3, old, fresh)  // created long ago
	for _, s := range []core.Session{keep, idle, abs} {
		require.NoError(t, f.sessions.Create(ctx, s))
	}

	n, err := f.sessions.DeleteExpired(ctx, t0.Add(24*time.Hour), t0.Add(24*time.Hour))

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	_, err = f.sessions.Get(ctx, keep.TokenHash)
	assert.NoError(t, err)
	_, err = f.sessions.Get(ctx, idle.TokenHash)
	assert.ErrorIs(t, err, core.ErrNotFound)
	_, err = f.sessions.Get(ctx, abs.TokenHash)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestSessionRepoSessionsDieWithTheirUser(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	users := store.NewUserRepo(db)
	u := newUser("admin")
	require.NoError(t, users.Create(ctx, u))
	sessions := store.NewSessionRepo(db)
	s := core.Session{TokenHash: core.TokenHash{1}, UserID: u.ID, CreatedAt: t0, LastSeenAt: t0}
	require.NoError(t, sessions.Create(ctx, s))

	_, err := db.Writer().ExecContext(ctx, `DELETE FROM users WHERE id = ?`, string(u.ID))
	require.NoError(t, err)

	_, err = sessions.Get(ctx, s.TokenHash)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestSessionRepoDeleteForUserExceptKeepsOnlyTheNamedSession(t *testing.T) {
	ctx := context.Background()
	f := newSessionFixture(t)
	keep, other1, other2 := f.session(1, t0, t0), f.session(2, t0, t0), f.session(3, t0, t0)
	for _, s := range []core.Session{keep, other1, other2} {
		require.NoError(t, f.sessions.Create(ctx, s))
	}

	n, err := f.sessions.DeleteForUserExcept(ctx, f.user.ID, keep.TokenHash)

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	_, err = f.sessions.Get(ctx, keep.TokenHash)
	assert.NoError(t, err)
	_, err = f.sessions.Get(ctx, other1.TokenHash)
	assert.ErrorIs(t, err, core.ErrNotFound)
	_, err = f.sessions.Get(ctx, other2.TokenHash)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestSessionRepoDeleteForUserExceptLeavesOtherUsersAlone(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	users := store.NewUserRepo(db)
	a, b := newUser("alice"), newUser("bob")
	require.NoError(t, users.Create(ctx, a))
	require.NoError(t, users.Create(ctx, b))
	repo := store.NewSessionRepo(db)
	mine := core.Session{TokenHash: core.TokenHash{1}, UserID: a.ID, CreatedAt: t0, LastSeenAt: t0}
	theirs := core.Session{TokenHash: core.TokenHash{2}, UserID: b.ID, CreatedAt: t0, LastSeenAt: t0}
	require.NoError(t, repo.Create(ctx, mine))
	require.NoError(t, repo.Create(ctx, theirs))

	n, err := repo.DeleteForUserExcept(ctx, a.ID, core.TokenHash{9})

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = repo.Get(ctx, theirs.TokenHash)
	assert.NoError(t, err)
}
