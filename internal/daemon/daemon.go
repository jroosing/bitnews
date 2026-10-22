package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/adapter/sabnzbd"
	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/postproc/adapt"
	"gitlab.com/kyura4k/gitnews/internal/postproc/finalize"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
)

// DefaultSessionCleanup is how often expired sessions are deleted.
const DefaultSessionCleanup = time.Hour

// Deps is what Build needs. The caller owns the database and closes it
// after Run has returned.
type Deps struct {
	// DB and Key are required: the open database and the master key that
	// seals Server and archive passwords.
	DB  *store.DB
	Key store.Key

	Logger  *slog.Logger
	Version api.Version
	// AdminPasswordFile, when set, bootstraps the admin from that file
	// before the setup screen decides whether it is needed.
	AdminPasswordFile string

	// Bus is shared with whoever else listens; a new one by default.
	Bus *core.Bus
	// Factory opens connections to a Server. The default dials with strict
	// TLS; tests pass in-memory servers.
	Factory func(runner.ServerSpec) nntp.ConnFactory
	// Prober tests Server connections for the API. The default dials for
	// real.
	Prober app.Prober

	Tuning Tuning
}

// Tuning holds the knobs tests turn to make time pass quickly. The zero
// value means production defaults.
type Tuning struct {
	// Runner intervals, see runner.Config.
	PollInterval      time.Duration
	SaveInterval      time.Duration
	DoneBytesInterval time.Duration
	DiskCheckInterval time.Duration
	FailureBackoff    time.Duration
	Backoff           func(attempt int) time.Duration
	Clock             runner.Clock
	// FreeSpace replaces the statfs probe of the disk guard and of Unpack.
	FreeSpace func(path string) (uint64, error)
	// MinFree replaces the 5 GiB reserve of the disk guard and of Unpack.
	MinFree *uint64

	// PostProcPollInterval is how often the Post-processor looks for
	// Fetched Downloads besides reacting to events.
	PostProcPollInterval time.Duration
	// PostProcConcurrency is how many Downloads are post-processed at once.
	PostProcConcurrency int
	// VolumeMargin is the number of recovery blocks fetched beyond the
	// minimum; zero means adapt.DefaultMargin.
	VolumeMargin int
	// SessionCleanup is how often expired sessions are deleted.
	SessionCleanup time.Duration
}

// Daemon is the assembled application.
type Daemon struct {
	log      *slog.Logger
	bus      *core.Bus
	server   *api.Server
	runner   *runner.Runner
	post     *core.PostProcessor
	sessions *auth.SessionService
	cleanup  time.Duration
	dirs     *workDirs
	track    *tracker

	ran atomic.Bool
}

