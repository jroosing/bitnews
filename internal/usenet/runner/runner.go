package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
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
	free     func(string) (uint64, error)
	pools    *poolSet
	throttle *core.ProgressThrottle
	store    ProgressStore
	lim      *limiter
	wake     chan struct{}

	mu          sync.Mutex
	runCtx      context.Context
	jobs        map[core.ID]*job
	userPaused  bool
	diskPaused  bool
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
	r := &Runner{cfg: cfg, log: cfg.Logger, clock: cfg.Clock, free: cfg.FreeSpace, store: cfg.Progress}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.clock == nil {
		r.clock = systemClock{}
	}
	if r.free == nil {
		r.free = defaultFreeSpace
	}
	if r.free == nil {
		r.log.Warn("usenet disk guard disabled: no free-space provider")
	}
	if r.store == nil {
		r.store = newMemStore()
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
	return r, nil
}

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

	r.reconcile(ctx)
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
				r.reconcile(ctx)
				continue
			}
			if ev.Kind().IsProgress() {
				continue
			}
			if rm, isRm := ev.(core.Removed); isRm {
				r.cancelJob(rm.ID, errRemoved)
			}
			r.reconcile(ctx)
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.wake:
			r.reconcile(ctx)
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

// listQueue returns the Queued and Fetching NZB Downloads.
func (r *Runner) listQueue(ctx context.Context) ([]core.Download, error) {
	var out []core.Download
	for _, st := range []core.State{core.StateQueued, core.StateFetching} {
		after := ""
		for {
			page, next, err := r.cfg.Downloads.List(ctx, core.DownloadQuery{
				State: st, SourceKind: core.SourceNZB, After: after, Limit: 200,
			})
			if err != nil {
				return nil, fmt.Errorf("runner: list %s downloads: %w", st, err)
			}
			out = append(out, page...)
			if next == "" {
				break
			}
			after = next
		}
	}
	return out, nil
}

// setState applies c to the stored Download and publishes DownloadUpdated.
// It must not be tied to a context that is being cancelled.
func (r *Runner) setState(ctx context.Context, id core.ID, c core.StateChange) (core.Download, error) {
	d, err := r.cfg.Downloads.UpdateState(ctx, id, c)
	if err != nil {
		return core.Download{}, err
	}
	r.cfg.Bus.Publish(core.DownloadUpdated{Download: d})
	return d, nil
}

// Recover moves the Usenet Downloads found Fetching back to Queued. Call it
// once at start-up, before Run: those Downloads were interrupted by a
// shutdown or a crash.
func (r *Runner) Recover(ctx context.Context) error {
	var errs []error
	for after := ""; ; {
		page, next, err := r.cfg.Downloads.List(ctx, core.DownloadQuery{
			State: core.StateFetching, SourceKind: core.SourceNZB, After: after, Limit: 200,
		})
		if err != nil {
			return fmt.Errorf("runner: recover: %w", err)
		}
		for _, d := range page {
			_, err := r.setState(ctx, d.ID, core.StateChange{To: core.StateQueued, At: r.clock.Now()})
			if err != nil {
				errs = append(errs, fmt.Errorf("runner: recover %s: %w", d.ID, err))
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	return errors.Join(errs...)
}
