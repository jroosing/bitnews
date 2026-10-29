package blackbox_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// bootstrapPassword is a password that passes the password rules (12 or more
// characters).
const bootstrapPassword = "bootstrap pässword 123"

func TestAdminPasswordFileCreatesTheAdminOnAFreshInstanceThatGoCanVerify(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"no trailing newline": bootstrapPassword,
		"one trailing LF":     bootstrapPassword + "\n",
		"one trailing CRLF":   bootstrapPassword + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := prepareConfigDir(t, withoutAdmin())
			pwFile := filepath.Join(t.TempDir(), "admin_password")
			writeFile(t, pwFile, content, 0o600)
			d := startDaemon(t, cfg, withEnv("BITNEWS_ADMIN_PASSWORD_FILE", pwFile))

			// Setup is closed from the first request on: no gate, no token.
			assert.Equal(t, http.StatusOK, d.get("/api/v1/version", bearer(cfg.AppKey)).status)
			r := d.stop()

			assert.Contains(t, r.stderr, `level=INFO msg="admin account created from the admin password file" username=admin path=`+pwFile)
			assert.NotContains(t, r.stderr, "first-run setup required")
			assert.NotContains(t, r.stderr, "setup_token")
			assert.NotContains(t, r.stderr, bootstrapPassword, "the password is never logged")

			db := openDB(t, cfg)
			defer func() { require.NoError(t, db.Close()) }()
			users := store.NewUserRepo(db)
			n, err := users.Count(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			u, err := users.GetByUsername(t.Context(), auth.BootstrapAdminUsername)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(string(u.PasswordHash), "$argon2id$v=19$"), "an argon2id hash in PHC format: %s", u.PasswordHash)
			ok, err := auth.VerifyPassword(u.PasswordHash, core.Secret(bootstrapPassword))
			require.NoError(t, err)
			assert.True(t, ok, "Go verifies the hash the binary made")
			ok, err = auth.VerifyPassword(u.PasswordHash, core.Secret(bootstrapPassword+"\n"))
			require.NoError(t, err)
			assert.False(t, ok, "only one trailing newline was trimmed")
		})
	}
}

func TestAdminPasswordFileIsNotTouchedWhenAUserAlreadyExists(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	d := startDaemon(t, cfg, withEnv("BITNEWS_ADMIN_PASSWORD_FILE", missing))

	assert.Equal(t, http.StatusOK, d.get("/api/v1/version", bearer(cfg.AppKey)).status)
	r := d.stop()

	assert.NotContains(t, r.stderr, "admin account created")
}

func TestAdminPasswordFileWithAWeakPasswordStopsTheStart(t *testing.T) {
	t.Parallel()
	pwFile := filepath.Join(t.TempDir(), "pw")
	writeFile(t, pwFile, "short\n", 0o600)

	r, cfg := runWithAdminFile(t, pwFile)

	assert.Equal(t, 1, r.code)
	assert.Equal(t, "bitnews: auth: admin password file "+pwFile+": auth: password must be at least 12 characters\n", errorText(r.stderr))
	assertNoUsers(t, cfg)
}

func TestAdminPasswordFileThatIsTooLargeStopsTheStart(t *testing.T) {
	t.Parallel()
	pwFile := filepath.Join(t.TempDir(), "pw")
	writeFile(t, pwFile, strings.Repeat("a", 4097), 0o600)

	r, cfg := runWithAdminFile(t, pwFile)

	assert.Equal(t, 1, r.code)
	assert.Equal(t, "bitnews: auth: admin password file "+pwFile+": auth: admin password file is larger than 4 KiB\n", errorText(r.stderr))
	assertNoUsers(t, cfg)
}

func TestAdminPasswordFileThatIsNotAFileStopsTheStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	r, cfg := runWithAdminFile(t, dir)

	assert.Equal(t, 1, r.code)
	assert.Equal(t, "bitnews: auth: admin password file "+dir+": auth: admin password file is not a regular file\n", errorText(r.stderr))
	assertNoUsers(t, cfg)
}

func TestAdminPasswordFileThatDoesNotExistStopsTheStart(t *testing.T) {
	t.Parallel()
	pwFile := filepath.Join(t.TempDir(), "missing")

	r, cfg := runWithAdminFile(t, pwFile)

	assert.Equal(t, 1, r.code)
	assert.True(t, strings.HasPrefix(errorText(r.stderr), "bitnews: auth: admin password file: "), r.stderr)
	assertNoUsers(t, cfg)
}

func TestAdminPasswordFileReadableByOthersIsWarnedAbout(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t, withoutAdmin())
	pwFile := filepath.Join(t.TempDir(), "pw")
	writeFile(t, pwFile, bootstrapPassword, 0o644)
	require.NoError(t, os.Chmod(pwFile, 0o644))
	d := startDaemon(t, cfg, withEnv("BITNEWS_ADMIN_PASSWORD_FILE", pwFile))

	r := d.stop()

	assert.Contains(t, r.stderr, `level=WARN msg="admin password file is readable by group or others; restrict it to the owner (chmod 600)" path=`+pwFile+` mode=0644`)
	assert.Contains(t, r.stderr, "admin account created from the admin password file", "it is only a warning")
	assert.NotContains(t, r.stderr, bootstrapPassword)
}

// runWithAdminFile runs the binary once on a database without users and the
// given admin password file.
func runWithAdminFile(t *testing.T, pwFile string) (result, configDir) {
	t.Helper()
	cfg := prepareConfigDir(t, withoutAdmin())
	env := serveEnv(t, cfg, freePort(t))
	env["BITNEWS_ADMIN_PASSWORD_FILE"] = pwFile
	return runOnce(t, nil, env), cfg
}

func assertNoUsers(t *testing.T, cfg configDir) {
	t.Helper()
	db := openDB(t, cfg)
	defer func() { require.NoError(t, db.Close()) }()
	n, err := store.NewUserRepo(db).Count(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n, "a failed start creates no admin")
}
