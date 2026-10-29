package blackbox_test

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/bootstrap"
)

// usageHint ends every message about invalid flags or settings.
const usageHint = "Run 'bitnews --help' for usage.\n"

// setupEnv is the environment of a run that is not meant to serve: valid
// paths inside a temp dir. keep and drop adjust it.
func setupEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	env := baseEnv(t)
	env["BITNEWS_CONFIG_DIR"] = filepath.Join(dir, "config")
	env["BITNEWS_KEY_FILE"] = filepath.Join(dir, "keys", "bitnews.key")
	env["BITNEWS_LISTEN"] = "127.0.0.1:0"
	return env
}

func TestInvalidSettingsExitWithCodeTwoAndTheExactMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		env  map[string]string
		// drop lists environment variables to remove.
		drop []string
		// want is the message after "bitnews: ".
		want string
	}{
		{name: "log level from the environment", env: map[string]string{"BITNEWS_LOG_LEVEL": "verbose"},
			want: `BITNEWS_LOG_LEVEL="verbose": must be debug, info, warn or error`},
		{name: "log level is case sensitive", env: map[string]string{"BITNEWS_LOG_LEVEL": "INFO"},
			want: `BITNEWS_LOG_LEVEL="INFO": must be debug, info, warn or error`},
		{name: "log level from a flag", args: []string{"--log-level", "loud"},
			want: `--log-level="loud": must be debug, info, warn or error`},
		{name: "a flag wins over a valid environment variable", args: []string{"--log-level=loud"}, env: map[string]string{"BITNEWS_LOG_LEVEL": "debug"},
			want: `--log-level="loud": must be debug, info, warn or error`},
		{name: "log format", env: map[string]string{"BITNEWS_LOG_FORMAT": "xml"},
			want: `BITNEWS_LOG_FORMAT="xml": must be text or json`},
		{name: "log format from a flag", args: []string{"--log-format=yaml"},
			want: `--log-format="yaml": must be text or json`},
		{name: "listen without a port", env: map[string]string{"BITNEWS_LISTEN": "no-port"},
			want: `BITNEWS_LISTEN="no-port": must be host:port`},
		{name: "listen with an address and no port", args: []string{"--listen", "0.0.0.0"},
			want: `--listen="0.0.0.0": must be host:port`},
		{name: "listen with an IPv6 address and no port", env: map[string]string{"BITNEWS_LISTEN": "[::1]"},
			want: `BITNEWS_LISTEN="[::1]": must be host:port`},
		{name: "listen with too many colons", env: map[string]string{"BITNEWS_LISTEN": "::1:80"},
			want: `BITNEWS_LISTEN="::1:80": must be host:port`},
		{name: "listen given empty on the command line", args: []string{"--listen="},
			want: `--listen="": must be host:port`},
		{name: "listen with a port name", env: map[string]string{"BITNEWS_LISTEN": "localhost:http"},
			want: `BITNEWS_LISTEN="localhost:http": port must be a number from 0 to 65535`},
		{name: "listen with an empty port", env: map[string]string{"BITNEWS_LISTEN": "localhost:"},
			want: `BITNEWS_LISTEN="localhost:": port must be a number from 0 to 65535`},
		{name: "listen with a port above 65535", args: []string{"--listen", ":70000"},
			want: `--listen=":70000": port must be a number from 0 to 65535`},
		{name: "listen with a negative port", env: map[string]string{"BITNEWS_LISTEN": "127.0.0.1:-1"},
			want: `BITNEWS_LISTEN="127.0.0.1:-1": port must be a number from 0 to 65535`},
		{name: "umask that is not octal", env: map[string]string{"UMASK": "008"},
			want: `UMASK="008": must be an octal mode from 000 to 777`},
		{name: "umask that is not a number", env: map[string]string{"UMASK": "u=rwx"},
			want: `UMASK="u=rwx": must be an octal mode from 000 to 777`},
		{name: "umask above 0777", env: map[string]string{"UMASK": "1000"},
			want: `UMASK="1000": must be an octal mode from 000 to 777`},
		{name: "negative umask", env: map[string]string{"UMASK": "-2"},
			want: `UMASK="-2": must be an octal mode from 000 to 777`},
		{name: "umask with a prefix", env: map[string]string{"UMASK": "0o22"},
			want: `UMASK="0o22": must be an octal mode from 000 to 777`},
		{name: "relative config dir from a flag", args: []string{"--config-dir", "data"},
			want: `--config-dir="data": must be an absolute path`},
		{name: "relative config dir from the environment", env: map[string]string{"BITNEWS_CONFIG_DIR": "./data"},
			want: `BITNEWS_CONFIG_DIR="./data": must be an absolute path`},
		{name: "no config dir and no home to derive one from", drop: []string{"HOME", "BITNEWS_CONFIG_DIR"},
			want: `BITNEWS_CONFIG_DIR (default)=".local/share/bitnews": must be an absolute path`},
		{name: "relative key file", env: map[string]string{"BITNEWS_KEY_FILE": "./bitnews.key"},
			want: `BITNEWS_KEY_FILE="./bitnews.key": must be an absolute path`},
		{name: "key file that is the config dir", args: []string{"--config-dir", "/config", "--key-file", "/config/"},
			want: `--key-file="/config/": must be a file, but it is the config directory "/config"`},
		{name: "relative admin password file", env: map[string]string{"BITNEWS_ADMIN_PASSWORD_FILE": "secrets/admin"},
			want: `BITNEWS_ADMIN_PASSWORD_FILE="secrets/admin": must be an absolute path`},
		{name: "torrent port zero", env: map[string]string{"BITNEWS_TORRENT_PORT": "0"},
			want: `BITNEWS_TORRENT_PORT="0": must be a port number from 1 to 65535`},
		{name: "torrent port above 65535", env: map[string]string{"BITNEWS_TORRENT_PORT": "65536"},
			want: `BITNEWS_TORRENT_PORT="65536": must be a port number from 1 to 65535`},
		{name: "torrent port not a number", env: map[string]string{"BITNEWS_TORRENT_PORT": "51413.5"},
			want: `BITNEWS_TORRENT_PORT="51413.5": must be a port number from 1 to 65535`},
		{name: "the first problem in the order of the table is the one reported",
			env:  map[string]string{"BITNEWS_LISTEN": "nope", "BITNEWS_LOG_LEVEL": "nope", "UMASK": "nope"},
			want: `BITNEWS_LISTEN="nope": must be host:port`},
		{name: "unknown flag", args: []string{"--bogus"},
			want: `flag provided but not defined: -bogus`},
		{name: "unknown single-dash flag", args: []string{"-bogus=1"},
			want: `flag provided but not defined: -bogus`},
		{name: "flag without its value", args: []string{"--log-level"},
			want: `flag needs an argument: -log-level`},
		{name: "positional argument", args: []string{"serve"},
			want: `unexpected argument "serve"`},
		{name: "positional argument after a flag", args: []string{"--log-level=info", "extra", "more"},
			want: `unexpected argument "extra"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := setupEnv(t)
			maps.Copy(env, tt.env)
			for _, k := range tt.drop {
				delete(env, k)
			}

			r := runOnce(t, tt.args, env)

			assert.Equal(t, 2, r.code)
			assert.Equal(t, "bitnews: "+tt.want+"\n"+usageHint, r.stderr)
			assert.Empty(t, r.stdout)
			_, err := os.Stat(env["BITNEWS_CONFIG_DIR"])
			assert.True(t, os.IsNotExist(err) || env["BITNEWS_CONFIG_DIR"] == "", "nothing is created when the settings are invalid")
		})
	}
}

func TestEmptyEnvironmentVariablesAreIgnored(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg, withEnv(
		"BITNEWS_LOG_LEVEL", "", "BITNEWS_LOG_FORMAT", "", "UMASK", "", "BITNEWS_ADMIN_PASSWORD_FILE", "", "BITNEWS_KEY_FILE", ""))

	r := d.stop()

	assert.Contains(t, r.stderr, `level=INFO msg="starting bitnews"`, "the defaults are text format at info level")
}

func TestFlagsWinOverEnvironmentVariablesInEveryForm(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"--listen=%s", "-listen=%s", "--listen %s", "-listen %s"} {
		t.Run(form, func(t *testing.T) {
			t.Parallel()
			cfg := prepareConfigDir(t)
			port := freePort(t)
			addr := fmt.Sprintf("127.0.0.1:%d", port)
			args := strings.Fields(fmt.Sprintf(form, addr))

			d := startDaemon(t, cfg, withArgs(args...), withEnv("BITNEWS_LISTEN", "127.0.0.1:1"), withFlagAddr(addr))

			assert.Equal(t, healthBody, d.get("/api/v1/health", nil).body)
			assert.Equal(t, addr, d.addr)
		})
	}
}

func TestConfigDirAndKeyFileFlagsWinOverTheEnvironment(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	elsewhere := t.TempDir()
	keyElsewhere := filepath.Join(elsewhere, "k", "other.key")

	d := startDaemon(t, cfg,
		withEnv("BITNEWS_CONFIG_DIR", elsewhere, "BITNEWS_KEY_FILE", filepath.Join(elsewhere, "env.key")),
		withArgs("--config-dir", cfg.Dir, "--key-file="+keyElsewhere))
	d.stop()

	assert.FileExists(t, keyElsewhere, "the flag decides where the key is")
	assert.NoFileExists(t, filepath.Join(elsewhere, "env.key"))
	assert.NoFileExists(t, filepath.Join(elsewhere, "bitnews.db"))
}

func TestVersionFlagPrintsTheVersionAndIgnoresInvalidSettings(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--version", "-version"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			env := setupEnv(t)
			env["BITNEWS_LISTEN"] = "not-an-address"
			env["UMASK"] = "999"

			r := runOnce(t, []string{flag}, env)

			assert.Equal(t, 0, r.code)
			assert.Empty(t, r.stderr)
			assert.Regexp(t, regexp.MustCompile(`^bitnews \S+ \(\S+\)\n$`), r.stdout)
			if binary.isGo {
				assert.Equal(t, "bitnews "+builtVersion+" ("+builtCommit+")\n", r.stdout)
			}
		})
	}
}

func TestHelpPrintsTheUsageTextToStdout(t *testing.T) {
	t.Parallel()
	var want bytes.Buffer
	bootstrap.Usage(&want)
	for _, flag := range []string{"--help", "-h", "-help"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			env := setupEnv(t)
			env["BITNEWS_LISTEN"] = "not-an-address" // help comes before validation

			r := runOnce(t, []string{flag}, env)

			assert.Equal(t, 0, r.code)
			assert.Empty(t, r.stderr)
			assert.Equal(t, want.String(), r.stdout)
		})
	}
}

func TestRuntimeStartupFailuresExitWithCodeOne(t *testing.T) {
	t.Parallel()
	help := "Create the key file yourself, for example:\n"
	tests := []struct {
		name  string
		setup func(t *testing.T, env map[string]string)
		// want is the start of the error text on stderr, after the log lines.
		want string
		// wantAlso are substrings of stderr.
		wantAlso []string
		// untouched is a path whose content must be the same afterwards.
		untouched func(env map[string]string) string
	}{
		{
			name: "a key file that is not base64",
			setup: func(t *testing.T, env map[string]string) {
				writeFile(t, env["BITNEWS_KEY_FILE"], "not base64 at all!!\n", 0o600)
			},
			want:      "bitnews: key file %KEY% is invalid: key is not valid base64\n" + help,
			wantAlso:  []string{"Do not replace the key of an existing installation"},
			untouched: func(env map[string]string) string { return env["BITNEWS_KEY_FILE"] },
		},
		{
			name: "a key of the wrong length",
			setup: func(t *testing.T, env map[string]string) {
				writeFile(t, env["BITNEWS_KEY_FILE"], "AAAAAAAAAAAAAAAAAAAAAA==\n", 0o600) // 16 bytes
			},
			want:      "bitnews: key file %KEY% is invalid: key must be 32 bytes, got 16\n" + help,
			untouched: func(env map[string]string) string { return env["BITNEWS_KEY_FILE"] },
		},
		{
			name: "an empty key file",
			setup: func(t *testing.T, env map[string]string) {
				writeFile(t, env["BITNEWS_KEY_FILE"], "", 0o600)
			},
			want:      "bitnews: key file %KEY%: key file is empty\n" + help,
			untouched: func(env map[string]string) string { return env["BITNEWS_KEY_FILE"] },
		},
		{
			name: "a key file that is far too large",
			setup: func(t *testing.T, env map[string]string) {
				writeFile(t, env["BITNEWS_KEY_FILE"], strings.Repeat("A", 2000), 0o600)
			},
			want:      "bitnews: key file %KEY% is too large to be a key\n",
			untouched: func(env map[string]string) string { return env["BITNEWS_KEY_FILE"] },
		},
		{
			name: "a key file that is a directory",
			setup: func(t *testing.T, env map[string]string) {
				require.NoError(t, os.MkdirAll(env["BITNEWS_KEY_FILE"], 0o700))
			},
			want: "bitnews: key file %KEY% is not a regular file\n",
		},
		{
			name: "a config dir that is a regular file",
			setup: func(t *testing.T, env map[string]string) {
				writeFile(t, env["BITNEWS_CONFIG_DIR"], "x", 0o600)
			},
			want: "bitnews: create config dir: ",
		},
		{
			name: "a database that is not a SQLite file",
			setup: func(t *testing.T, env map[string]string) {
				writeFile(t, filepath.Join(env["BITNEWS_CONFIG_DIR"], "bitnews.db"), strings.Repeat("this is not a database\n", 200), 0o600)
			},
			want:      "bitnews: ",
			untouched: func(env map[string]string) string { return filepath.Join(env["BITNEWS_CONFIG_DIR"], "bitnews.db") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := setupEnv(t)
			tt.setup(t, env)
			var before []byte
			if tt.untouched != nil {
				before = readFile(t, tt.untouched(env))
			}

			r := runOnce(t, nil, env)

			assert.Equal(t, 1, r.code, r.stderr)
			assert.Empty(t, r.stdout)
			want := strings.ReplaceAll(tt.want, "%KEY%", fmt.Sprintf("%q", env["BITNEWS_KEY_FILE"]))
			msg := errorText(r.stderr)
			assert.True(t, strings.HasPrefix(msg, want), "the error message starts with %q, got %q", want, msg)
			for _, s := range tt.wantAlso {
				assert.Contains(t, msg, s)
			}
			if tt.untouched != nil {
				assert.Equal(t, before, readFile(t, tt.untouched(env)), "a file that cannot be used is never overwritten")
			}
		})
	}
}

func TestAnInvalidKeyFileStopsTheStartBeforeTheDatabaseIsCreated(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)
	writeFile(t, env["BITNEWS_KEY_FILE"], "garbage\n", 0o600)

	r := runOnce(t, nil, env)

	require.Equal(t, 1, r.code)
	assert.NoFileExists(t, filepath.Join(env["BITNEWS_CONFIG_DIR"], "bitnews.db"))
}

func TestADatabaseFromANewerVersionIsRefused(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	db := openDB(t, cfg)
	_, err := db.Writer().ExecContext(t.Context(),
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, 'from_the_future', 1)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	known := migrationFiles(t)
	before := readFile(t, cfg.DBPath)
	env := serveEnv(t, cfg, freePort(t))

	r := runOnce(t, nil, env)

	assert.Equal(t, 1, r.code)
	assert.Equal(t, fmt.Sprintf("bitnews: store: database schema is newer than this binary (database at migration 9999, binary knows up to %04d); "+
		"upgrade bitnews or restore an older database\n", len(known)), errorText(r.stderr))
	assert.Equal(t, before, readFile(t, cfg.DBPath), "a refused database is not touched")
}

func TestAConfigDirTheUserCannotWriteToIsExplained(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	env := setupEnv(t)
	dir := env["BITNEWS_CONFIG_DIR"]
	require.NoError(t, os.MkdirAll(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	r := runOnce(t, nil, env)

	assert.Equal(t, 1, r.code)
	msg := errorText(r.stderr)
	assert.True(t, strings.HasPrefix(msg, fmt.Sprintf("bitnews: the config dir %q is not writable by uid %d gid %d, the user bitnews runs as. ",
		dir, os.Getuid(), os.Getgid())), msg)
	assert.Contains(t, msg, "sudo mkdir -p <host dir> && sudo chown ")
}

func TestListeningOnAPortThatIsTakenFailsWithCodeOne(t *testing.T) {
	t.Parallel()
	first := startDaemon(t, prepareConfigDir(t))
	env := serveEnv(t, prepareConfigDir(t), first.port)

	r := runOnce(t, nil, env)

	assert.Equal(t, 1, r.code)
	msg := errorText(r.stderr)
	assert.True(t, strings.HasPrefix(msg, "bitnews: "), msg)
	assert.Len(t, splitLines(msg), 1, "one line of error")
}

func TestHealthcheckSubcommandExitsZeroForAHealthyDaemon(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	r := runOnce(t, []string{"healthcheck"}, map[string]string{"PATH": os.Getenv("PATH"), "BITNEWS_LISTEN": d.addr})

	assert.Equal(t, 0, r.code, r.stderr)
	assert.Equal(t, "ok\n", r.stdout)
	assert.Empty(t, r.stderr)
}

func TestHealthcheckSubcommandExitsOneWhenNothingListens(t *testing.T) {
	t.Parallel()

	r := runOnce(t, []string{"healthcheck"}, map[string]string{"PATH": os.Getenv("PATH"), "BITNEWS_LISTEN": fmt.Sprintf("127.0.0.1:%d", freePort(t))})

	assert.Equal(t, 1, r.code)
	assert.Empty(t, r.stdout)
	assert.True(t, strings.HasPrefix(r.stderr, "bitnews healthcheck: "), r.stderr)
}

func TestHealthcheckSubcommandExitsTwoForAnInvalidListenAddress(t *testing.T) {
	t.Parallel()
	for _, listen := range []string{"nope", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:http"} {
		t.Run(listen, func(t *testing.T) {
			t.Parallel()

			r := runOnce(t, []string{"healthcheck"}, map[string]string{"PATH": os.Getenv("PATH"), "BITNEWS_LISTEN": listen})

			assert.Equal(t, 2, r.code)
			assert.Equal(t, fmt.Sprintf("bitnews healthcheck: BITNEWS_LISTEN=%q: must be host:port\n", listen), r.stderr)
		})
	}
}

// errorText is what a failed start printed besides its log lines: the daemon
// logs "starting bitnews" and may log warnings before the error that stops
// it, and the error itself is plain text starting with "bitnews: ". Log lines
// are the ones that start with a time field, in text or in JSON.
func errorText(stderr string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(stderr, "\n") {
		if strings.HasPrefix(l, "time=") || strings.HasPrefix(l, `{"time"`) {
			continue
		}
		b.WriteString(l)
	}
	return b.String()
}

// migrationFiles lists the SQL migrations the Go store embeds: the files
// the rewrite has to apply too.
func migrationFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "store", "migrations"))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
