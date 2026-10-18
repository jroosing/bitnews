package bootstrap_test

import (
	"io/fs"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/bootstrap"
)

// env builds a getenv func backed by a map.
func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// devEnv is a minimal environment that yields valid defaults.
func devEnv() map[string]string {
	return map[string]string{
		"XDG_DATA_HOME":   "/home/u/.local/share",
		"XDG_CONFIG_HOME": "/home/u/.config",
		"HOME":            "/home/u",
	}
}

func TestLoadDefaultsPathsFromXDGDirectories(t *testing.T) {
	cfg, err := bootstrap.Load(nil, env(devEnv()))
	require.NoError(t, err)
	assert.Equal(t, "/home/u/.local/share/bitnews", cfg.ConfigDir)
	assert.Equal(t, "/home/u/.config/bitnews/bitnews.key", cfg.KeyFile)
}

func TestLoadFallsBackToHomeWhenXDGUnsetOrRelative(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"unset":    {"HOME": "/home/u"},
		"empty":    {"HOME": "/home/u", "XDG_DATA_HOME": "", "XDG_CONFIG_HOME": ""},
		"relative": {"HOME": "/home/u", "XDG_DATA_HOME": "rel/share", "XDG_CONFIG_HOME": "rel/config"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := bootstrap.Load(nil, env(vars))
			require.NoError(t, err)
			assert.Equal(t, "/home/u/.local/share/bitnews", cfg.ConfigDir)
			assert.Equal(t, "/home/u/.config/bitnews/bitnews.key", cfg.KeyFile)
		})
	}
}

