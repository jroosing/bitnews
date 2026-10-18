package auth_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func writePasswordFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin_password")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestBootstrapAdminFromFileCreatesAdminAndClosesSetup(t *testing.T) {
	auth.UseFastParams(t)
	ctx := context.Background()
	users := &fakeUsers{}
	logger, buf := newLogger()
	path := writePasswordFile(t, "file based passphrase\n", 0o600)

	created, err := auth.BootstrapAdminFromFile(ctx, path, users, logger)

	require.NoError(t, err)
	assert.True(t, created)
	require.Len(t, users.users, 1)
	assert.Equal(t, "admin", users.users[0].Username)
	ok, err := auth.VerifyPassword(users.users[0].PasswordHash, "file based passphrase")
	require.NoError(t, err)
	assert.True(t, ok, "single trailing newline is trimmed")
	assert.NotContains(t, buf.String(), "file based passphrase")

	setupLogger, setupBuf := newLogger()
	s, err := auth.NewSetup(ctx, users, setupLogger)
	require.NoError(t, err)
	assert.False(t, s.Required(), "setup closed afterwards")
	assert.NotContains(t, setupBuf.String(), "setup_token")
}

func TestBootstrapAdminFromFileTrimsOnlyOneTrailingNewline(t *testing.T) {
	auth.UseFastParams(t)
	users := &fakeUsers{}
	logger, _ := newLogger()
	path := writePasswordFile(t, "passphrase with spaces \n\n", 0o600)

	_, err := auth.BootstrapAdminFromFile(context.Background(), path, users, logger)

	require.NoError(t, err)
	ok, err := auth.VerifyPassword(users.users[0].PasswordHash, "passphrase with spaces \n")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestBootstrapAdminFromFileTrimsCRLF(t *testing.T) {
	auth.UseFastParams(t)
	users := &fakeUsers{}
	logger, _ := newLogger()
	path := writePasswordFile(t, "windows passphrase\r\n", 0o600)

	_, err := auth.BootstrapAdminFromFile(context.Background(), path, users, logger)

	require.NoError(t, err)
	ok, _ := auth.VerifyPassword(users.users[0].PasswordHash, "windows passphrase")
	assert.True(t, ok)
}

func TestBootstrapAdminFromFileWithExistingUserDoesNothingAndDoesNotReadFile(t *testing.T) {
	users := &fakeUsers{users: []core.User{{ID: core.NewID(), Username: "someone"}}}
	logger, _ := newLogger()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	created, err := auth.BootstrapAdminFromFile(context.Background(), missing, users, logger)

	require.NoError(t, err, "a missing file is irrelevant once a user exists")
	assert.False(t, created)
	assert.Len(t, users.users, 1)
}

func TestBootstrapAdminFromFileRejectsBadFiles(t *testing.T) {
	logger, buf := newLogger()
	dir := t.TempDir()
	cases := map[string]struct {
		path string
		want error
	}{
		"empty":          {writePasswordFile(t, "", 0o600), auth.ErrPasswordTooShort},
		"only newline":   {writePasswordFile(t, "\n", 0o600), auth.ErrPasswordTooShort},
		"too short":      {writePasswordFile(t, "short\n", 0o600), auth.ErrPasswordTooShort},
		"too long":       {writePasswordFile(t, strings.Repeat("a", 4097), 0o600), auth.ErrAdminPasswordFileTooLarge},
		"over 1024 pass": {writePasswordFile(t, strings.Repeat("a", 2000), 0o600), auth.ErrPasswordTooLong},
		"missing":        {filepath.Join(dir, "missing"), os.ErrNotExist},
		"directory":      {dir, auth.ErrAdminPasswordFileNotRegular},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			users := &fakeUsers{}

			created, err := auth.BootstrapAdminFromFile(context.Background(), tc.path, users, logger)

			require.ErrorIs(t, err, tc.want)
			assert.False(t, created)
			assert.Empty(t, users.users)
			assert.NotContains(t, err.Error(), "aaaa", "error never echoes the content")
		})
	}
	assert.NotContains(t, buf.String(), "aaaa")
}

func TestBootstrapAdminFromFileAcceptsExactly4KiBFileWhenPasswordIsValid(t *testing.T) {
	// A 4096-byte file is within the cap, but the password limit (1024) still applies.
	logger, _ := newLogger()
	path := writePasswordFile(t, strings.Repeat("a", 4096), 0o600)

	_, err := auth.BootstrapAdminFromFile(context.Background(), path, &fakeUsers{}, logger)

	assert.ErrorIs(t, err, auth.ErrPasswordTooLong)
}

func TestBootstrapAdminFromFileWarnsWhenFileIsGroupOrWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	auth.UseFastParams(t)
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		logger, buf := newLogger()
		path := writePasswordFile(t, "file based passphrase", mode)

		_, err := auth.BootstrapAdminFromFile(context.Background(), path, &fakeUsers{}, logger)

		require.NoError(t, err, "warn, not fail")
		var warned bool
		for _, r := range buf.records(t) {
			if r["level"] == "WARN" {
				warned = true
			}
		}
		assert.True(t, warned, "mode %o", mode)
		assert.NotContains(t, buf.String(), "file based passphrase")
	}
}

func TestBootstrapAdminFromFileDoesNotWarnForOwnerOnlyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	auth.UseFastParams(t)
	logger, buf := newLogger()
	path := writePasswordFile(t, "file based passphrase", 0o400)

	_, err := auth.BootstrapAdminFromFile(context.Background(), path, &fakeUsers{}, logger)

	require.NoError(t, err)
	for _, r := range buf.records(t) {
		assert.NotEqual(t, "WARN", r["level"])
	}
}
