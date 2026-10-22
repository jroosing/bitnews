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

	"gitlab.com/kyura4k/gitnews/internal/adapter/qbit"
	"gitlab.com/kyura4k/gitnews/internal/adapter/sabnzbd"
	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/netbind"
	"gitlab.com/kyura4k/gitnews/internal/postproc/adapt"
	"gitlab.com/kyura4k/gitnews/internal/postproc/finalize"
	"gitlab.com/kyura4k/gitnews/internal/store"
	trunner "gitlab.com/kyura4k/gitnews/internal/torrent/runner"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
	"gitlab.com/kyura4k/gitnews/internal/web"
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
	// minimum; zero means adapt.DefaultMargin.
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
	binding  *bindingWatcher
	restarts *restartWatcher
	speed    *speedApplier
	switches []namedSwitch

	ran atomic.Bool
}

// namedSwitch is a network kill switch to run as a worker.
type namedSwitch struct {
	name string
	ks   *netbind.Killswitch
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

	var managed func(core.ID) bool
	if trun != nil {
		managed = trun.Managed
	}
	post, err := newPostProcessor(downloads, sources, run, bus, dirFor, minFree, postprocConcurrency(settings.Current(), tune), tune, log,
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
		settings: settings.Current, categories: categories, holder: run, dirFor: dirFor, log: log,
		torrentHolder: torrentHolder, torrentDir: torrentDir,
	}
	downloadService := app.NewDownloadService(downloads, categories, bus, app.WithDataRemover(remover))
	shareLimits.downloads = downloadService
	track := newTracker()
	diskSpace := statfs
	if tune.FreeSpace != nil {
		diskSpace = func(path string) (int64, int64, bool) {
			free, err := tune.FreeSpace(path)
			return int64(min(free, math.MaxInt64)), int64(min(free, math.MaxInt64)), err == nil //nolint:gosec // clamped
		}
	}
	adapterCreds := auth.NewAdapterCredentialService(store.NewAdapterCredentialRepo(db), auth.WithAdapterLogger(log))
	addOpts := []app.Option{app.WithLogger(log)}
	if trun != nil {
		addOpts = append(addOpts, app.WithTorrentSources(torrentSources))
	}
	adder := app.NewAddService(downloadService, sources, settings, addOpts...)
	categoryService := app.NewCategoryService(categories, downloads, bus)
	var qbitHandler http.Handler
	var webTorrentSource web.TorrentSource
	if trun != nil {
		torrents := newQBitTorrents(downloads, torrentSources, torrentRecords, engine, trun, settings.Current, log)
		webTorrentSource = newWebTorrents(torrents)
		qbitHandler = qbit.New(qbit.Deps{
			Credentials: adapterCreds, Downloads: downloadService, Categories: categoryService, Settings: settings,
			Torrents: torrents, Files: torrents, ShareLimits: torrents,
			Adder:       &qbitAdder{add: adder, limits: torrents, log: log},
			GlobalShare: qbitGlobalShare(settings.Current), Logger: log,
		})
	}
	serverService := app.NewServerService(servers, bus, prober)
	apiKeys := auth.NewAPIKeyService(store.NewAPIKeyRepo(db), auth.WithAPIKeyLogger(log))
	runtime := newWebRuntime(run, torrentRunnerOrNil(trun), engine, downloads, track)
	// The speed limits of the Settings are in force from the start, before
	// the runners run (the torrent runner passes them to its engine then).
	speed := &speedApplier{settings: settings.Current, target: runtime}
	speed.apply()
	started := settings.Current()
	webUI, err := web.New(web.Deps{
		Logger: log, Version: deps.Version.Version,
		Sessions: sessions, UIAuth: uiAuth, Setup: setup, Settings: settings,
		Downloads: downloadService, Categories: categoryService, Servers: serverService, Bus: bus,
		Adder: adder, APIKeys: apiKeys, AdapterCredentials: adapterCreds,
		Runtime: runtime, Torrents: webTorrentSource, Config: &webConfig{started: started, portOverride: deps.Torrent.Port},
		Detail: webDetail{run: run, reports: reports{repo: store.NewUsenetReportRepo(db)}, track: track},
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
		Sessions:   sessions,
		UIAuth:     uiAuth,
		APIKeys:    apiKeys,
		Bus:        bus,

		AdapterCredentials: adapterCreds,
		Web:                webUI,
		QBit:               qbitHandler,
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
	d = &Daemon{
		log: log, bus: bus, server: server, runner: run, post: post, sessions: sessions, cleanup: cleanup,
		dirs: dirs, track: track, torrents: trun, owned: owned,
		binding:  &bindingWatcher{start: started.Network, current: settings.Current, log: log},
		restarts: &restartWatcher{start: restartOnlyOf(started), current: settings.Current, log: log},
		speed:    speed,
	}
	d.switches = networkSwitches(settings.Current().Network, run, trun, log, tune)
	return d, nil
}

// networkSwitches builds a kill switch for each protocol whose binding is
// on. When the bound interface goes away the protocol's Queue is paused; when
// it returns, the Queue is resumed, but only if the kill switch paused it.
func networkSwitches(n core.NetworkSettings, usenet *runner.Runner, torrents *trunner.Runner, log *slog.Logger, tune Tuning) []namedSwitch {
	var out []namedSwitch
	add := func(name string, b core.ProtocolBinding, k *bindingKillswitch) {
		if !b.Enabled() {
			return
		}
		out = append(out, namedSwitch{name: name, ks: netbind.NewKillswitch(netbind.KillswitchConfig{
			Binding:  netbind.Binding{Interface: b.Interface, Address: b.Address},
			Interval: tune.BindingInterval, Pause: k.onPause, Resume: k.onResume,
		})})
	}
	add("usenet", n.Usenet, &bindingKillswitch{
		name: "usenet", log: log,
		pause: func(string) bool {
			if paused, _ := usenet.QueuePaused(); paused {
				return false // somebody else paused it: not ours to resume
			}
			usenet.PauseQueue()
			return true
		},
		resume: usenet.ResumeQueue,
	})
	if torrents != nil {
		add("torrent", n.Torrent, &bindingKillswitch{
			name: "torrent", log: log,
			pause: func(reason string) bool {
				torrents.PauseQueueFor(trunner.ReasonBindUnavailable, reason)
				return true
			},
			resume: func() { torrents.ResumeQueueFor(trunner.ReasonBindUnavailable) },
		})
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
	minFree func(context.Context) (uint64, error), concurrency int, tune Tuning, log *slog.Logger, finisher core.Finisher,
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
	return core.NewPostProcessor(core.PostProcessorConfig{
		Downloads: downloads, Bus: bus,
		Verifier: adapt.Verifier{DeferredBlocks: run.DeferredBlocks},
		Repairer: adapt.Repairer{},
		Unpacker: adapt.Unpacker{Passwords: passwords, MinFreeFunc: func() uint64 { v, _ := minFree(context.Background()); return v }, FreeSpace: tune.FreeSpace},
		Finisher: finisher, FetchReporter: torrentFetchReporter{usenet: run},
		DeferredVolumes: &adapt.DeferredVolumes{Controller: run, Requeue: requeue, Margin: tune.VolumeMargin},
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
	if err := d.runner.Recover(ctx); err != nil && ctx.Err() == nil {
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
	start("network binding watcher", func(ctx context.Context) error {
		d.binding.run(ctx, d.bus)
		return nil
	})
	start("restart watcher", func(ctx context.Context) error {
		d.restarts.run(ctx, d.bus)
		return nil
	})
	start("speed limits", func(ctx context.Context) error {
		d.speed.run(ctx, d.bus)
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

// torrentRunnerOrNil converts the torrent runner to the interface the UI's
// runtime needs, keeping a nil runner a nil interface.
func torrentRunnerOrNil(r *trunner.Runner) torrentQueue {
	if r == nil {
		return nil
	}
	return r
}