func TestLoadPrecedenceFlagOverEnvOverDefault(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		get  func(bootstrap.Config) string
		want string
	}{
		{
			name: "config-dir from env",
			env:  map[string]string{"BITNEWS_CONFIG_DIR": "/srv/env"},
			get:  func(c bootstrap.Config) string { return c.ConfigDir },
			want: "/srv/env",
		},
		{
			name: "config-dir flag wins over env",
			args: []string{"--config-dir", "/srv/flag"},
			env:  map[string]string{"BITNEWS_CONFIG_DIR": "/srv/env"},
			get:  func(c bootstrap.Config) string { return c.ConfigDir },
			want: "/srv/flag",
		},
		{
			name: "config-dir empty env falls back to default",
			env:  map[string]string{"BITNEWS_CONFIG_DIR": ""},
			get:  func(c bootstrap.Config) string { return c.ConfigDir },
			want: "/home/u/.local/share/bitnews",
		},
		{
			name: "key-file from env",
			env:  map[string]string{"BITNEWS_KEY_FILE": "/run/secrets/env.key"},
			get:  func(c bootstrap.Config) string { return c.KeyFile },
			want: "/run/secrets/env.key",
		},
		{
			name: "key-file flag wins over env",
			args: []string{"--key-file=/etc/bitnews/flag.key"},
			env:  map[string]string{"BITNEWS_KEY_FILE": "/run/secrets/env.key"},
			get:  func(c bootstrap.Config) string { return c.KeyFile },
			want: "/etc/bitnews/flag.key",
		},
		{
			name: "listen default",
			get:  func(c bootstrap.Config) string { return c.Listen },
			want: "0.0.0.0:9494",
		},
		{
			name: "listen from env",
			env:  map[string]string{"BITNEWS_LISTEN": "127.0.0.1:8080"},
			get:  func(c bootstrap.Config) string { return c.Listen },
			want: "127.0.0.1:8080",
		},
		{
			name: "listen flag wins over env",
			args: []string{"--listen", "[::1]:9000"},
			env:  map[string]string{"BITNEWS_LISTEN": "127.0.0.1:8080"},
			get:  func(c bootstrap.Config) string { return c.Listen },
			want: "[::1]:9000",
		},
		{
			name: "log-level default",
			get:  func(c bootstrap.Config) string { return c.LogLevel.String() },
			want: "INFO",
		},
		{
			name: "log-level from env",
			env:  map[string]string{"BITNEWS_LOG_LEVEL": "debug"},
			get:  func(c bootstrap.Config) string { return c.LogLevel.String() },
			want: "DEBUG",
		},
		{
			name: "log-level flag wins over env",
			args: []string{"--log-level", "error"},
			env:  map[string]string{"BITNEWS_LOG_LEVEL": "debug"},
			get:  func(c bootstrap.Config) string { return c.LogLevel.String() },
			want: "ERROR",
		},
		{
			name: "log-level warn",
			args: []string{"--log-level", "warn"},
			get:  func(c bootstrap.Config) string { return c.LogLevel.String() },
			want: "WARN",
		},
		{
			name: "log-format default",
			get:  func(c bootstrap.Config) string { return string(c.LogFormat) },
			want: "text",
		},
		{
			name: "log-format from env",
			env:  map[string]string{"BITNEWS_LOG_FORMAT": "json"},
			get:  func(c bootstrap.Config) string { return string(c.LogFormat) },
			want: "json",
		},
		{
			name: "log-format flag wins over env",
			args: []string{"--log-format", "text"},
			env:  map[string]string{"BITNEWS_LOG_FORMAT": "json"},
			get:  func(c bootstrap.Config) string { return string(c.LogFormat) },
			want: "text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := devEnv()
			maps.Copy(vars, tt.env)
			cfg, err := bootstrap.Load(tt.args, env(vars))
			require.NoError(t, err)
			assert.Equal(t, tt.want, tt.get(cfg))
		})
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		// wantErr are substrings the error must contain; the first names
		// the offending flag or env var.
		wantErr []string
	}{
		{
			name:    "unknown log level from flag",
			args:    []string{"--log-level", "verbose"},
			wantErr: []string{"--log-level", "verbose"},
		},
		{
			name:    "unknown log level from env",
			env:     map[string]string{"BITNEWS_LOG_LEVEL": "INFO2"},
			wantErr: []string{"BITNEWS_LOG_LEVEL", "INFO2"},
		},
		{
			name:    "unknown log format",
			env:     map[string]string{"BITNEWS_LOG_FORMAT": "xml"},
			wantErr: []string{"BITNEWS_LOG_FORMAT", "xml"},
		},
		{
			name:    "listen without port",
			args:    []string{"--listen", "0.0.0.0"},
			wantErr: []string{"--listen", "0.0.0.0"},
		},
		{
			name:    "listen with non-numeric port",
			env:     map[string]string{"BITNEWS_LISTEN": "localhost:http"},
			wantErr: []string{"BITNEWS_LISTEN", "localhost:http"},
		},
		{
			name:    "listen with out-of-range port",
			args:    []string{"--listen", ":70000"},
			wantErr: []string{"--listen", ":70000"},
		},
		{
			name:    "listen given empty on command line",
			args:    []string{"--listen="},
			wantErr: []string{"--listen"},
		},
		{
			name:    "umask not octal",
			env:     map[string]string{"UMASK": "008"},
			wantErr: []string{"UMASK", "008", "octal"},
		},
		{
			name:    "umask not a number",
			env:     map[string]string{"UMASK": "u=rwx"},
			wantErr: []string{"UMASK", "octal"},
		},
		{
			name:    "umask above 0777",
			env:     map[string]string{"UMASK": "1000"},
			wantErr: []string{"UMASK", "1000"},
		},
		{
			name:    "umask negative",
			env:     map[string]string{"UMASK": "-2"},
			wantErr: []string{"UMASK"},
		},
		{
			name:    "PUID without PGID",
			env:     map[string]string{"PUID": "1000"},
			wantErr: []string{"PGID"},
		},
		{
			name:    "PGID without PUID",
			env:     map[string]string{"PGID": "1000"},
			wantErr: []string{"PUID"},
		},
		{
			name:    "PUID negative",
			env:     map[string]string{"PUID": "-1", "PGID": "1000"},
			wantErr: []string{"PUID", "-1"},
		},
		{
			name:    "PGID not a number",
			env:     map[string]string{"PUID": "1000", "PGID": "users"},
			wantErr: []string{"PGID", "users"},
		},
		{
			name:    "PUID out of range",
			env:     map[string]string{"PUID": "4294967296", "PGID": "1000"},
			wantErr: []string{"PUID", "4294967296"},
		},
		{
			name:    "relative admin password file",
			env:     map[string]string{"BITNEWS_ADMIN_PASSWORD_FILE": "secrets/admin"},
			wantErr: []string{"BITNEWS_ADMIN_PASSWORD_FILE", "absolute"},
		},
		{
			name:    "relative config-dir",
			args:    []string{"--config-dir", "data"},
			wantErr: []string{"--config-dir", "absolute"},
		},
		{
			name:    "relative key-file",
			env:     map[string]string{"BITNEWS_KEY_FILE": "./bitnews.key"},
			wantErr: []string{"BITNEWS_KEY_FILE", "absolute"},
		},
		{
			name:    "default config-dir relative because HOME is unset",
			env:     map[string]string{"HOME": "", "XDG_DATA_HOME": ""},
			wantErr: []string{"BITNEWS_CONFIG_DIR", "absolute"},
		},
		{
			name:    "key-file inside config-dir",
			args:    []string{"--config-dir", "/config", "--key-file", "/config/bitnews.key"},
			wantErr: []string{"--key-file", "inside", "/config"},
		},
		{
			name:    "key-file nested inside config-dir with trailing slash",
			args:    []string{"--config-dir", "/config/", "--key-file", "/config/secrets/bitnews.key"},
			wantErr: []string{"--key-file", "inside"},
		},
		{
			name:    "key-file inside config-dir after cleaning",
			args:    []string{"--config-dir", "/config", "--key-file", "/etc/../config/./bitnews.key"},
			wantErr: []string{"--key-file", "inside"},
		},
		{
			name:    "key-file inside config-dir with dot-dot-prefixed name",
			args:    []string{"--config-dir", "/config", "--key-file", "/config/..bitnews.key"},
			wantErr: []string{"--key-file", "inside"},
		},
		{
			name:    "key-file equal to config-dir",
			args:    []string{"--config-dir", "/config", "--key-file", "/config/"},
			wantErr: []string{"--key-file", "inside"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := devEnv()
			maps.Copy(vars, tt.env)
			_, err := bootstrap.Load(tt.args, env(vars))
			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestLoadAcceptsKeyFileOutsideConfigDirAndCleansPaths(t *testing.T) {
	tests := []struct {
		name                  string
		configDir, keyFile    string
		wantConfigDir, wantKF string
	}{
		{"sibling sharing a name prefix", "/config", "/config-keys/bitnews.key", "/config", "/config-keys/bitnews.key"},
		{"parent directory", "/var/lib/bitnews/", "/var/lib/bitnews.key", "/var/lib/bitnews", "/var/lib/bitnews.key"},
		{"escapes via dot-dot", "/config", "/config/../run/secrets/bitnews_key", "/config", "/run/secrets/bitnews_key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := bootstrap.Load([]string{"--config-dir", tt.configDir, "--key-file", tt.keyFile}, env(devEnv()))
			require.NoError(t, err)
			assert.Equal(t, tt.wantConfigDir, cfg.ConfigDir)
			assert.Equal(t, tt.wantKF, cfg.KeyFile)
		})
	}
}

func TestLoadUmaskIsOctalFromEnvWithDefault002(t *testing.T) {
	tests := []struct {
		name  string
		value *string
		want  fs.FileMode
	}{
		{"unset", nil, 0o002},
		{"empty", new(""), 0o002},
		{"three digits", new("022"), 0o022},
		{"leading zero", new("0027"), 0o027},
		{"single digit", new("7"), 0o007},
		{"maximum", new("777"), 0o777},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := devEnv()
			if tt.value != nil {
				vars["UMASK"] = *tt.value
			}
			cfg, err := bootstrap.Load(nil, env(vars))
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Umask)
		})
	}
}