// Build wires everything. It starts nothing: call Run, and serve Handler or
// HTTPServer.
func Build(ctx context.Context, deps Deps) (*Daemon, error) {
	if deps.DB == nil {
		return nil, errors.New("daemon: Deps.DB is required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	bus := deps.Bus
	if bus == nil {
		bus = core.NewBus()
	}
	db := deps.DB
	tune := deps.Tuning

	users := store.NewUserRepo(db)
	downloads := store.NewDownloadRepo(db)
	categories := store.NewCategoryRepo(db)
	servers := store.NewServerRepo(db, deps.Key)
	sources := store.NewDownloadSourceRepo(db, deps.Key)

	settings, err := app.NewSettingsService(ctx, store.NewSettingsRepo(db), bus)
	if err != nil {
		return nil, err
	}
	settingsNow := func(context.Context) (core.Settings, error) { return settings.Current(), nil }
	dirs := newWorkDirs(sources, settings.Current, log)
	dirFor := dirs.dirFor

	minFree := runner.DefaultMinFree
	if tune.MinFree != nil {
		minFree = *tune.MinFree
	}
	run, err := runner.New(runner.Config{
		Downloads: downloads, Bus: bus,
		Servers:  newServerSource(servers, log),
		Sources:  nzbSource{repo: sources, log: log},
		DirFor:   dirFor,
		Settings: settingsNow,
		MinFree:  func(context.Context) (uint64, error) { return minFree, nil },
		Factory:  deps.Factory,
		Progress: store.NewUsenetProgressRepo(db),
		Reports:  reports{repo: store.NewUsenetReportRepo(db)},
		Volumes:  store.NewUsenetUndeferredRepo(db),
		// A nil FreeSpace keeps the runner's statfs default.
		FreeSpace: tune.FreeSpace,
		Clock:     tune.Clock, Logger: log, Backoff: tune.Backoff,
		PollInterval: tune.PollInterval, DiskCheckInterval: tune.DiskCheckInterval,
		SaveInterval: tune.SaveInterval, DoneBytesInterval: tune.DoneBytesInterval,
		FailureBackoff: tune.FailureBackoff,
	})
	if err != nil {
		return nil, err
	}

	post, err := newPostProcessor(downloads, categories, sources, run, bus, settingsNow, dirFor, minFree, tune, log)
	if err != nil {
		return nil, err
	}

	// The admin file must be read before NewSetup decides whether setup is
	// required, or setup would announce a token for an instance that is
	// about to get its admin.
	if deps.AdminPasswordFile != "" {
		if _, err := auth.BootstrapAdminFromFile(ctx, deps.AdminPasswordFile, users, log); err != nil {
			return nil, err
		}
	}
	setup, err := auth.NewSetup(ctx, users, log)
	if err != nil {
		return nil, err
	}
	sessions, err := auth.NewSessionService(store.NewSessionRepo(db), users, auth.WithSessionLogger(log))
	if err != nil {
		return nil, err
	}
	uiAuth, err := auth.NewUIAuth(sessions, settings.Current)
	if err != nil {
		return nil, err
	}
	prober := deps.Prober
	if prober == nil {
		prober = app.NewNNTPProber()
	}
	remover := &dataRemover{
		settings: settings.Current, categories: categories, holder: run, dirFor: dirFor, log: log,
	}
	downloadService := app.NewDownloadService(downloads, categories, bus, app.WithDataRemover(remover))
	track := newTracker()
	diskSpace := statfs
	if tune.FreeSpace != nil {
		diskSpace = func(path string) (int64, int64, bool) {
			free, err := tune.FreeSpace(path)
			return int64(min(free, math.MaxInt64)), int64(min(free, math.MaxInt64)), err == nil //nolint:gosec // clamped
		}
	}
	adapterCreds := auth.NewAdapterCredentialService(store.NewAdapterCredentialRepo(db), auth.WithAdapterLogger(log))
	adder := app.NewAddService(downloadService, sources, settings, app.WithLogger(log))
	categoryService := app.NewCategoryService(categories, downloads, bus)
	server, err := api.New(api.Config{
		Logger:     log,
		Version:    deps.Version,
		Downloads:  downloadService,
		Adder:      adder,
		Categories: categoryService,
		Servers:    app.NewServerService(servers, bus, prober),
		Settings:   settings,
		Setup:      setup,
		Sessions:   sessions,
		UIAuth:     uiAuth,
		APIKeys:    auth.NewAPIKeyService(store.NewAPIKeyRepo(db), auth.WithAPIKeyLogger(log)),
		Bus:        bus,

		AdapterCredentials: adapterCreds,
		SABnzbd: sabnzbd.New(sabnzbd.Deps{
			Credentials: adapterCreds, Downloads: downloadService, Adder: adder,
			Categories: categoryService, Settings: settings,
			StageSource: track.stage,
			SpeedSource: func() (int64, bool) { paused, _ := run.QueuePaused(); return track.speed(), paused },
			DiskSpace:   diskSpace,
			Logger:      log,
		}),
	})
	if err != nil {
		return nil, err
	}
	cleanup := tune.SessionCleanup
	if cleanup <= 0 {
		cleanup = DefaultSessionCleanup
	}
	return &Daemon{log: log, bus: bus, server: server, runner: run, post: post, sessions: sessions, cleanup: cleanup, dirs: dirs, track: track}, nil
}

// newPostProcessor assembles the Post-processing pipeline on the real
// adapters.
func newPostProcessor(
	downloads core.DownloadRepo, categories core.CategoryRepo, sources core.DownloadSourceRepo,
	run *runner.Runner, bus *core.Bus,
	settings func(context.Context) (core.Settings, error), dirFor func(core.Download) string,
	minFree uint64, tune Tuning, log *slog.Logger,
) (*core.PostProcessor, error) {
	clock := time.Now
	requeue := func(ctx context.Context, id core.ID) error {
		// PostProcessing to Fetching: the runner sees the Download without
		// a job and fetches the volumes that were just un-deferred.
		d, err := downloads.UpdateState(ctx, id, core.StateChange{To: core.StateFetching, At: clock()})
		if err != nil {
			return err
		}
		bus.Publish(core.DownloadUpdated{Download: d})
		return nil
	}
	passwords := func(id core.ID) (core.Secret, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, pw, err := sources.Get(ctx, id)
		switch {
		case errors.Is(err, core.ErrNotFound):
			return "", nil
		case errors.Is(err, core.ErrSecretUnavailable):
			log.Warn("download password cannot be decrypted", "id", id)
			return "", nil
		case err != nil:
			return "", err
		}
		return pw, nil
	}
	finisher := finalize.New(finalize.Deps{
		Settings: settings,
		Categories: func(ctx context.Context, id core.ID) (core.Category, error) {
			return categories.Get(ctx, id)
		},
	}, finalize.WithLogger(log))
	return core.NewPostProcessor(core.PostProcessorConfig{
		Downloads: downloads, Bus: bus,
		Verifier: adapt.Verifier{DeferredBlocks: run.DeferredBlocks},
		Repairer: adapt.Repairer{},
		Unpacker: adapt.Unpacker{Passwords: passwords, MinFree: minFree, FreeSpace: tune.FreeSpace},
		Finisher: finisher, FetchReporter: run,
		DeferredVolumes: &adapt.DeferredVolumes{Controller: run, Requeue: requeue, Margin: tune.VolumeMargin},
		FS:              osFS{},
		Concurrency:     tune.PostProcConcurrency, PollInterval: tune.PostProcPollInterval,
		Logger: log, DirFor: dirFor,
	})
}

// osFS implements core.FS on the real filesystem.
type osFS struct{}

func (osFS) DirExists(dir string) (bool, error) {
	fi, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return fi.IsDir(), nil
}

// Handler returns the API handler.
func (d *Daemon) Handler() http.Handler { return d.server.Handler() }

// HTTPServer returns the hardened http.Server for addr.
func (d *Daemon) HTTPServer(addr string) *http.Server { return d.server.HTTPServer(addr) }

// Bus returns the event bus.
func (d *Daemon) Bus() *core.Bus { return d.bus }

// Run recovers interrupted work and runs the workers until ctx ends, then
// waits for every one of them. It returns nil after a clean stop. It can be
// called once.
func (d *Daemon) Run(ctx context.Context) error {
	if !d.ran.CompareAndSwap(false, true) {
		return errors.New("daemon: Run called twice")
	}
	if err := d.runner.Recover(ctx); err != nil && ctx.Err() == nil {
		// Downloads that could not be requeued are picked up by the
		// runner's own loop as orphans.
		d.log.Error("usenet recover failed", "err", err)
	}

	workers, stop := context.WithCancel(ctx)
	defer stop()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	start := func(name string, f func(context.Context) error) {
		wg.Go(func() {
			err := f(workers)
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				d.log.Error("daemon worker stopped", "worker", name, "err", err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("daemon: %s: %w", name, err))
				mu.Unlock()
			}
			stop() // one worker ending takes the others down with it
		})
	}
	start("usenet runner", d.runner.Run)
	start("post-processor", d.post.Run)
	start("working directory cache", func(ctx context.Context) error {
		d.dirs.forgetRemoved(ctx, d.bus)
		return nil
	})
	start("event tracker", func(ctx context.Context) error {
		d.track.run(ctx, d.bus)
		return nil
	})
	start("session cleanup", func(ctx context.Context) error {
		d.sessions.RunCleanup(ctx, d.cleanup)
		return nil
	})
	wg.Wait()
	return errors.Join(errs...)
}

// Serve runs the workers and the HTTP server built for addr and shuts down in
// order: when ctx ends it stops the HTTP server first (serve does that), then
// cancels the workers and waits for them. serve receives the context that
// ends the server; it normally is api.Serve on a listener.
func (d *Daemon) Serve(ctx context.Context, addr string, serve func(ctx context.Context, srv *http.Server) error) error {
	// The workers must outlive ctx until the server is down.
	workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(workCtx) }()
	serveErr := serve(ctx, d.HTTPServer(addr))
	cancel()
	return errors.Join(serveErr, <-done)
}
