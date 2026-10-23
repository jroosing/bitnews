package runner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/queue"
)

// Why a job's context was cancelled (context.Cause).
var (
	errRemoved     = errors.New("runner: download removed")
	errDeactivated = errors.New("runner: download deactivated")
	errDiskFull    = errors.New("runner: not enough free disk space")
	errShutdown    = errors.New("runner: shutting down")
)

// Runner runs the Usenet Queue. Create it with New and call Run once.
type Runner struct {
	cfg      Config
	log      *slog.Logger
	clock    Clock
	q        *queue.Queue
	pools    *poolSet
	throttle *core.ProgressThrottle
	store    ProgressStore
	volumes  VolumeStore
	lim      *limiter
	wake     chan struct{}

	mu          sync.Mutex
	runCtx      context.Context
	jobs        map[core.ID]*job
	cooldown    map[core.ID]time.Time
	reports     map[core.ID]FetchDetail
	noServerLog time.Time
	wg          sync.WaitGroup
}

// New validates cfg and returns a Runner.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Downloads == nil:
		return nil, errors.New("runner: Config.Downloads is required")
	case cfg.Bus == nil:
		return nil, errors.New("runner: Config.Bus is required")
	case cfg.Servers == nil:
		return nil, errors.New("runner: Config.Servers is required")
	case cfg.Sources == nil:
		return nil, errors.New("runner: Config.Sources is required")
	case cfg.DirFor == nil:
		return nil, errors.New("runner: Config.DirFor is required")
	case cfg.Settings == nil:
		return nil, errors.New("runner: Config.Settings is required")
	}
	r := &Runner{cfg: cfg, log: cfg.Logger, clock: cfg.Clock, store: cfg.Progress}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.clock == nil {
		r.clock = systemClock{}
	}
	free := cfg.FreeSpace
	if free == nil {
		free = fsutil.FreeBytes
	}
	if r.store == nil {
		r.store = newMemStore()
	}
	if r.volumes = cfg.Volumes; r.volumes == nil {
		r.volumes = newMemVolumes()
	}
	r.cfg.PollInterval = orDefault(cfg.PollInterval, defaultPollInterval)
	r.cfg.DiskCheckInterval = orDefault(cfg.DiskCheckInterval, defaultDiskCheckInterval)
	r.cfg.SaveInterval = orDefault(cfg.SaveInterval, defaultSaveInterval)
	r.cfg.DoneBytesInterval = orDefault(cfg.DoneBytesInterval, defaultDoneBytesInterval)
	r.cfg.FailureBackoff = orDefault(cfg.FailureBackoff, defaultFailureBackoff)

	factory := cfg.Factory
	if factory == nil {
		factory = defaultFactory
	}
	r.pools = newPoolSet(factory, r.log)
	r.throttle = core.NewProgressThrottle(r.clock.Now, core.DefaultProgressInterval)
	r.lim = newLimiter(r.clock)
	r.wake = make(chan struct{}, 1)
	r.jobs = map[core.ID]*job{}
	r.cooldown = map[core.ID]time.Time{}
	r.reports = map[core.ID]FetchDetail{}
	q, err := queue.New(queue.Config{
		Source: core.SourceNZB, Downloads: cfg.Downloads, Bus: cfg.Bus, Settings: cfg.Settings,
		Protocol: protocol{r}, MinFree: cfg.MinFree, FreeSpace: free,
		Now: r.clock.Now, Wake: r.poke, Logger: r.log,
	})
	if err != nil {
		return nil, err
	}
	r.q = q
	return r, nil
}

// Queue returns the Usenet Queue: its pause reasons, holds and recovery.
func (r *Runner) Queue() *queue.Queue { return r.q }

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// poke asks the control loop to run a pass soon.
func (r *Runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run drives the Queue until ctx ends. Fetches still running when it ends
// stay Fetching (call Recover after a restart); their resume state is saved.
// Run returns ctx.Err() once every fetch has stopped.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.runCtx != nil {
		r.mu.Unlock()
		return errors.New("runner: Run called twice")
	}
	r.runCtx = ctx
	r.mu.Unlock()
	defer r.shutdown()

	sub := r.cfg.Bus.Subscribe(ctx)
	defer func() { sub.Close() }()
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	r.q.Reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// The bus dropped us for being slow: we missed events.
				sub = r.cfg.Bus.Subscribe(ctx)
				r.q.Reconcile(ctx)
				continue
			}
			if ev.Kind().IsProgress() {
				continue
			}
			if rm, isRm := ev.(core.Removed); isRm {
				r.cancelJob(rm.ID, errRemoved)
				r.q.Release(rm.ID)
			}
			r.q.Reconcile(ctx)
		case <-ticker.C:
			r.q.Reconcile(ctx)
		case <-r.wake:
			r.q.Reconcile(ctx)
		}
	}
}

// shutdown stops every fetch and closes the pools.
func (r *Runner) shutdown() {
	r.mu.Lock()
	for _, j := range r.jobs {
		j.cancel(errShutdown)
	}
	r.mu.Unlock()
	r.wg.Wait()
	r.pools.close()
	r.mu.Lock()
	r.runCtx = nil
	r.mu.Unlock()
}

func (r *Runner) cancelJob(id core.ID, cause error) {
	r.mu.Lock()
	j := r.jobs[id]
	r.mu.Unlock()
	if j != nil {
		j.cancel(cause)
	}
}
