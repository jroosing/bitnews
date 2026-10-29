package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jroosing/bitnews/internal/adapter/qbit"
	"github.com/jroosing/bitnews/internal/adapter/sabnzbd"
	"github.com/jroosing/bitnews/internal/api"
	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/fsutil"
	"github.com/jroosing/bitnews/internal/netbind"
	"github.com/jroosing/bitnews/internal/postproc/finalize"
	"github.com/jroosing/bitnews/internal/postproc/unpack"
	"github.com/jroosing/bitnews/internal/queue"
	"github.com/jroosing/bitnews/internal/store"
	trunner "github.com/jroosing/bitnews/internal/torrent/runner"
	"github.com/jroosing/bitnews/internal/usenet/nntp"
	"github.com/jroosing/bitnews/internal/usenet/runner"
	"github.com/jroosing/bitnews/internal/view"
	"github.com/jroosing/bitnews/internal/web"
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

	// TorrentEngine is the torrent engine. The default is a real one built
	// from Torrent and the Settings, which Run closes after the workers have
	// stopped; an engine passed here belongs to the caller, who closes it
	// after Run has returned. Tests pass coretest.FakeTorrentEngine.
	TorrentEngine core.TorrentEngine
	// DisableTorrent turns torrents off: no engine, no torrent Queue runner,
	// and adding a torrent is refused. The NZB side is unaffected.
	DisableTorrent bool
	// Torrent configures the engine Build creates.
	Torrent TorrentOptions

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
	// TorrentSampleInterval, TorrentPersistInterval and TorrentLimitInterval
	// are the torrent runner's progress sampling, counter persistence and
	// Share Limit evaluation intervals (1 s, 30 s and 30 s by default).
	// PollInterval, DoneBytesInterval and DiskCheckInterval apply to it too.
	TorrentSampleInterval  time.Duration
	TorrentPersistInterval time.Duration
	TorrentLimitInterval   time.Duration
	Backoff                func(attempt int) time.Duration
	Clock                  runner.Clock
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
	// minimum; zero means runner.DefaultVolumeMargin.
	VolumeMargin int
	// SessionCleanup is how often expired sessions are deleted.
	SessionCleanup time.Duration
	// BindingInterval is how often the network bindings are checked; zero
	// means netbind.DefaultInterval.
	BindingInterval time.Duration
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

	// torrents is nil when torrents are off. engine is closed by Run when
	// Build made it (owned).
	torrents *trunner.Runner
	owned    *closeOnce
	restarts *restartWatcher
	speed    *speedApplier
	port     *torrentPort
	switches []namedSwitch

	ran atomic.Bool
}

// namedSwitch is a network kill switch to run as a worker.
type namedSwitch struct {
	name string
	ks   *netbind.Monitor
}

