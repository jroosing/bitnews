package auth_test

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

type sessionEnv struct {
	svc      *auth.SessionService
	sessions *fakeSessions
	users    *fakeUsers
	clock    *fakeClock
	user     core.User
}

func newSessionEnv(t *testing.T, opts ...auth.SessionOption) sessionEnv {
	t.Helper()
	auth.UseFastParams(t)
	env := sessionEnv{sessions: newFakeSessions(), users: &fakeUsers{}, clock: newClock()}
	hash, err := auth.HashPassword(goodPassword)
	require.NoError(t, err)
	env.user = core.User{ID: core.NewID(), Username: "admin", PasswordHash: hash, CreatedAt: env.clock.Now()}
	require.NoError(t, env.users.Create(context.Background(), env.user))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts = append([]auth.SessionOption{auth.WithSessionClock(env.clock.Now), auth.WithSessionLogger(quiet)}, opts...)
	env.svc, err = auth.NewSessionService(env.sessions, env.users, opts...)
	require.NoError(t, err)
	return env
}

func TestCreateSessionStoresOnlyTheTokenHash(t *testing.T) {
	env := newSessionEnv(t)

	token, expires, err := env.svc.CreateSession(context.Background(), env.user.ID)

	require.NoError(t, err)
	row, err := env.sessions.Get(context.Background(), auth.HashToken(token))
	require.NoError(t, err)
	assert.Equal(t, env.user.ID, row.UserID)
	assert.Equal(t, env.clock.Now(), row.CreatedAt)
	assert.Equal(t, env.clock.Now(), row.LastSeenAt)
	assert.Equal(t, env.clock.Now().Add(30*24*time.Hour), expires, "latest the session can live")
}

func TestAuthenticateReturnsSessionAndUser(t *testing.T) {
	env := newSessionEnv(t)
	token, _, _ := env.svc.CreateSession(context.Background(), env.user.ID)

	s, u, err := env.svc.Authenticate(context.Background(), token)

	require.NoError(t, err)
	assert.Equal(t, env.user.ID, u.ID)
	assert.Equal(t, env.user.ID, s.UserID)
	assert.Equal(t, auth.HashToken(token), s.TokenHash)
}

func TestAuthenticateRejectsUnknownAndMalformedTokens(t *testing.T) {
	env := newSessionEnv(t)
	unknown, _, _ := auth.NewToken()

	for _, tok := range []string{unknown, "", "short", string(make([]byte, 10_000))} {
		_, _, err := env.svc.Authenticate(context.Background(), tok)

		assert.ErrorIs(t, err, auth.ErrInvalidSession)
	}
}

func TestSessionExpiresAfterSevenDaysIdle(t *testing.T) {
	env := newSessionEnv(t)
	token, _, _ := env.svc.CreateSession(context.Background(), env.user.ID)

	env.clock.Advance(7*24*time.Hour - time.Second)
	_, _, err := env.svc.Authenticate(context.Background(), token)
	require.NoError(t, err, "still inside the idle window")

	env.clock.Advance(7*24*time.Hour + time.Second)
	_, _, err = env.svc.Authenticate(context.Background(), token)
	assert.ErrorIs(t, err, auth.ErrInvalidSession)
	assert.Zero(t, env.sessions.count(), "the dead row is removed")
}

func TestActivityKeepsASessionAliveUntilThirtyDaysAbsolute(t *testing.T) {
	env := newSessionEnv(t)
	token, _, _ := env.svc.CreateSession(context.Background(), env.user.ID)

	for day := 1; day < 30; day++ {
		env.clock.Advance(24 * time.Hour)
		_, _, err := env.svc.Authenticate(context.Background(), token)
		require.NoError(t, err, "day %d", day)
	}
	env.clock.Advance(24*time.Hour + time.Second) // past day 30
	_, _, err := env.svc.Authenticate(context.Background(), token)

	assert.ErrorIs(t, err, auth.ErrInvalidSession, "absolute limit beats recent activity")
}

func TestAuthenticateTouchesAtMostOncePerMinute(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()
	token, _, _ := env.svc.CreateSession(ctx, env.user.ID)

	for range 5 {
		env.clock.Advance(10 * time.Second)
		_, _, err := env.svc.Authenticate(ctx, token)
		require.NoError(t, err)
	}
	assert.Equal(t, 0, env.sessions.touchCount(), "50 s since creation")

	env.clock.Advance(10 * time.Second)
	_, _, _ = env.svc.Authenticate(ctx, token)
	assert.Equal(t, 1, env.sessions.touchCount(), "a minute has passed")

	env.clock.Advance(59 * time.Second)
	_, _, _ = env.svc.Authenticate(ctx, token)
	assert.Equal(t, 1, env.sessions.touchCount())
}

func TestAuthenticateRejectsASessionWhoseUserIsGone(t *testing.T) {
	env := newSessionEnv(t)
	token, _, _ := env.svc.CreateSession(context.Background(), core.NewID()) // no such user

	_, _, err := env.svc.Authenticate(context.Background(), token)

	assert.ErrorIs(t, err, auth.ErrInvalidSession)
}

func TestLogoutDeletesTheSession(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()
	token, _, _ := env.svc.CreateSession(ctx, env.user.ID)

	require.NoError(t, env.svc.Logout(ctx, token))

	_, _, err := env.svc.Authenticate(ctx, token)
	assert.ErrorIs(t, err, auth.ErrInvalidSession)
	assert.NoError(t, env.svc.Logout(ctx, token), "logging out twice is fine")
}

func TestCleanupRemovesOnlyExpiredSessions(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()
	old, _, _ := env.svc.CreateSession(ctx, env.user.ID)
	env.clock.Advance(8 * 24 * time.Hour)
	fresh, _, _ := env.svc.CreateSession(ctx, env.user.ID)

	n, err := env.svc.Cleanup(ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, _, err = env.svc.Authenticate(ctx, fresh)
	assert.NoError(t, err)
	_, _, err = env.svc.Authenticate(ctx, old)
	assert.ErrorIs(t, err, auth.ErrInvalidSession)
}

func TestSessionExpiryIsTheEarlierOfIdleAndAbsolute(t *testing.T) {
	now := newClock().Now()
	s := core.Session{CreatedAt: now, LastSeenAt: now.Add(29 * 24 * time.Hour)}

	assert.Equal(t, now.Add(30*24*time.Hour), auth.SessionExpiry(s))
	s.LastSeenAt = now.Add(time.Hour)
	assert.Equal(t, now.Add(time.Hour+7*24*time.Hour), auth.SessionExpiry(s))
}

func TestRunCleanupRemovesExpiredSessionsUntilCancelled(t *testing.T) {
	env := newSessionEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, _, _ = env.svc.CreateSession(ctx, env.user.ID)
	env.clock.Advance(auth.SessionIdleTimeout + time.Second)
	done := make(chan struct{})

	go func() { env.svc.RunCleanup(ctx, 5*time.Millisecond); close(done) }()
	require.Eventually(t, func() bool { return env.sessions.count() == 0 }, 2*time.Second, 5*time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunCleanup did not stop after cancel")
	}
}

func TestNewSessionServiceDoesNotRunArgon2AtConstruction(t *testing.T) {
	// Production parameters (64 MiB) on purpose: no fast-params hook here.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	svc, err := auth.NewSessionService(newFakeSessions(), &fakeUsers{})

	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.NotNil(t, svc)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<20),
		"building the service must not allocate the 64 MiB argon2 working set; the unknown-user hash is made on first use")
}
