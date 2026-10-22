package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// Runner runs the torrent Queue. Create it with New and call Run once.
type Runner struct {
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
	free   func(string) (uint64, error)
	engine core.TorrentEngine
	wake   chan struct{}

	mu         sync.Mutex
	runCtx     context.Context
	tors       map[core.ID]*tor
	holds      map[string]string // Queue pause reason -> detail
	diskPaused bool
	rateDown   int64
	rateUp     int64
	bad        map[core.ID]struct{} // Downloads whose torrent cannot be added
	held       map[core.ID]time.Time
	needSweep  bool
	recMu      sync.Mutex // serialises read-modify-write of the records
	throttle   *core.ProgressThrottle
	wg         sync.WaitGroup
}

// New validates cfg and returns a Runner.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Downloads == nil:
		return nil, errors.New("torrent runner: Config.Downloads is required")
	case cfg.Sources == nil:
		return nil, errors.New("torrent runner: Config.Sources is required")
	case cfg.Records == nil:
		return nil, errors.New("torrent runner: Config.Records is required")
	case cfg.Engine == nil:
		return nil, errors.New("torrent runner: Config.Engine is required")
	case cfg.Bus == nil:
		return nil, errors.New("torrent runner: Config.Bus is required")
	case cfg.Settings == nil:
		return nil, errors.New("torrent runner: Config.Settings is required")
	}
	r := &Runner{cfg: cfg, log: cfg.Logger, now: cfg.Now, free: cfg.FreeSpace, engine: cfg.Engine}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.free == nil {
		r.free = fsutil.FreeBytes
	}
	r.cfg.PollInterval = orDefault(cfg.PollInterval, defaultPollInterval)
	r.cfg.SampleInterval = orDefault(cfg.SampleInterval, defaultSampleInterval)
	r.cfg.DoneBytesInterval = orDefault(cfg.DoneBytesInterval, defaultDoneBytesInterval)
	r.cfg.PersistInterval = orDefault(cfg.PersistInterval, defaultPersistInterval)
	r.cfg.LimitInterval = orDefault(cfg.LimitInterval, defaultLimitInterval)
	r.cfg.DiskCheckInterval = orDefault(cfg.DiskCheckInterval, defaultDiskCheckInterval)
	r.throttle = core.NewProgressThrottle(r.now, orDefault(cfg.ProgressInterval, core.DefaultProgressInterval))
	r.wake = make(chan struct{}, 1)
	r.tors = map[core.ID]*tor{}
	r.holds = map[string]string{}
	r.bad = map[core.ID]struct{}{}
	r.held = map[core.ID]time.Time{}
	return r, nil
}

// poke asks the control loop to run a pass soon.
func (r *Runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run drives the Queue until ctx ends. Torrents stay in the engine when it
// ends (the daemon closes the engine after the workers have stopped); Run
// returns ctx.Err() once every background step has stopped.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.runCtx != nil {
		r.mu.Unlock()
		return errors.New("torrent runner: Run called twice")
	}
	r.runCtx = ctx
	r.mu.Unlock()
	defer r.shutdown()

	sub := r.cfg.Bus.Subscribe(ctx)
	defer func() { sub.Close() }()
	poll := time.NewTicker(r.cfg.PollInterval)
	defer poll.Stop()

	disk := time.NewTicker(r.cfg.DiskCheckInterval)
	defer disk.Stop()
	sample := time.NewTicker(r.cfg.SampleInterval)
	defer sample.Stop()
	persist := time.NewTicker(r.cfg.PersistInterval)
	defer persist.Stop()
	limits := time.NewTicker(r.cfg.LimitInterval)
	defer limits.Stop()

	events := r.engine.Events()
	r.applyRateLimits()
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				events = nil // the engine is closed
				continue
			}
			r.onEngineEvent(ctx, ev)
		case <-persist.C:
			r.persistAll(ctx)
		case <-limits.C:
			r.evaluateLimits(ctx)
		case <-sample.C:
			r.sample(ctx)
		case <-disk.C:
			r.checkDisk(ctx)
		case ev, ok := <-sub.Events():
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// The bus dropped us for being slow: we missed events.
				sub = r.cfg.Bus.Subscribe(ctx)
				r.reconcile(ctx)
				continue
			}
			if ev.Kind().IsProgress() {
				continue
			}
			if rm, isRm := ev.(core.Removed); isRm {
				r.drop(ctx, rm.ID)
				r.Release(rm.ID)
			}
			r.reconcile(ctx)
		case <-poll.C:
			r.reconcile(ctx)
			r.sweep(ctx)
		case <-r.wake:
			r.reconcile(ctx)
			if r.takeSweep() {
				r.sweep(ctx)
			}
		}
	}
}

