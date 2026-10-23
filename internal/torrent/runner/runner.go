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
	"gitlab.com/kyura4k/gitnews/internal/queue"
)

// Runner runs the torrent Queue. Create it with New and call Run once.
type Runner struct {
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
	engine core.TorrentEngine
	q      *queue.Queue
	wake   chan struct{}

	mu        sync.Mutex
	runCtx    context.Context
	tors      map[core.ID]*tor
	rateDown  int64
	rateUp    int64
	bad       map[core.ID]struct{}      // Downloads whose torrent cannot be added
	hashes    map[core.ID]core.InfoHash // remembered info hashes, see hashOf
	needSweep bool
	recMu     sync.Mutex // serialises read-modify-write of the records
	throttle  *core.ProgressThrottle
	wg        sync.WaitGroup
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
	r := &Runner{cfg: cfg, log: cfg.Logger, now: cfg.Now, engine: cfg.Engine}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.now == nil {
		r.now = time.Now
	}
	free := cfg.FreeSpace
	if free == nil {
		free = fsutil.FreeBytes
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
	r.bad = map[core.ID]struct{}{}
	r.hashes = map[core.ID]core.InfoHash{}
	q, err := queue.New(queue.Config{
		Source: core.SourceTorrent, Downloads: cfg.Downloads, Bus: cfg.Bus, Settings: cfg.Settings,
		Protocol: protocol{r}, MinFree: cfg.MinFree, FreeSpace: free, Now: r.now, Wake: r.poke, Logger: r.log,
	})
	if err != nil {
		return nil, fmt.Errorf("torrent runner: %w", err)
	}
	r.q = q
	return r, nil
}

// Queue returns the torrent Queue: its Pause reasons, holds and state
// changes.
func (r *Runner) Queue() *queue.Queue { return r.q }

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

// Recover moves the torrent Downloads found Fetching back to Queued and adds
// the torrents that are Seeding (Fetched or later, with their data still on
// disk) to the engine again. Call it once at start-up, before Run: those
// Downloads were interrupted by a shutdown or a crash.
func (r *Runner) Recover(ctx context.Context) error {
	return errors.Join(r.q.Recover(ctx), r.sweep(ctx))
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
