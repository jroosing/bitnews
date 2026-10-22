// Package bootstrap parses the settings that must be known before the
// database is opened: the flags and environment variables of the bootstrap
// table, plus the logger built from them.
package bootstrap

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds the validated bootstrap settings.
type Config struct {
	// ConfigDir holds bitnews.db.
	ConfigDir string
	// KeyFile is the master key for secret encryption.
	KeyFile string
	// Listen is the host:port of the single HTTP listener.
	Listen string
	// LogLevel is the minimum level that is logged.
	LogLevel slog.Level
	// LogFormat selects the slog handler.
	LogFormat LogFormat
	// Umask is applied process-wide before any file is created.
	Umask fs.FileMode
	// RunAs is the identity to drop to when started as root. It is nil when
	// PUID and PGID are unset.
	RunAs *Identity
	// AdminPasswordFile optionally bootstraps the admin password. It is
	// empty when unset.
	AdminPasswordFile string
	// TorrentPort is the TCP and uTP port the torrent engine listens on,
	// when the operator fixes it (BITNEWS_TORRENT_PORT, typically to match a
	// published container port). It overrides the torrent.listen_port
	// setting. Zero means not set: the setting decides, by default 51413.
	// Peers need the port forwarded to reach bitnews: there is no UPnP.
	TorrentPort int
}

// Identity is a numeric user and group id.
type Identity struct {
	UID int
	GID int
}

// LogFormat is the output format of the logger.
type LogFormat string

// The supported log formats.
const (
	LogText LogFormat = "text"
	LogJSON LogFormat = "json"
)

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// option is one row of the bootstrap table. flag is empty for env-only rows.
// arg and def only feed the usage text.
type option struct {
	flag  string
	env   string
	arg   string
	usage string
	def   string
}

var (
	optConfigDir   = option{"config-dir", "BITNEWS_CONFIG_DIR", "dir", "directory holding bitnews.db", "$XDG_DATA_HOME/bitnews, or ~/.local/share/bitnews"}
	optKeyFile     = option{"key-file", "BITNEWS_KEY_FILE", "path", "master key that encrypts stored Server and archive passwords; created on first start", "bitnews.key inside the config dir"}
	optListen      = option{"listen", "BITNEWS_LISTEN", "host:port", "address for the API, adapters and UI", "0.0.0.0:9494"}
	optLogLevel    = option{"log-level", "BITNEWS_LOG_LEVEL", "level", "debug, info, warn or error", "info"}
	optLogFormat   = option{"log-format", "BITNEWS_LOG_FORMAT", "format", "text or json", "text"}
	optPUID        = option{"", "PUID", "uid", "user id to drop to when started as root; requires PGID", "unset"}
	optPGID        = option{"", "PGID", "gid", "group id to drop to when started as root; requires PUID", "unset"}
	optUmask       = option{"", "UMASK", "octal", "file mode creation mask applied at startup", "002"}
	optTorrentPort = option{"", "BITNEWS_TORRENT_PORT", "port", "TCP and uTP port the torrent engine listens on, overriding the setting; forward it for incoming peers (no UPnP)", "the torrent port setting (51413)"}
	optAdminPass   = option{"", "BITNEWS_ADMIN_PASSWORD_FILE", "path", "absolute path of a file holding the initial admin password", "unset"}
)

var options = []option{optConfigDir, optKeyFile, optListen, optLogLevel, optLogFormat, optPUID, optPGID, optUmask, optAdminPass, optTorrentPort}

// Usage writes the help text for every bootstrap option to w.
func Usage(w io.Writer) {
	var b strings.Builder
	b.WriteString("Usage: bitnews [options]\n       bitnews healthcheck\n\n")
	b.WriteString("healthcheck asks the running daemon on 127.0.0.1 (port from BITNEWS_LISTEN) for /api/v1/health\nand exits 0 on a 200 within 3 seconds, otherwise 1. It is meant for container health checks.\n\n")
	b.WriteString("Options (a flag wins over its environment variable; empty variables are ignored):\n")
	for _, o := range options {
		if o.flag != "" {
			fmt.Fprintf(&b, "  --%s %s\n        %s (env %s, default %s)\n", o.flag, o.arg, o.usage, o.env, o.def)
		}
	}
	b.WriteString("  --version\n        print version and exit\n")
	b.WriteString("  -h, --help\n        print this help and exit\n")
	b.WriteString("\nEnvironment only:\n")
	for _, o := range options {
		if o.flag == "" {
			fmt.Fprintf(&b, "  %s=%s\n        %s (default %s)\n", o.env, o.arg, o.usage, o.def)
		}
	}
	_, _ = io.WriteString(w, b.String())
}

// setting is a raw value together with where it came from, so errors can
// name the flag or env var the user has to fix.
type setting struct {
	value string
	from  string
}

func (s setting) errorf(format string, args ...any) error {
	return fmt.Errorf("%s=%q: %s", s.from, s.value, fmt.Sprintf(format, args...))
}

// ErrHelp is returned by Load when -h or --help is given. The caller should
// print Usage and exit successfully.
var ErrHelp = flag.ErrHelp

// ErrVersion is returned by Load when --version is given. Nothing else is
// validated in that case.
var ErrVersion = errors.New("version requested")

