package auth_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

var loginIP = netip.MustParseAddr("192.0.2.10")

func TestLoginWithRightPasswordReturnsAUsableSessionToken(t *testing.T) {
	env := newSessionEnv(t)

	token, err := env.svc.Login(context.Background(), "admin", goodPassword, loginIP)

	require.NoError(t, err)
	_, u, err := env.svc.Authenticate(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, env.user.ID, u.ID)
}

func TestLoginRejectsWrongPasswordAndUnknownUserWithTheSameError(t *testing.T) {
	env := newSessionEnv(t)

	_, errWrong := env.svc.Login(context.Background(), "admin", "wrong password!!", loginIP)
	_, errUnknown := env.svc.Login(context.Background(), "nobody", goodPassword, loginIP)

	assert.ErrorIs(t, errWrong, auth.ErrInvalidCredentials)
	assert.ErrorIs(t, errUnknown, auth.ErrInvalidCredentials)
	assert.Equal(t, errWrong.Error(), errUnknown.Error(), "no user enumeration through the message")
	assert.Zero(t, env.sessions.count())
}

// recordingVerifier wraps the real verifier and records every hash checked.
type recordingVerifier struct {
	mu     sync.Mutex
	hashes [][]byte
}

func (r *recordingVerifier) verify(hash []byte, pw core.Secret) (bool, error) {
	r.mu.Lock()
	r.hashes = append(r.hashes, bytes.Clone(hash))
	r.mu.Unlock()
	return auth.VerifyPassword(hash, pw)
}

func TestLoginRunsOneArgonVerifyForAnUnknownUserAgainstADummyHash(t *testing.T) {
	rec := &recordingVerifier{}
	env := newSessionEnv(t, auth.WithVerifier(rec.verify))

	_, _ = env.svc.Login(context.Background(), "admin", "wrong password!!", loginIP)
	_, _ = env.svc.Login(context.Background(), "nobody", "wrong password!!", loginIP)

	require.Len(t, rec.hashes, 2, "one verify per attempt, known user or not")
	assert.Equal(t, env.user.PasswordHash, rec.hashes[0])
	dummy := rec.hashes[1]
	assert.NotEqual(t, env.user.PasswordHash, dummy)
	assert.False(t, auth.NeedsRehash(dummy), "dummy hash costs the same as a real one")
}

func TestLoginBlockedByTheLimiterSkipsPasswordVerification(t *testing.T) {
	rec := &recordingVerifier{}
	env := newSessionEnv(t, auth.WithVerifier(rec.verify))
	ctx := context.Background()
	for range 5 {
		_, _ = env.svc.Login(ctx, "admin", "wrong password!!", loginIP)
	}
	checked := len(rec.hashes)

	_, err := env.svc.Login(ctx, "admin", goodPassword, loginIP) // right password, still blocked

	var rl *auth.RateLimitError
	require.ErrorAs(t, err, &rl)
	assert.ErrorIs(t, err, auth.ErrRateLimited)
	assert.Equal(t, time.Second, rl.RetryAfter)
	assert.Len(t, rec.hashes, checked, "no argon2 work while blocked")
}

func TestLoginBackoffGrowsAndClearsAfterASuccess(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()
	for range 5 {
		_, _ = env.svc.Login(ctx, "admin", "wrong password!!", loginIP)
	}
	env.clock.Advance(time.Second)

	_, err := env.svc.Login(ctx, "admin", "wrong password!!", loginIP) // 6th failure: 2 s
	assert.ErrorIs(t, err, auth.ErrInvalidCredentials)
	_, err = env.svc.Login(ctx, "admin", goodPassword, loginIP)
	var rl *auth.RateLimitError
	require.ErrorAs(t, err, &rl)
	assert.Equal(t, 2*time.Second, rl.RetryAfter)

	env.clock.Advance(2 * time.Second)
	_, err = env.svc.Login(ctx, "admin", goodPassword, loginIP)
	require.NoError(t, err)
	for range 4 {
		_, err = env.svc.Login(ctx, "admin", "wrong password!!", loginIP)
		assert.ErrorIs(t, err, auth.ErrInvalidCredentials, "counter was reset by the success")
	}
}

func TestLoginRateLimitsAUsernameAcrossIPs(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()
	for i := range 5 {
		ip := netip.AddrFrom4([4]byte{198, 51, 100, byte(i + 1)})
		_, _ = env.svc.Login(ctx, "admin", "wrong password!!", ip)
	}

	_, err := env.svc.Login(ctx, "admin", goodPassword, netip.MustParseAddr("203.0.113.77"))

	assert.ErrorIs(t, err, auth.ErrRateLimited)
}

func TestLoginRehashesAnOutdatedHashAfterASuccessfulVerify(t *testing.T) {
	env := newSessionEnv(t)
	old := env.user.PasswordHash
	auth.SetParamsForTest(t, auth.Params{Memory: 16, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	require.True(t, auth.NeedsRehash(old), "parameters moved on since the hash was made")

	_, err := env.svc.Login(context.Background(), "admin", goodPassword, loginIP)

	require.NoError(t, err)
	u, _ := env.users.Get(context.Background(), env.user.ID)
	assert.NotEqual(t, old, u.PasswordHash)
	assert.False(t, auth.NeedsRehash(u.PasswordHash))
	ok, err := auth.VerifyPassword(u.PasswordHash, goodPassword)
	require.NoError(t, err)
	assert.True(t, ok, "the new hash accepts the same password")
}

func TestLoginDoesNotRehashAfterAFailedVerify(t *testing.T) {
	env := newSessionEnv(t)
	auth.SetParamsForTest(t, auth.Params{Memory: 16, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})

	_, _ = env.svc.Login(context.Background(), "admin", "wrong password!!", loginIP)

	u, _ := env.users.Get(context.Background(), env.user.ID)
	assert.Equal(t, env.user.PasswordHash, u.PasswordHash)
}

func TestLoginKeepsTheHashWhenItIsUpToDate(t *testing.T) {
	env := newSessionEnv(t)

	_, err := env.svc.Login(context.Background(), "admin", goodPassword, loginIP)

	require.NoError(t, err)
	u, _ := env.users.Get(context.Background(), env.user.ID)
	assert.Equal(t, env.user.PasswordHash, u.PasswordHash)
}

func TestLoginTreatsACorruptStoredHashAsAFailure(t *testing.T) {
	env := newSessionEnv(t)
	require.NoError(t, env.users.UpdatePasswordHash(context.Background(), env.user.ID, []byte("garbage")))

	_, err := env.svc.Login(context.Background(), "admin", goodPassword, loginIP)

	assert.ErrorIs(t, err, auth.ErrInvalidCredentials)
}

func TestLoginRejectsOversizedPasswordWithoutHashing(t *testing.T) {
	rec := &recordingVerifier{}
	env := newSessionEnv(t, auth.WithVerifier(rec.verify))

	_, err := env.svc.Login(context.Background(), "admin", core.Secret(string(make([]byte, auth.MaxPasswordBytes+1))), loginIP)

	assert.ErrorIs(t, err, auth.ErrInvalidCredentials)
	assert.Empty(t, rec.hashes)
}

func TestLoginLogsFailuresWithoutThePassword(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	env := newSessionEnv(t, auth.WithSessionLogger(logger))

	_, _ = env.svc.Login(context.Background(), "admin", "super-secret-guess", loginIP)

	out := buf.String()
	assert.Contains(t, out, "login failed")
	assert.Contains(t, out, "admin")
	assert.Contains(t, out, "192.0.2.10")
	assert.NotContains(t, out, "super-secret-guess")
}
