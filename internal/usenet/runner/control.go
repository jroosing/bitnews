package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// PauseQueue stops the whole Usenet Queue: running Downloads go back to
// Queued and nothing is activated until ResumeQueue.
func (r *Runner) PauseQueue() {
	r.mu.Lock()
	was := r.userPaused
	r.userPaused = true
	r.mu.Unlock()
	if !was {
		r.cfg.Bus.Publish(QueuePaused{Reason: ReasonUser})
		r.poke()
	}
}

// ResumeQueue undoes PauseQueue. It does not override the disk guard.
func (r *Runner) ResumeQueue() {
	r.mu.Lock()
	was := r.userPaused
	r.userPaused = false
	r.mu.Unlock()
	if was {
		r.cfg.Bus.Publish(QueueResumed{})
		r.poke()
	}
}

// QueuePaused reports whether the Queue is paused and why: ReasonDiskFull
// when the disk guard holds it, else ReasonUser.
func (r *Runner) QueuePaused() (paused bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.diskPaused:
		return true, ReasonDiskFull
	case r.userPaused:
		return true, ReasonUser
	}
	return false, ""
}

// pauseDisk holds the Queue for lack of disk space.
func (r *Runner) pauseDisk(detail string) {
	r.mu.Lock()
	was := r.diskPaused
	r.diskPaused = true
	r.mu.Unlock()
	if !was {
		r.log.Warn("usenet queue paused: not enough free disk space", "detail", detail)
		r.cfg.Bus.Publish(QueuePaused{Reason: ReasonDiskFull, Detail: detail})
		r.poke()
	}
}

func (r *Runner) resumeDisk() {
	r.mu.Lock()
	was := r.diskPaused
	r.diskPaused = false
	r.mu.Unlock()
	if was {
		r.log.Info("usenet queue resumed: disk space is back")
		r.cfg.Bus.Publish(QueueResumed{})
	}
}

func (r *Runner) minFree(ctx context.Context) uint64 {
	if r.cfg.MinFree == nil {
		return DefaultMinFree
	}
	v, err := r.cfg.MinFree(ctx)
	if err != nil {
		r.log.Warn("usenet min_free unavailable, using the default", "err", err)
		return DefaultMinFree
	}
	return v
}

// diskFits applies the disk guard: the incomplete root must have at least
// remaining plus min_free bytes free. It passes when the guard is disabled or
// free space cannot be read.
func (r *Runner) diskFits(ctx context.Context, root string, remaining int64) (ok bool, detail string) {
	if r.free == nil {
		return true, ""
	}
	free, err := r.free(root)
	if errors.Is(err, fsutil.ErrUnsupported) {
		return true, "" // no probe on this platform: the guard is off
	}
	if err != nil {
		r.log.Warn("usenet disk guard: cannot read free space", "path", root, "err", err)
		return true, ""
	}
	need := uint64(max(remaining, 0)) + r.minFree(ctx)
	if free >= need {
		return true, ""
	}
	return false, fmt.Sprintf("%d bytes free, %d needed", free, need)
}

