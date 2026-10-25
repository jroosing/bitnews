// Command bitnews is a self-hosted Usenet and BitTorrent download daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jroosing/bitnews/internal/api"
	"github.com/jroosing/bitnews/internal/bootstrap"
	"github.com/jroosing/bitnews/internal/daemon"
	"github.com/jroosing/bitnews/internal/fsutil"
	"github.com/jroosing/bitnews/internal/store"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

const (
	// shutdownGrace is how long a graceful shutdown waits for requests in
	// flight before closing their connections.
	shutdownGrace = 10 * time.Second
	// sessionCleanupEvery is how often expired sessions are deleted.
	sessionCleanupEvery = time.Hour
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.LookupEnv, setUmask, os.Stdout, os.Stderr, listenAndServe)
	stop()
	os.Exit(code)
}

// serveFunc runs srv until ctx ends and then shuts it down gracefully. It is
// a seam: tests that must not bind a port pass a stub.
type serveFunc func(ctx context.Context, srv *http.Server, log *slog.Logger) error

// listenAndServe listens on srv.Addr and serves until ctx ends.
func listenAndServe(ctx context.Context, srv *http.Server, log *slog.Logger) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String())
	return api.Serve(ctx, srv, ln, shutdownGrace)
}

// run is main without the process globals, so it can be tested. It runs
// until ctx ends. It returns the exit code: 0 on success, 1 for a startup or
// runtime failure, 2 for invalid flags or environment. umask applies the file
// mode creation mask process-wide; tests pass a recorder.
func run(ctx context.Context, args []string, getenv func(string) (string, bool), umask func(mask int), stdout, stderr io.Writer, serve serveFunc) int {
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck(ctx, getenv, stdout, stderr)
	}

	cfg, err := bootstrap.Load(args, getenv)
	switch {
	case errors.Is(err, bootstrap.ErrVersion):
		fmt.Fprintf(stdout, "bitnews %s (%s)\n", version, commit)
		return 0
	case errors.Is(err, bootstrap.ErrHelp):
		bootstrap.Usage(stdout)
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "bitnews: %v\nRun 'bitnews --help' for usage.\n", err)
		return 2
	}

	// Startup order step 2: the umask is set before any file is created.
	// bitnews runs as whoever started it (ADR-0006), so there is no
	// identity to change.
	log := bootstrap.NewLogger(stderr, cfg)
	umask(int(cfg.Umask))

	log.Info("starting bitnews",
		"version", version,
		"commit", commit,
		"listen", cfg.Listen,
		"config_dir", cfg.ConfigDir,
		"key_file", cfg.KeyFile,
		"torrent_port", cfg.TorrentPort,
	)
	if err := serveAll(ctx, cfg, log, serve); err != nil {
		fmt.Fprintf(stderr, "bitnews: %v\n", err)
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

// serveAll runs startup steps 3 to 6: load the key, open the database, build
// the daemon, serve, and on the way out close the database. Shutdown order:
// the HTTP server stops first, then the daemon's workers are cancelled and
// awaited, then the database is closed.
func serveAll(ctx context.Context, cfg bootstrap.Config, log *slog.Logger, serve serveFunc) (err error) {
	if err := prepareConfigDir(cfg.ConfigDir); err != nil {
		return err
	}
	// The master key encrypts Server passwords and archive passwords.
	// Loading it creates it on first start and fails early when it cannot
	// be read.
	key, err := store.LoadOrCreateKey(cfg.KeyFile, log)
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, filepath.Join(cfg.ConfigDir, "bitnews.db"), store.WithLogger(log))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	d, err := daemon.Build(ctx, daemon.Deps{
		DB: db, Key: key, Logger: log,
		Version:           api.Version{Version: version, Commit: commit},
		AdminPasswordFile: cfg.AdminPasswordFile,
		Torrent:           daemon.TorrentOptions{Port: cfg.TorrentPort},
		Tuning:            daemon.Tuning{SessionCleanup: sessionCleanupEvery},
	})
	if err != nil {
		return err
	}
	return d.Serve(ctx, cfg.Listen, func(ctx context.Context, srv *http.Server) error {
		return serve(ctx, srv, log)
	})
}

// prepareConfigDir creates the config dir and checks that bitnews can write
// in it. bitnews runs as whoever starts it and cannot change an owner
// (ADR-0006), so a directory it may not write to is a startup error that
// says which user it runs as and how to fix it. The usual cause is Docker:
// it creates a missing bind-mount directory on the host as root.
func prepareConfigDir(dir string) error {
	err := os.MkdirAll(dir, 0o700)
	if err == nil && fsutil.Writable(dir) {
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("create config dir: %w", err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	return fmt.Errorf("the config dir %q is not writable by uid %d gid %d, the user bitnews runs as. "+
		"bitnews cannot change the owner itself. With the Docker image, drop `user:` and set PUID/PGID instead, "+
		"or give that user the directory on the host, for example:\n"+
		"  sudo mkdir -p <host dir> && sudo chown %d:%d <host dir>\n"+
		"where <host dir> is what you mount at %s (Docker creates a missing bind-mount directory as root)",
		dir, uid, gid, uid, gid, dir)
}
