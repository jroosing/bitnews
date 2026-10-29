package blackbox_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/store"
)

// freshConfigDir describes a config dir that does not exist yet: the binary
// has to create it, the key file and the database.
func freshConfigDir(t *testing.T) configDir {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "nested", "bitnews")
	return configDir{Dir: dir, KeyFile: filepath.Join(dir, "bitnews.key"), DBPath: filepath.Join(dir, "bitnews.db")}
}

// entryNames lists the names in dir.
func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	require.NoError(t, err)
	return st.Mode()
}

func TestAFreshConfigDirGetsAPrivateKeyFileAndDatabaseThatGoCanOpen(t *testing.T) {
	t.Parallel()
	cfg := freshConfigDir(t)
	d := startDaemon(t, cfg)
	require.Equal(t, healthBody, d.get("/api/v1/health", nil).body)

	r := d.stop()

	// What the binary created, and with which modes.
	assert.Equal(t, fs.ModeDir|0o700, modeOf(t, cfg.Dir), "the config dir is private")
	assert.Equal(t, fs.ModeDir|0o700, modeOf(t, filepath.Dir(cfg.Dir)), "so are the parents it had to create")
	assert.Equal(t, fs.FileMode(0o600), modeOf(t, cfg.KeyFile))
	assert.Equal(t, fs.FileMode(0o600), modeOf(t, cfg.DBPath))
	assert.Equal(t, []string{"bitnews.db", "bitnews.key"}, entryNames(t, cfg.Dir),
		"after a clean shutdown nothing else is left: no WAL, shared memory or temporary key file")

	// The key file: 32 random bytes in base64, one newline.
	keyText := string(readFile(t, cfg.KeyFile))
	assert.Len(t, keyText, 45)
	assert.True(t, strings.HasSuffix(keyText, "\n"))
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(keyText, "\n"))
	require.NoError(t, err)
	assert.Len(t, raw, 32)
	assert.False(t, bytes.Equal(raw, make([]byte, 32)), "the key is random")
	_, err = store.ParseKey(strings.TrimSpace(keyText))
	assert.NoError(t, err, "Go accepts the key")
	assert.Contains(t, r.stderr, "generated a new master key for secret encryption")
	assert.Contains(t, r.stderr, "path="+cfg.KeyFile)

	// Go opens the database: every migration is applied, in order.
	db := openDB(t, cfg)
	defer func() { require.NoError(t, db.Close()) }()
	rows, err := db.Reader().QueryContext(t.Context(), `SELECT version, name FROM schema_migrations ORDER BY version`)
	require.NoError(t, err)
	var got []string
	for i := 1; rows.Next(); i++ {
		var version int
		var name string
		require.NoError(t, rows.Scan(&version, &name))
		assert.Equal(t, i, version, "the versions are contiguous from 1")
		got = append(got, fmt.Sprintf("%04d_%s.sql", version, name))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Equal(t, migrationFiles(t), got, "the same migrations, under the same names")

	var mode, check string
	require.NoError(t, db.Reader().QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&mode))
	assert.Equal(t, "wal", mode, "the database file is in WAL mode")
	require.NoError(t, db.Reader().QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&check))
	assert.Equal(t, "ok", check)

	// The app key the binary made is readable and well formed.
	key := storedAppKey(t, cfg)
	kind, ok := auth.KindOf(key)
	assert.True(t, ok && kind == auth.KindAppKey, "the app key is shaped like one: %q", key)
	assert.True(t, strings.HasPrefix(key, "bitnews_"))
	assert.NotContains(t, r.stderr, key, "the app key is not logged")
}

func TestARestartKeepsTheMasterKeyTheAppKeyAndTheDatabase(t *testing.T) {
	t.Parallel()
	cfg := freshConfigDir(t)
	startDaemon(t, cfg).stop()
	keyBefore, appKeyBefore := readFile(t, cfg.KeyFile), storedAppKey(t, cfg)

	d := startDaemon(t, cfg)
	assert.Equal(t, http.StatusServiceUnavailable, d.get("/api/v1/version", bearer(appKeyBefore)).status, "still in setup mode: nobody created an admin")
	r := d.stop()

	assert.Equal(t, keyBefore, readFile(t, cfg.KeyFile), "the key file is never regenerated")
	assert.Equal(t, appKeyBefore, storedAppKey(t, cfg))
	assert.NotContains(t, r.stderr, "generated a new master key")
	assert.Equal(t, []string{"bitnews.db", "bitnews.key"}, entryNames(t, cfg.Dir))
}