func TestLoadRunAsIsUnsetWithoutPUIDAndPGID(t *testing.T) {
	cfg, err := bootstrap.Load(nil, env(devEnv()))
	require.NoError(t, err)
	assert.Nil(t, cfg.RunAs)
}

func TestLoadRunAsFromPUIDAndPGID(t *testing.T) {
	for _, tt := range []struct {
		puid, pgid string
		want       bootstrap.Identity
	}{
		{"1000", "1001", bootstrap.Identity{UID: 1000, GID: 1001}},
		{"0", "0", bootstrap.Identity{UID: 0, GID: 0}},
		{"4294967294", "65534", bootstrap.Identity{UID: 4294967294, GID: 65534}},
	} {
		t.Run(tt.puid+":"+tt.pgid, func(t *testing.T) {
			vars := devEnv()
			vars["PUID"], vars["PGID"] = tt.puid, tt.pgid
			cfg, err := bootstrap.Load(nil, env(vars))
			require.NoError(t, err)
			require.NotNil(t, cfg.RunAs)
			assert.Equal(t, tt.want, *cfg.RunAs)
		})
	}
}

func TestLoadAdminPasswordFileIsOptional(t *testing.T) {
	cfg, err := bootstrap.Load(nil, env(devEnv()))
	require.NoError(t, err)
	assert.Empty(t, cfg.AdminPasswordFile)

	vars := devEnv()
	vars["BITNEWS_ADMIN_PASSWORD_FILE"] = "/run/secrets/admin_password"
	cfg, err = bootstrap.Load(nil, env(vars))
	require.NoError(t, err)
	assert.Equal(t, "/run/secrets/admin_password", cfg.AdminPasswordFile)
}

func TestLoadHelpReturnsErrHelp(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		_, err := bootstrap.Load([]string{arg}, env(devEnv()))
		assert.ErrorIs(t, err, bootstrap.ErrHelp, arg)
	}
}

func TestLoadVersionReturnsErrVersionWithoutValidating(t *testing.T) {
	vars := devEnv()
	vars["BITNEWS_LISTEN"] = "not-an-address"
	vars["UMASK"] = "999"
	_, err := bootstrap.Load([]string{"--version"}, env(vars))
	assert.ErrorIs(t, err, bootstrap.ErrVersion)
}

func TestLoadRejectsUnknownFlagsAndArguments(t *testing.T) {
	for _, args := range [][]string{{"--no-such-flag"}, {"serve"}} {
		_, err := bootstrap.Load(args, env(devEnv()))
		assert.Error(t, err, args)
	}
}

func TestUsageDocumentsEveryBootstrapOption(t *testing.T) {
	var out strings.Builder
	bootstrap.Usage(&out)
	got := out.String()
	for _, want := range []string{
		"--config-dir", "BITNEWS_CONFIG_DIR", "$XDG_DATA_HOME/bitnews",
		"--key-file", "BITNEWS_KEY_FILE", "$XDG_CONFIG_HOME/bitnews/bitnews.key",
		"--listen", "BITNEWS_LISTEN", "0.0.0.0:9494",
		"--log-level", "BITNEWS_LOG_LEVEL",
		"--log-format", "BITNEWS_LOG_FORMAT",
		"--version", "--help",
		"PUID", "PGID",
		"UMASK", "002",
		"BITNEWS_ADMIN_PASSWORD_FILE",
	} {
		assert.Contains(t, got, want)
	}
}
