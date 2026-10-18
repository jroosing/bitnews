package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log/slog"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

func openStore(t *testing.T) (*store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bitnews.db")
	db, err := store.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// fakeUsers is an in-memory core.UserRepo. It enforces unique usernames like
// the real store but nothing else, so concurrency safety must come from auth.
type fakeUsers struct {
	mu    sync.Mutex
	users []core.User
}

func (f *fakeUsers) Create(_ context.Context, u core.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.users {
		if e.Username == u.Username {
			return core.ErrConflict
		}
	}
	f.users = append(f.users, u)
	return nil
}

func (f *fakeUsers) Get(_ context.Context, id core.ID) (core.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.users {
		if e.ID == id {
			return e, nil
		}
	}
	return core.User{}, core.ErrNotFound
}

func (f *fakeUsers) GetByUsername(_ context.Context, name string) (core.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.users {
		if e.Username == name {
			return e, nil
		}
	}
	return core.User{}, core.ErrNotFound
}

func (f *fakeUsers) Count(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users), nil
}

// logBuf is a concurrency-safe log sink that records everything.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// records parses the JSON log lines.
func (l *logBuf) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		out = append(out, m)
	}
	return out
}

func newLogger() (*slog.Logger, *logBuf) {
	buf := &logBuf{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// setupToken extracts the one-time token from the captured log.
func setupToken(t *testing.T, buf *logBuf) string {
	t.Helper()
	var tokens []string
	for _, r := range buf.records(t) {
		if tok, ok := r["setup_token"].(string); ok {
			assert.Equal(t, "WARN", r["level"])
			tokens = append(tokens, tok)
		}
	}
	require.Len(t, tokens, 1, "token must be logged exactly once")
	return tokens[0]
}

const goodPassword = core.Secret("a perfectly fine passphrase")

func TestNewSetupWithNoUsersRequiresSetupAndLogsTokenOnceAtWarn(t *testing.T) {
	logger, buf := newLogger()

	s, err := auth.NewSetup(context.Background(), &fakeUsers{}, logger)

	require.NoError(t, err)
	assert.True(t, s.Required())
	tok := setupToken(t, buf)
	assert.GreaterOrEqual(t, len(tok), 52, "256 bits in base32")
}

func TestNewSetupWithExistingUserNeedsNoSetupAndLogsNoToken(t *testing.T) {
	logger, buf := newLogger()
	users := &fakeUsers{users: []core.User{{ID: core.NewID(), Username: "admin"}}}

	s, err := auth.NewSetup(context.Background(), users, logger)

	require.NoError(t, err)
	assert.False(t, s.Required())
	assert.NotContains(t, buf.String(), "setup_token")
}

func newSetup(t *testing.T, users core.UserRepo) (*auth.Setup, string, *logBuf) {
	t.Helper()
	auth.UseFastParams(t)
	logger, buf := newLogger()
	s, err := auth.NewSetup(context.Background(), users, logger)
	require.NoError(t, err)
	return s, setupToken(t, buf), buf
}

func TestCompleteWithValidTokenCreatesExactlyOneAdminWithVerifiableHash(t *testing.T) {
	ctx := context.Background()
	users := &fakeUsers{}
	s, tok, _ := newSetup(t, users)

	err := s.Complete(ctx, tok, "admin", goodPassword)

	require.NoError(t, err)
	assert.False(t, s.Required())
	require.Len(t, users.users, 1)
	u := users.users[0]
	assert.Equal(t, "admin", u.Username)
	assert.NotEmpty(t, u.ID)
	assert.WithinDuration(t, time.Now(), u.CreatedAt, time.Minute)
	ok, err := auth.VerifyPassword(u.PasswordHash, goodPassword)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.NotContains(t, string(u.PasswordHash), goodPassword.Reveal())
}

func TestCompleteWithWrongOrEmptyTokenFailsAndCreatesNothing(t *testing.T) {
	users := &fakeUsers{}
	s, tok, _ := newSetup(t, users)

	for _, bad := range []string{"", "wrong", tok + "x", tok[:len(tok)-1], strings.ToLower(tok)} {
		err := s.Complete(context.Background(), bad, "admin", goodPassword)

		assert.ErrorIs(t, err, auth.ErrBadSetupToken, "token %q", bad)
	}
	assert.Empty(t, users.users)
	assert.True(t, s.Required())
	assert.NoError(t, s.Complete(context.Background(), tok, "admin", goodPassword), "real token still works")
}

func TestCompleteAfterSuccessIsSetupClosedEvenWithTheRightToken(t *testing.T) {
	ctx := context.Background()
	users := &fakeUsers{}
	s, tok, _ := newSetup(t, users)
	require.NoError(t, s.Complete(ctx, tok, "admin", goodPassword))

	err := s.Complete(ctx, tok, "second", goodPassword)

	assert.ErrorIs(t, err, auth.ErrSetupClosed)
	assert.Len(t, users.users, 1)
}

func TestCompleteIsSetupClosedWhenAnotherPathCreatedAUserMeanwhile(t *testing.T) {
	users := &fakeUsers{}
	s, tok, _ := newSetup(t, users)
	require.NoError(t, users.Create(context.Background(), core.User{ID: core.NewID(), Username: "other"}))

	err := s.Complete(context.Background(), tok, "admin", goodPassword)

	assert.ErrorIs(t, err, auth.ErrSetupClosed)
	assert.Len(t, users.users, 1)
	assert.False(t, s.Required(), "service notices setup is closed")
}

func TestCompleteRejectsWeakPasswordAndBadUsernameKeepingSetupOpen(t *testing.T) {
	users := &fakeUsers{}
	s, tok, _ := newSetup(t, users)
	ctx := context.Background()

	assert.ErrorIs(t, s.Complete(ctx, tok, "admin", "short"), auth.ErrPasswordTooShort)
	for _, name := range []string{"", " admin", "admin ", "ad\nmin", strings.Repeat("a", 65), "\xff"} {
		assert.ErrorIs(t, s.Complete(ctx, tok, name, goodPassword), auth.ErrInvalidUsername, "username %q", name)
	}

	assert.Empty(t, users.users)
	assert.True(t, s.Required())
	assert.NoError(t, s.Complete(ctx, tok, "admin", goodPassword), "token survives validation failures")
}

func TestSetupTokenAppearsInLogOnceAndPasswordNeverDoes(t *testing.T) {
	users := &fakeUsers{}
	s, tok, buf := newSetup(t, users)
	ctx := context.Background()

	_ = s.Complete(ctx, "wrong-token", "admin", goodPassword)
	_ = s.Complete(ctx, tok, "admin", "short")
	require.NoError(t, s.Complete(ctx, tok, "admin", goodPassword))
	_ = s.Complete(ctx, tok, "admin", goodPassword)

	assert.Equal(t, 1, strings.Count(buf.String(), tok), "token logged once, never again")
	assert.NotContains(t, buf.String(), goodPassword.Reveal())
	assert.NotContains(t, buf.String(), string(users.users[0].PasswordHash))
}

func TestConcurrentCompleteHasExactlyOneWinner(t *testing.T) {
	db, _ := openStore(t)
	users := store.NewUserRepo(db)
	s, tok, _ := newSetup(t, users)
	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			// Distinct usernames: the unique constraint cannot be what saves us.
			errs[i] = s.Complete(context.Background(), tok, fmt.Sprintf("admin%d", i), goodPassword)
		})
	}
	close(start)
	wg.Wait()

	var wins int
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		default:
			assert.ErrorIs(t, err, auth.ErrSetupClosed)
		}
	}
	assert.Equal(t, 1, wins)
	count, err := users.Count(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}