// newFlagSet defines every flag of the bootstrap table plus --version. It
// returns the flag values keyed by flag name.
func newFlagSet() (*flag.FlagSet, map[string]*string, *bool) {
	flags := flag.NewFlagSet("bitnews", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	values := map[string]*string{}
	for _, o := range options {
		if o.flag != "" {
			values[o.flag] = flags.String(o.flag, "", o.usage)
		}
	}
	showVersion := flags.Bool("version", false, "print version and exit")
	return flags, values, showVersion
}

// Load parses args (without the program name) and the environment seen
// through getenv into a validated Config.
func Load(args []string, getenv func(string) (string, bool)) (Config, error) {
	flags, flagValues, showVersion := newFlagSet()
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if *showVersion {
		return Config{}, ErrVersion
	}
	if flags.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })

	// lookup returns the setting for o: the flag if given, else a non-empty
	// env var, else def.
	lookup := func(o option, def string) setting {
		if given[o.flag] {
			return setting{*flagValues[o.flag], "--" + o.flag}
		}
		if v, _ := getenv(o.env); v != "" {
			return setting{v, o.env}
		}
		return setting{def, o.env + " (default)"}
	}

	var cfg Config
	configDir := lookup(optConfigDir, filepath.Join(xdgDir(getenv, "XDG_DATA_HOME", ".local/share"), "bitnews"))
	if !filepath.IsAbs(configDir.value) {
		return Config{}, configDir.errorf("must be an absolute path")
	}
	cfg.ConfigDir = filepath.Clean(configDir.value)

	// The key is generated on first start and lives next to the database by
	// default, so there is nothing to set up. A separate location (a mounted
	// secret, say) keeps a copy of the database alone from exposing the
	// stored passwords; it is optional.
	keyFile := lookup(optKeyFile, filepath.Join(cfg.ConfigDir, "bitnews.key"))
	if !filepath.IsAbs(keyFile.value) {
		return Config{}, keyFile.errorf("must be an absolute path")
	}
	cfg.KeyFile = filepath.Clean(keyFile.value)
	if cfg.KeyFile == cfg.ConfigDir {
		return Config{}, keyFile.errorf("must be a file, but it is the config directory %q", cfg.ConfigDir)
	}

	listen := lookup(optListen, "0.0.0.0:9494")
	if err := validateListen(listen.value); err != nil {
		return Config{}, listen.errorf("%v", err)
	}
	cfg.Listen = listen.value

	level := lookup(optLogLevel, "info")
	var ok bool
	if cfg.LogLevel, ok = logLevels[level.value]; !ok {
		return Config{}, level.errorf("must be debug, info, warn or error")
	}

	format := lookup(optLogFormat, string(LogText))
	switch cfg.LogFormat = LogFormat(format.value); cfg.LogFormat {
	case LogText, LogJSON:
	default:
		return Config{}, format.errorf("must be text or json")
	}

	umask := lookup(optUmask, "002")
	mask, err := strconv.ParseUint(umask.value, 8, 32)
	if err != nil || mask > 0o777 {
		return Config{}, umask.errorf("must be an octal mode from 000 to 777")
	}
	cfg.Umask = fs.FileMode(mask)

	puid, pgid := lookup(optPUID, ""), lookup(optPGID, "")
	switch {
	case puid.value == "" && pgid.value == "":
	case puid.value == "":
		return Config{}, errors.New("PGID is set but PUID is not: set both or neither")
	case pgid.value == "":
		return Config{}, errors.New("PUID is set but PGID is not: set both or neither")
	default:
		uid, err := parseID(puid)
		if err != nil {
			return Config{}, err
		}
		gid, err := parseID(pgid)
		if err != nil {
			return Config{}, err
		}
		cfg.RunAs = &Identity{UID: uid, GID: gid}
	}

	if adminPass := lookup(optAdminPass, ""); adminPass.value != "" {
		if !filepath.IsAbs(adminPass.value) {
			return Config{}, adminPass.errorf("must be an absolute path")
		}
		cfg.AdminPasswordFile = filepath.Clean(adminPass.value)
	}

	if port := lookup(optTorrentPort, ""); port.value != "" {
		n, err := strconv.ParseUint(port.value, 10, 16)
		if err != nil || n == 0 {
			return Config{}, port.errorf("must be a port number from 1 to 65535")
		}
		cfg.TorrentPort = int(n)
	}
	return cfg, nil
}

// parseID parses a numeric uid or gid, which is a 32-bit unsigned value.
func parseID(s setting) (int, error) {
	id, err := strconv.ParseUint(s.value, 10, 32)
	if err != nil {
		return 0, s.errorf("must be a number from 0 to 4294967295")
	}
	return int(id), nil
}

// validateListen checks that addr is host:port with a numeric port. The
// host may be empty to listen on all interfaces.
func validateListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be host:port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errors.New("port must be a number from 0 to 65535")
	}
	return nil
}

// xdgDir returns the XDG base directory named by envName. Per the XDG spec an
// unset, empty or relative value is ignored in favour of $HOME/fallback.
func xdgDir(getenv func(string) (string, bool), envName, fallback string) string {
	if dir, _ := getenv(envName); filepath.IsAbs(dir) {
		return dir
	}
	home, _ := getenv("HOME")
	return filepath.Join(home, fallback)
}