// shutdown stops the background steps and waits for them.
func (r *Runner) shutdown() {
	r.mu.Lock()
	for _, t := range r.tors {
		t.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.persistAll(ctx)
	r.mu.Lock()
	r.runCtx = nil
	r.mu.Unlock()
}

// listQueue returns the Queued and Fetching torrent Downloads.
func (r *Runner) listQueue(ctx context.Context) ([]core.Download, error) {
	var out []core.Download
	for _, st := range []core.State{core.StateQueued, core.StateFetching} {
		ds, err := r.list(ctx, st)
		if err != nil {
			return nil, err
		}
		out = append(out, ds...)
	}
	return out, nil
}

// list returns every torrent Download in state st.
func (r *Runner) list(ctx context.Context, st core.State) ([]core.Download, error) {
	var out []core.Download
	for after := ""; ; {
		page, next, err := r.cfg.Downloads.List(ctx, core.DownloadQuery{
			State: st, SourceKind: core.SourceTorrent, After: after, Limit: 200,
		})
		if err != nil {
			return nil, fmt.Errorf("torrent runner: list %s downloads: %w", st, err)
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		after = next
	}
}

// setState applies c to the stored Download and publishes DownloadUpdated.
func (r *Runner) setState(ctx context.Context, id core.ID, c core.StateChange) (core.Download, error) {
	d, err := r.cfg.Downloads.UpdateState(ctx, id, c)
	if err != nil {
		return core.Download{}, err
	}
	r.cfg.Bus.Publish(core.DownloadUpdated{Download: d})
	return d, nil
}

// Recover moves the torrent Downloads found Fetching back to Queued and adds
// the torrents that are Seeding (Fetched or later, with their data still on
// disk) to the engine again. Call it once at start-up, before Run: those
// Downloads were interrupted by a shutdown or a crash.
func (r *Runner) Recover(ctx context.Context) error {
	var errs []error
	fetching, err := r.list(ctx, core.StateFetching)
	if err != nil {
		return fmt.Errorf("torrent runner: recover: %w", err)
	}
	for _, d := range fetching {
		if _, err := r.setState(ctx, d.ID, core.StateChange{To: core.StateQueued, At: r.now()}); err != nil {
			errs = append(errs, fmt.Errorf("torrent runner: recover %s: %w", d.ID, err))
		}
	}
	if err := r.sweep(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// SetRateLimits sets the global download and upload limits of the torrent
// engine in bytes per second; zero or less is unlimited.
func (r *Runner) SetRateLimits(down, up int64) {
	r.mu.Lock()
	r.rateDown, r.rateUp = down, up
	running := r.runCtx != nil
	r.mu.Unlock()
	if running {
		r.applyRateLimits()
	}
}

func (r *Runner) applyRateLimits() {
	r.mu.Lock()
	down, up := r.rateDown, r.rateUp
	r.mu.Unlock()
	if err := r.engine.SetRateLimits(down, up); err != nil {
		r.log.Warn("torrent runner: cannot set the rate limits", "err", err)
	}
}

// onEngineEvent handles an event of the engine.
func (r *Runner) onEngineEvent(ctx context.Context, ev core.TorrentEvent) {
	t := r.lookupHash(ev.Hash)
	if t == nil {
		return
	}
	switch ev.Kind {
	case core.TorrentMetadataReceived:
		r.goStep(t, func(ctx context.Context) { r.onMetadata(ctx, t) })
	case core.TorrentFetched:
		r.fetched(t)
	case core.TorrentErrored:
		r.goStep(t, func(ctx context.Context) { r.onTorrentError(ctx, t, ev.Err) })
	}
}