// Build wires everything. It starts nothing: call Run, and serve Handler or
// HTTPServer.
func Build(ctx context.Context, deps Deps) (d *Daemon, err error) {
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

	tools, err := locateTools()
	if err != nil {
		return nil, err
	}

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

	// min_free is read from the Settings on each use, so a change applies
	// at once; a test's fixed reserve wins.
	minFree := minFreeFunc(settings.Current, tune.MinFree)

	finisher := finalize.New(finalize.Deps{
		Settings: settingsNow,
		Categories: func(ctx context.Context, id core.ID) (core.Category, error) {
			return categories.Get(ctx, id)
		},
	}, finalize.WithLogger(log))

	// The torrent side: engine, then the Queue runner on top of it. The
	// Share Limit actions remove Downloads through the DownloadService, which
	// needs the data remover, which needs this runner: shareLimits closes the
	// loop below.
	shareLimits := &shareLimitRemover{}
	var (
		trun           *trunner.Runner
		owned          *closeOnce
		engine         core.TorrentEngine
		torrentSources *store.TorrentSourceRepo
		torrentRecords *store.TorrentStateRepo
	)
	if !deps.DisableTorrent {
		engine = deps.TorrentEngine
		if engine == nil {
			eng, err := newTorrentEngine(db, settings.Current(), deps.Torrent, log)
			if err != nil {
				return nil, fmt.Errorf("daemon: torrent engine: %w", err)
			}
			engine, owned = eng, &closeOnce{c: eng}
		}
		defer func() {
			if err != nil {
				_ = owned.Close()
			}
		}()
		torrentSources, torrentRecords = store.NewTorrentSourceRepo(db, deps.Key), store.NewTorrentStateRepo(db)
		trun, err = trunner.New(trunner.Config{
			Downloads: downloads, Sources: torrentSources, Records: torrentRecords,
			Engine: engine, Bus: bus, Settings: settingsNow,
			Destination: finisher.Destination, Remover: shareLimits,
			MinFree:   minFree,
			FreeSpace: tune.FreeSpace, Logger: log,
			PollInterval: tune.PollInterval, SampleInterval: tune.TorrentSampleInterval,
			DoneBytesInterval: tune.DoneBytesInterval, DiskCheckInterval: tune.DiskCheckInterval,
			PersistInterval: tune.TorrentPersistInterval, LimitInterval: tune.TorrentLimitInterval,
		})
		if err != nil {
			return nil, err
		}
	}
	tport := &torrentPort{settings: settings, override: deps.Torrent.Port, log: log, events: bus}
	if lp, ok := engine.(listenPorter); ok {
		tport.engine = lp
	}
	dirFor := dirs.dirFor
	var torrentHolder fetchHolder
	torrentDir := dirs.dirFor
	if trun != nil {
		torrentHolder, torrentDir = trun, trun.Dir
		dirFor = func(d core.Download) string {
			if d.SourceKind == core.SourceTorrent {
				return trun.ContentDir(d)
			}
			return dirs.dirFor(d)
		}
	}
	run, err := runner.New(runner.Config{
		Downloads: downloads, Bus: bus,
		Servers:  newServerSource(servers, log),
		Sources:  nzbSource{repo: sources, log: log},
		DirFor:   dirFor,
		Settings: settingsNow,
		MinFree:  minFree,
		Factory:  usenetFactory(deps.Factory, settings.Current),
		Progress: store.NewUsenetProgressRepo(db),
		Reports:  store.NewUsenetReportRepo(db),
		Volumes:  store.NewUsenetUndeferredRepo(db),
		// A nil FreeSpace keeps the runner's statfs default.
		FreeSpace: tune.FreeSpace,
		Clock:     tune.Clock, Logger: log, Backoff: tune.Backoff,
		PollInterval: tune.PollInterval, DiskCheckInterval: tune.DiskCheckInterval,
		SaveInterval: tune.SaveInterval, DoneBytesInterval: tune.DoneBytesInterval,
		FailureBackoff: tune.FailureBackoff, VolumeMargin: tune.VolumeMargin,
	})
	if err != nil {
		return nil, err
	}

	var managed func(core.ID) bool
	if trun != nil {
		managed = trun.Managed
	}
	post, err := newPostProcessor(downloads, sources, run, bus, dirFor, minFree, postprocConcurrency(settings.Current(), tune), tune, log, tools,
		torrentFinisher{inner: finisher, managed: managed, log: log})
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
		settings: settings.Current, categories: categories, holder: queueHolder{run.Queue()}, dirFor: dirFor, log: log,
		torrentHolder: torrentHolder, torrentDir: torrentDir,
	}
	dlOpts := []app.Option{app.WithDataRemover(remover)}
	if trun != nil {
		dlOpts = append(dlOpts, app.WithTorrentSources(torrentSources), app.WithShareLimits(trun))
	}
	downloadService := app.NewDownloadService(downloads, categories, bus, dlOpts...)
	shareLimits.downloads = downloadService
	track := newTracker()
	var facts torrentFacts // a nil interface when torrents are off
	if trun != nil {
		facts = trun
	}
	views, err := view.New(view.Config{Downloads: downloads, Live: liveSource{track: track, torrents: facts}, Settings: settings.Current})
	if err != nil {
		return nil, err
	}
	diskSpace := fsDiskSpace
	if tune.FreeSpace != nil {
		diskSpace = func(path string) (int64, int64, bool) {
			free, err := tune.FreeSpace(path)
			return int64(min(free, math.MaxInt64)), int64(min(free, math.MaxInt64)), err == nil //nolint:gosec // clamped
		}
	}
	appKey, err := auth.NewAppKeyService(ctx, store.NewAppKeyRepo(db), auth.WithAppKeyLogger(log))
	if err != nil {
		return nil, err
	}
	addOpts := []app.Option{app.WithLogger(log)}
	if trun != nil {
		addOpts = append(addOpts, app.WithTorrentSources(torrentSources), app.WithShareLimits(trun))
	}
	adder := app.NewAddService(downloadService, sources, settings, addOpts...)
	categoryService := app.NewCategoryService(categories, downloads, bus)
	var qbitHandler http.Handler
	if trun != nil {
		qbitHandler = qbit.New(qbit.Deps{
			AppKey: appKey, Downloads: downloadService, Categories: categoryService, Settings: settings,
			Adder: adder, View: views, ContentDir: trun.ContentDir, Logger: log,
		})
	}
	serverService := app.NewServerService(servers, bus, prober)
	runtime := newWebRuntime(queuesOf(run, trun), run, torrentRunnerOrNil(trun), track)
	// The speed limits of the Settings are in force from the start, before
	// the runners run (the torrent runner passes them to its engine then).
	speed := &speedApplier{settings: settings.Current, target: runtime}
	speed.apply()
	started := settings.Current()
	webUI, err := web.New(web.Deps{
		Logger: log, Version: deps.Version.Version, Sessions: sessions, UIAuth: uiAuth, Setup: setup, Settings: settings,
		Downloads: downloadService, Categories: categoryService, Servers: serverService, Bus: bus,
		Adder: adder, AppKey: appKey, View: views,
		Runtime: runtime, Config: &webConfig{started: started, portOverride: deps.Torrent.Port, port: tport},
		Detail: run.FetchDetailOf,
	})
	if err != nil {
		return nil, err
	}
	server, err := api.New(api.Config{
		Logger:     log,
		Version:    deps.Version,
		Downloads:  downloadService,
		Adder:      adder,
		Categories: categoryService,
		Servers:    serverService,
		Settings:   settings,
		Setup:      setup,
		AppKey:     appKey,
		Bus:        bus,

		TorrentPort: tport,

		Web:  webUI,
		QBit: qbitHandler,
		SABnzbd: sabnzbd.New(sabnzbd.Deps{
			AppKey: appKey, Downloads: downloadService, Adder: adder, View: views,
			Categories: categoryService, Settings: settings,
			SpeedSource: func() (int64, bool) { paused, _, _ := run.Queue().Paused(); return track.speed(), paused },
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
	d = &Daemon{
		log: log, bus: bus, server: server, runner: run, post: post, sessions: sessions, cleanup: cleanup,
		dirs: dirs, track: track, torrents: trun, owned: owned,
		restarts: &restartWatcher{start: restartOnlyOf(started), current: settings.Current, log: log},
		speed:    speed,
		port:     tport,
	}
	d.switches = networkSwitches(settings.Current().Network, run, trun, log, tune)
	return d, nil
}

// networkSwitches builds a kill switch for each protocol whose binding is
// on. When the bound interface goes away the protocol's Queue is paused for
// that reason; when it returns, the reason is released.
func networkSwitches(n core.NetworkSettings, usenet *runner.Runner, torrents *trunner.Runner, log *slog.Logger, tune Tuning) []namedSwitch {
	var out []namedSwitch
	add := func(name string, b core.ProtocolBinding, q *queue.Queue) {
		if !b.Enabled() {
			return
		}
		out = append(out, namedSwitch{name: name, ks: netbind.NewMonitor(
			netbind.Binding{Interface: b.Interface, Address: b.Address}, tune.BindingInterval, bindingSwitch(name, q, log), nil)})
	}
	add("usenet", n.Usenet, usenet.Queue())
	if torrents != nil {
		add("torrent", n.Torrent, torrents.Queue())
	}
	return out
}

// usenetFactory returns the connection factory of the Usenet runner: the
// given one (tests), else strict-TLS dialling whose sockets are bound as the
// current Settings say. The binding is read for each Server's factory, so a
// change applies to connections made after the next Server sync.
func usenetFactory(f func(runner.ServerSpec) nntp.ConnFactory, settings func() core.Settings) func(runner.ServerSpec) nntp.ConnFactory {
	if f != nil {
		return f
	}
	return func(s runner.ServerSpec) nntp.ConnFactory {
		cfg := nntp.Config{Host: s.Host, Port: s.Port, User: s.User, Pass: s.Pass}
		if b := settings().Network.Usenet; b.Enabled() {
			cfg.Dialer = netbind.Binding{Interface: b.Interface, Address: b.Address}.Dialer(&net.Dialer{})
		}
		return nntp.DialFactory(cfg)
	}
}

// newPostProcessor assembles the Post-processing pipeline on the real
// adapters. finisher is the (torrent-aware) Finisher.
func newPostProcessor(
	downloads core.DownloadRepo, sources core.DownloadSourceRepo,
	run *runner.Runner, bus *core.Bus,
	dirFor func(core.Download) string,
	minFree func(context.Context) (uint64, error), concurrency int, tune Tuning, log *slog.Logger, tools postprocTools, finisher core.Finisher,
) (*core.PostProcessor, error) {
	passwords := func(id core.ID) (core.Secret, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, pw, err := sourceOf(ctx, sources, id, log)
		switch {
		case errors.Is(err, core.ErrNotFound):
			return "", nil
		case err != nil:
			return "", err
		}
		return pw, nil
	}
	return core.NewPostProcessor(core.PostProcessorConfig{
		Downloads: downloads, Bus: bus,
		Repairer: tools.par2,
		Unpacker: unpack.Unpacker{SevenZip: tools.sevenZip, Passwords: passwords, MinFreeFunc: func() uint64 { v, _ := minFree(context.Background()); return v }, FreeSpace: tune.FreeSpace},
		// Torrents never reach the FetchReport: it is asked only when
		// there is no PAR2 set to verify, and torrents skip Verify.
		Finisher: finisher, FetchReporter: run,
		DeferredVolumes: run,
		FS:              osFS{},
		Concurrency:     concurrency, PollInterval: tune.PostProcPollInterval,
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
	if err := d.runner.Queue().Recover(ctx); err != nil && ctx.Err() == nil {
		// Downloads that could not be requeued are picked up by the
		// runner's own loop as orphans.
		d.log.Error("usenet recover failed", "err", err)
	}
	if d.torrents != nil {
		if err := d.torrents.Recover(ctx); err != nil && ctx.Err() == nil {
			d.log.Error("torrent recover failed", "err", err)
		}
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
	if d.torrents != nil {
		start("torrent runner", d.torrents.Run)
	}
	for _, sw := range d.switches {
		start(sw.name+" network monitor", sw.ks.Run)
	}
	start("restart watcher", func(ctx context.Context) error {
		d.restarts.run(ctx, d.bus)
		return nil
	})
	start("speed limits", func(ctx context.Context) error {
		d.speed.run(ctx, d.bus)
		return nil
	})
	start("torrent listen port", func(ctx context.Context) error {
		d.port.run(ctx, d.bus)
		return nil
	})
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
	// The engine goes after the workers that use it and before the database.
	if err := d.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Close closes the torrent engine if Build made it. Run does it after the
// workers have stopped; call Close yourself only when Run was never called.
// It is safe to call twice.
func (d *Daemon) Close() error { return d.owned.Close() }

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

// fsDiskSpace reports the free and total bytes of the filesystem holding
// path, or ok false when they cannot be read.
func fsDiskSpace(path string) (free, total int64, ok bool) {
	f, t, err := fsutil.DiskSpace(path)
	if err != nil {
		return 0, 0, false
	}
	return int64(min(f, math.MaxInt64)), int64(min(t, math.MaxInt64)), true //nolint:gosec // clamped
}

// torrentRunnerOrNil converts the torrent runner to the interface the UI's
// runtime needs, keeping a nil runner a nil interface.
func torrentRunnerOrNil(r *trunner.Runner) torrentRunner {
	if r == nil {
		return nil
	}
	return r
}

// queuesOf maps each Source kind to its Queue; torrents are absent when the
// torrent side is off.
func queuesOf(usenet *runner.Runner, torrents *trunner.Runner) map[core.SourceKind]queueControl {
	qs := map[core.SourceKind]queueControl{core.SourceNZB: usenet.Queue()}
	if torrents != nil {
		qs[core.SourceTorrent] = torrents.Queue()
	}
	return qs
}
