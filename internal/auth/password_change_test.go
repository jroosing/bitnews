package auth_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

const newPassword = core.Secret("a brand new passphrase")

func TestChangePasswordWithTheRightCurrentPasswordSwitchesLogins(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()

	_, err := env.svc.ChangePassword(ctx, env.user.ID, goodPassword, newPassword, "", loginIP)

	require.NoError(t, err)
	_, err = env.svc.Login(ctx, "admin", newPassword, loginIP)
	assert.NoError(t, err, "new password logs in")
	_, err = env.svc.Login(ctx, "admin", goodPassword, loginIP)
	assert.ErrorIs(t, err, auth.ErrInvalidCredentials, "old password is gone")
}

func TestChangePasswordRejectsAWrongCurrentPasswordAndChangesNothing(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()

	_, err := env.svc.ChangePassword(ctx, env.user.ID, "not the password!!", newPassword, "", loginIP)

	assert.ErrorIs(t, err, auth.ErrInvalidCredentials)
	_, err = env.svc.Login(ctx, "admin", goodPassword, loginIP)
	assert.NoError(t, err, "old password still works")
}

func TestChangePasswordEnforcesTheLengthRulesOnTheNewPassword(t *testing.T) {
	env := newSessionEnv(t)

	_, short := env.svc.ChangePassword(context.Background(), env.user.ID, goodPassword, "too short", "", loginIP)
	_, long := env.svc.ChangePassword(context.Background(), env.user.ID, goodPassword, core.Secret(string(make([]byte, auth.MaxPasswordBytes+1))), "", loginIP)

	assert.ErrorIs(t, short, auth.ErrPasswordTooShort)
	assert.ErrorIs(t, long, auth.ErrPasswordTooLong)
}

func TestChangePasswordEndsEveryOtherSessionButKeepsTheCurrentOne(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()
	current, _, _ := env.svc.CreateSession(ctx, env.user.ID)
	other, _, _ := env.svc.CreateSession(ctx, env.user.ID)

	n, err := env.svc.ChangePassword(ctx, env.user.ID, goodPassword, newPassword, current, loginIP)

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, _, err = env.svc.Authenticate(ctx, current)
	assert.NoError(t, err)
	_, _, err = env.svc.Authenticate(ctx, other)
	assert.ErrorIs(t, err, auth.ErrInvalidSession)
}

func TestChangePasswordFailuresCountAgainstTheLoginLimiter(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()
	for range 5 {
		_, _ = env.svc.ChangePassword(ctx, env.user.ID, "not the password!!", newPassword, "", loginIP)
	}

	_, err := env.svc.ChangePassword(ctx, env.user.ID, goodPassword, newPassword, "", loginIP)

	var rl *auth.RateLimitError
	require.ErrorAs(t, err, &rl, "right password, still held back")
	assert.Positive(t, rl.RetryAfter)
}

func TestChangePasswordStoresAHashWithTheCurrentParameters(t *testing.T) {
	env := newSessionEnv(t)
	ctx := context.Background()

	_, err := env.svc.ChangePassword(ctx, env.user.ID, goodPassword, newPassword, "", loginIP)

	require.NoError(t, err)
	u, err := env.users.Get(ctx, env.user.ID)
	require.NoError(t, err)
	assert.False(t, auth.NeedsRehash(u.PasswordHash))
}

func TestChangePasswordForAnUnknownUserIsNotFound(t *testing.T) {
	env := newSessionEnv(t)

	_, err := env.svc.ChangePassword(context.Background(), core.NewID(), goodPassword, newPassword, "", loginIP)

	assert.ErrorIs(t, err, core.ErrNotFound)
}
