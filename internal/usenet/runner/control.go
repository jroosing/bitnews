package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/queue"
)

// protocol is what the Usenet runner gives its Queue (queue.Protocol): the
// Servers, the jobs and the cooldown after a failure.
type protocol struct{ r *Runner }

var _ queue.Protocol = protocol{}

// Prepare syncs the Servers, cancels the jobs whose Download left the Queue,
// expires cooldowns, and reports whether any Server can take a fetch.
func (p protocol) Prepare(ctx context.Context) bool {
	r := p.r
	r.syncServers(ctx)
	r.dropStaleJobs(ctx)
	now := r.clock.Now()
	r.mu.Lock()
	for id, until := range r.cooldown {
		if !now.Before(until) {
			delete(r.cooldown, id)
		}
	}
	r.mu.Unlock()
	if len(r.pools.active()) > 0 {
		return true
	}
	if r.anyQueued(ctx) {
		r.logNoServers(now)
	}
	return false
}

// Exclude keeps a Download that cools down after a failure out of the pass,
// so it stays Queued until its cooldown ends.
func (p protocol) Exclude(id core.ID) bool { return p.r.coolingDown(id) }

// Running reports whether a job of the Download runs.
func (p protocol) Running(id core.ID) bool { return p.r.hasJob(id) }

// Start launches the fetch of d, which the Queue has moved to Fetching.
func (p protocol) Start(ctx context.Context, d core.Download) error {
	if p.r.coolingDown(d.ID) {
		return queue.ErrNotNow
	}
	return p.r.startJob(ctx, d)
}

// Stop cancels the job; its resume state is kept.
func (p protocol) Stop(_ context.Context, id core.ID) { p.r.cancelJob(id, errDeactivated) }

// Abandon cancels the job as if the Download were removed (its resume state
// goes) and waits until it has stopped.
func (p protocol) Abandon(ctx context.Context, id core.ID) error {
	r := p.r
	r.mu.Lock()
	j := r.jobs[id]
	r.mu.Unlock()
	if j == nil {
		return nil
	}
	j.cancel(errRemoved)
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("runner: waiting for the fetch of %s to stop: %w", id, ctx.Err())
	}
}

// dropStaleJobs cancels, as removed, the jobs whose Download is gone, is no
// longer Queued or Fetching, or is held.
func (r *Runner) dropStaleJobs(ctx context.Context) {
	r.mu.Lock()
	ids := make([]core.ID, 0, len(r.jobs))
	for id := range r.jobs {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		d, err := r.cfg.Downloads.Get(ctx, id)
		switch {
		case errors.Is(err, core.ErrNotFound):
		case err != nil:
			continue // cannot tell: leave it for the next pass
		case (d.State == core.StateQueued || d.State == core.StateFetching) && !r.q.Held(id):
			continue
		}
		r.cancelJob(id, errRemoved)
	}
}

// anyQueued reports whether an NZB Download waits to be fetched.
func (r *Runner) anyQueued(ctx context.Context) bool {
	for _, st := range []core.State{core.StateQueued, core.StateFetching} {
		page, _, err := r.cfg.Downloads.List(ctx, core.DownloadQuery{State: st, SourceKind: core.SourceNZB, Limit: 1})
		if err == nil && len(page) > 0 {
			return true
		}
	}
	return false
}

func (r *Runner) hasJob(id core.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[id] != nil
}

func (r *Runner) coolingDown(id core.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clock.Now().Before(r.cooldown[id])
}

func (r *Runner) logNoServers(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.noServerLog) >= noServersLogInterval {
		r.noServerLog = now
		r.log.Warn("usenet runner: no enabled server is configured, downloads stay queued")
	}
}

// syncServers reads the Servers and updates the pools and, if the Server
// set changed, the running fetches.
func (r *Runner) syncServers(ctx context.Context) {
	specs, err := r.cfg.Servers.Servers(ctx)
	if err != nil {
		r.log.Error("usenet runner: cannot read servers", "err", err)
		return
	}
	if r.pools.sync(specs) {
		r.mu.Lock()
		for _, j := range r.jobs {
			j.reschedule()
		}
		r.mu.Unlock()
	}
}

// startJob launches the fetch of d, which is Fetching. It returns
// queue.ErrNotNow when the Download is held or its previous job has not
// ended yet.
func (r *Runner) startJob(ctx context.Context, d core.Download) error {
	jctx, cancel := context.WithCancelCause(ctx)
	j := newJob(d.ID, cancel)
	r.mu.Lock()
	// Checked under r.mu, which Running takes too: a Hold either sees this
	// job or is seen here.
	if r.q.Held(d.ID) || r.jobs[d.ID] != nil {
		r.mu.Unlock()
		cancel(nil)
		return queue.ErrNotNow
	}
	r.jobs[d.ID] = j
	r.mu.Unlock()
	r.wg.Go(func() {
		defer r.poke()
		defer close(j.done)
		defer func() {
			r.mu.Lock()
			delete(r.jobs, d.ID)
			r.mu.Unlock()
			cancel(nil)
		}()
		r.fetchOne(jctx, j, d)
	})
	return nil
}
