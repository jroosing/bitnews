// Command bitnews is a self-hosted Usenet and BitTorrent download daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/bootstrap"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/privdrop"
	"gitlab.com/kyura4k/gitnews/internal/store"
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
	code := run(ctx, os.Args[1:], os.LookupEnv, privdrop.OS(), os.Stdout, os.Stderr, listenAndServe)
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
// runtime failure (such as a failed privilege drop), 2 for invalid flags or
// environment. sys is the process-wide state the privilege drop changes;
// tests pass a fake.
func run(ctx context.Context, args []string, getenv func(string) (string, bool), sys privdrop.Syscalls, stdout, stderr io.Writer, serve serveFunc) int {
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

	// Startup order step 2: nothing may be opened or parsed before the
	// process has dropped root.
	log := bootstrap.NewLogger(stderr, cfg)
	if err := privdrop.Apply(privdrop.Options{RunAs: cfg.RunAs, Umask: cfg.Umask}, sys, log); err != nil {
		fmt.Fprintf(stderr, "bitnews: %v\n", err)
		return 1
	}

	log.Info("starting bitnews",
		"version", version,
		"commit", commit,
		"listen", cfg.Listen,
		"config_dir", cfg.ConfigDir,
		"key_file", cfg.KeyFile,
	)
	if err := serveAll(ctx, cfg, log, serve); err != nil {
		fmt.Fprintf(stderr, "bitnews: %v\n", err)
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

// serveAll runs startup steps 3 to 6: load the key, open the database, wire
// the services, serve, and on the way out close everything it opened.
func serveAll(ctx context.Context, cfg bootstrap.Config, log *slog.Logger, serve serveFunc) (err error) {
	// The master key encrypts Server passwords from M2 on. Loading it now
	// creates it on first start and fails early when it cannot be read.
	if _, err := store.LoadOrCreateKey(cfg.KeyFile, log); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	db, err := store.Open(ctx, filepath.Join(cfg.ConfigDir, "bitnews.db"), store.WithLogger(log))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	bus := core.NewBus()
	users := store.NewUserRepo(db)
	downloads := store.NewDownloadRepo(db)
	categories := store.NewCategoryRepo(db)

	settings, err := app.NewSettingsService(ctx, store.NewSettingsRepo(db), bus)
	if err != nil {
		return err
	}
	// The admin file must be read before NewSetup decides whether setup is
	// required, or setup would announce a token for an instance that is about
	// to get its admin.
	if cfg.AdminPasswordFile != "" {
		if _, err := auth.BootstrapAdminFromFile(ctx, cfg.AdminPasswordFile, users, log); err != nil {
			return err
		}
	}
	setup, err := auth.NewSetup(ctx, users, log)
	if err != nil {
		return err
	}
	sessions, err := auth.NewSessionService(store.NewSessionRepo(db), users, auth.WithSessionLogger(log))
	if err != nil {
		return err
	}
	uiAuth, err := auth.NewUIAuth(sessions, settings.Current)
	if err != nil {
		return err
	}
	server, err := api.New(api.Config{
		Logger:     log,
		Version:    api.Version{Version: version, Commit: commit},
		Downloads:  app.NewDownloadService(downloads, categories, bus),
		Categories: app.NewCategoryService(categories, downloads, bus),
		Settings:   settings,
		Setup:      setup,
		Sessions:   sessions,
		UIAuth:     uiAuth,
		APIKeys:    auth.NewAPIKeyService(store.NewAPIKeyRepo(db), auth.WithAPIKeyLogger(log)),
		Bus:        bus,
	})
	if err != nil {
		return err
	}

	cleanupCtx, stopCleanup := context.WithCancel(ctx)
	var cleanup sync.WaitGroup
	cleanup.Go(func() { sessions.RunCleanup(cleanupCtx, sessionCleanupEvery) })
	defer func() {
		stopCleanup()
		cleanup.Wait()
	}()

	return serve(ctx, server.HTTPServer(cfg.Listen), log)
}