func TestTheKeyFileCanLiveOutsideTheConfigDir(t *testing.T) {
	t.Parallel()
	cfg := freshConfigDir(t)
	keyFile := filepath.Join(t.TempDir(), "secrets", "deeper", "master.key")

	d := startDaemon(t, cfg, withEnv("BITNEWS_KEY_FILE", keyFile))
	d.stop()

	assert.Equal(t, fs.FileMode(0o600), modeOf(t, keyFile))
	assert.Equal(t, fs.ModeDir|0o700, modeOf(t, filepath.Dir(keyFile)), "its directory is created private")
	assert.Equal(t, []string{"bitnews.db"}, entryNames(t, cfg.Dir), "the config dir holds only the database")
	assert.Equal(t, []string{"master.key"}, entryNames(t, filepath.Dir(keyFile)))
}

func TestTheUmaskNeverWidensTheModeOfPrivateFiles(t *testing.T) {
	t.Parallel()
	for _, umask := range []string{"000", "022", "027", "077"} {
		t.Run(umask, func(t *testing.T) {
			t.Parallel()
			cfg := freshConfigDir(t)
			d := startDaemon(t, cfg, withEnv("UMASK", umask))
			d.get("/api/v1/health", nil)

			// While it runs the database has its WAL and shared memory
			// files next to it.
			for _, name := range entryNames(t, cfg.Dir) {
				assert.Zero(t, modeOf(t, filepath.Join(cfg.Dir, name)).Perm()&0o077, "%s is private while the daemon runs", name)
			}
			d.stop()

			assert.Equal(t, fs.ModeDir|0o700, modeOf(t, cfg.Dir))
			assert.Equal(t, fs.FileMode(0o600), modeOf(t, cfg.KeyFile))
			assert.Equal(t, fs.FileMode(0o600), modeOf(t, cfg.DBPath))
		})
	}
}

func TestTheUmaskIsAppliedToTheWholeProcess(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("needs /proc to read the umask of another process")
	}
	umaskLine := regexp.MustCompile(`(?m)^Umask:\s+(\d+)$`)
	for umask, want := range map[string]string{"": "0002", "027": "0027", "0": "0000"} {
		t.Run("UMASK="+umask, func(t *testing.T) {
			t.Parallel()
			cfg := freshConfigDir(t)
			opts := []startOption{}
			if umask != "" {
				opts = append(opts, withEnv("UMASK", umask))
			}
			d := startDaemon(t, cfg, opts...)

			status := readFile(t, fmt.Sprintf("/proc/%d/status", d.cmd.Process.Pid))

			m := umaskLine.FindSubmatch(status)
			require.NotNil(t, m, "no Umask line in /proc status")
			assert.Equal(t, want, string(m[1]), "the default umask is 002")
		})
	}
}

func TestExistingFilesWithBroaderModesAreWarnedAboutAndLeftAlone(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	require.NoError(t, os.Chmod(cfg.KeyFile, 0o644)) // as a Docker secret often is
	require.NoError(t, os.Chmod(cfg.DBPath, 0o640))
	d := startDaemon(t, cfg)

	r := d.stop()

	assert.Equal(t, fs.FileMode(0o644), modeOf(t, cfg.KeyFile), "a key file is never chmod-ed")
	assert.Equal(t, fs.FileMode(0o640), modeOf(t, cfg.DBPath), "nor is the database")
	assert.Contains(t, r.stderr, `level=WARN msg="key file is accessible by other users; consider chmod 600" path=`+cfg.KeyFile+` mode=0644`)
	assert.Contains(t, r.stderr, `level=WARN msg="database file permissions are broader than 0600" path=`+cfg.DBPath+` mode=0640`)
}

func TestPrivateFilesCauseNoPermissionWarnings(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	r := d.stop()

	assert.NotContains(t, r.stderr, "level=WARN", "an existing, private config dir starts without warnings")
}