// reconcile is one pass of the control loop: it makes the set of running
// fetches match core.Select. It is only called from Run's goroutine.
func (r *Runner) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	settings, err := r.cfg.Settings(ctx)
	if err != nil {
		r.log.Error("usenet runner: cannot read settings", "err", err)
		return
	}
	r.syncServers(ctx)
	ds, err := r.listQueue(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("usenet runner: cannot list downloads", "err", err)
		}
		return
	}
	ds = slices.DeleteFunc(ds, func(d core.Download) bool { return r.isHeld(d.ID) })
	byID := make(map[core.ID]core.Download, len(ds))
	for _, d := range ds {
		byID[d.ID] = d
	}
	r.mu.Lock()
	for id, j := range r.jobs {
		if _, ok := byID[id]; !ok {
			j.cancel(errRemoved)
		}
	}
	now := r.clock.Now()
	for id, until := range r.cooldown {
		if !now.Before(until) {
			delete(r.cooldown, id)
		}
	}
	r.mu.Unlock()

	root := settings.Paths.Usenet.Incomplete
	activate, deactivate := core.Select(ds, settings.Queues.Usenet)
	if r.diskHeld() && r.diskRecovered(ctx, root, ds, activate) {
		r.resumeDisk()
	}
	if paused, _ := r.QueuePaused(); paused {
		activate, deactivate = nil, nil
		for _, d := range ds {
			if d.State == core.StateFetching {
				deactivate = append(deactivate, d.ID)
			}
		}
	}
	for _, id := range deactivate {
		r.deactivate(ctx, id)
	}

	// Fetching Downloads without a job (left by a crash, if Recover was
	// not called) are chosen again here.
	chosen := activate
	for _, d := range ds {
		if d.State == core.StateFetching && !r.hasJob(d.ID) && !contains(deactivate, d.ID) {
			if paused, _ := r.QueuePaused(); !paused {
				chosen = append(chosen, d.ID)
			}
		}
	}
	if len(chosen) == 0 {
		return
	}
	if len(r.pools.active()) == 0 {
		r.logNoServers(now)
		return
	}
	for _, id := range chosen {
		d := byID[id]
		if r.hasJob(id) || r.coolingDown(id) {
			continue
		}
		if ok, detail := r.diskFits(ctx, root, d.TotalBytes-d.DoneBytes); !ok {
			r.pauseDisk(detail)
			for _, other := range ds {
				if other.State == core.StateFetching {
					r.deactivate(ctx, other.ID)
				}
			}
			return
		}
		if d.State == core.StateQueued {
			d, err = r.setState(ctx, id, core.StateChange{To: core.StateFetching, At: r.clock.Now()})
			if err != nil {
				r.log.Warn("usenet runner: cannot activate download", "id", id, "err", err)
				continue
			}
		} else {
			// The list may be stale: a job that finished meanwhile left
			// the Download Fetched, and starting it again would wipe its
			// files. State changes precede a job's removal from r.jobs, so
			// Fetching without a job here is a genuine orphan.
			cur, gerr := r.cfg.Downloads.Get(ctx, id)
			if gerr != nil || cur.State != core.StateFetching || r.hasJob(id) {
				continue
			}
			d = cur
		}
		r.startJob(ctx, d)
	}
}

func contains(ids []core.ID, id core.ID) bool {
	return slices.Contains(ids, id)
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

func (r *Runner) diskHeld() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.diskPaused
}

// diskRecovered reports whether the Download that would be activated next
// fits on disk again.
func (r *Runner) diskRecovered(ctx context.Context, root string, ds []core.Download, activate []core.ID) bool {
	var remaining int64
	if len(activate) > 0 {
		for _, d := range ds {
			if d.ID == activate[0] {
				remaining = d.TotalBytes - d.DoneBytes
			}
		}
	}
	ok, _ := r.diskFits(ctx, root, remaining)
	return ok
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

// deactivate stops the fetch of id and puts the Download back to Queued.
func (r *Runner) deactivate(ctx context.Context, id core.ID) {
	r.cancelJob(id, errDeactivated)
	_, err := r.setState(ctx, id, core.StateChange{To: core.StateQueued, At: r.clock.Now()})
	if err != nil && !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrIllegalTransition) {
		r.log.Warn("usenet runner: cannot deactivate download", "id", id, "err", err)
	}
}

// startJob launches the fetch of d, which is Fetching.
func (r *Runner) startJob(ctx context.Context, d core.Download) {
	jctx, cancel := context.WithCancelCause(ctx)
	j := newJob(d.ID, cancel)
	r.mu.Lock()
	if r.heldLocked(d.ID) {
		// Held between the control loop's check and now: leave it.
		r.mu.Unlock()
		cancel(nil)
		return
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
}

// holdTTL is how long a Hold lasts unless Release or a Removed event ends it
// first. It keeps a Download whose removal failed half-way from staying
// parked for ever.
const holdTTL = 2 * time.Minute

// Hold stops the fetch of the Download, waits until it has stopped, and
// keeps the Download from being activated again until Release, the Removed
// event of the Download, or holdTTL. A fetch that is cancelled this way
// forgets its resume state, as if the Download were removed. Call it before
// deleting the files of a Download that still exists; it returns at once
// for a Download without a running fetch. ctx bounds the wait.
func (r *Runner) Hold(ctx context.Context, id core.ID) error {
	r.mu.Lock()
	r.held[id] = r.clock.Now().Add(holdTTL)
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

// Release undoes Hold.
func (r *Runner) Release(id core.ID) {
	r.mu.Lock()
	_, was := r.held[id]
	delete(r.held, id)
	r.mu.Unlock()
	if was {
		r.poke()
	}
}

func (r *Runner) isHeld(id core.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.heldLocked(id)
}

func (r *Runner) heldLocked(id core.ID) bool {
	until, ok := r.held[id]
	if !ok {
		return false
	}
	if !r.clock.Now().Before(until) {
		delete(r.held, id)
		return false
	}
	return true
}
