package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// reconcile is one pass of the control loop: it makes the set of running
// torrents match core.Select. It is only called from Run's goroutine.
func (r *Runner) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	settings, err := r.cfg.Settings(ctx)
	if err != nil {
		r.log.Error("torrent runner: cannot read settings", "err", err)
		return
	}
	r.reconcileMetadata(ctx)
	ds, err := r.listQueue(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("torrent runner: cannot list downloads", "err", err)
		}
		return
	}
	// Held Downloads are being removed; fetched ones are Seeding already and
	// only wait for their Fetched step to end.
	ds = slices.DeleteFunc(ds, func(d core.Download) bool { return r.isHeld(d.ID) || r.isFetched(d.ID) })
	activate, deactivate := core.Select(ds, settings.Queues.Torrent)
	root := settings.Paths.Torrent.Incomplete
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
	byID := make(map[core.ID]core.Download, len(ds))
	for _, d := range ds {
		byID[d.ID] = d
	}
	for _, id := range deactivate {
		r.deactivate(ctx, id)
	}
	// A Queued Download whose torrent is running (its metadata just arrived)
	// but was not chosen waits paused.
	for _, d := range ds {
		if d.State == core.StateQueued && !slices.Contains(activate, d.ID) {
			if t := r.lookup(d.ID); t != nil {
				r.setRunning(t, false)
			}
		}
	}
	// Fetching Downloads without a torrent (after a Hold, or left by a crash
	// if Recover was not called) are started again here.
	chosen := slices.Clone(activate)
	if paused, _ := r.QueuePaused(); !paused {
		for _, d := range ds {
			if d.State == core.StateFetching && r.lookup(d.ID) == nil && !slices.Contains(deactivate, d.ID) {
				chosen = append(chosen, d.ID)
			}
		}
	}
	for _, id := range chosen {
		d := byID[id]
		if ok, detail := r.diskFits(ctx, root, d.TotalBytes-d.DoneBytes); !ok {
			r.pauseDisk(detail)
			for _, other := range ds {
				if other.State == core.StateFetching {
					r.deactivate(ctx, other.ID)
				}
			}
			return
		}
		r.activate(ctx, d)
	}
}

// deactivate pauses the torrent of id and puts the Download back to Queued.
func (r *Runner) deactivate(ctx context.Context, id core.ID) {
	if t := r.lookup(id); t != nil {
		r.setRunning(t, false)
	}
	_, err := r.setState(ctx, id, core.StateChange{To: core.StateQueued, At: r.now()})
	if err != nil && !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrIllegalTransition) {
		r.log.Warn("torrent runner: cannot deactivate download", "id", id, "err", err)
	}
}

// activate starts the torrent of the Queued Download d and moves the
// Download to Fetching.
func (r *Runner) activate(ctx context.Context, d core.Download) {
	t, err := r.ensureRegistered(ctx, d, true)
	if err != nil {
		r.addFailed(ctx, d, err)
		return
	}
	r.setRunning(t, true)
	if d.State == core.StateFetching {
		return
	}
	if _, err := r.setState(ctx, d.ID, core.StateChange{To: core.StateFetching, At: r.now()}); err != nil {
		r.log.Warn("torrent runner: cannot activate download", "id", d.ID, "err", err)
	}
}

// PauseQueue stops the whole torrent Queue: running Downloads go back to
// Queued and nothing is activated until ResumeQueue. Seeding goes on.
func (r *Runner) PauseQueue() { r.PauseQueueFor(ReasonUser, "") }

// ResumeQueue undoes PauseQueue. It does not override other reasons.
func (r *Runner) ResumeQueue() { r.ResumeQueueFor(ReasonUser) }

// PauseQueueFor pauses the Queue for a reason of the caller, for example the
// network monitor's bind_unavailable. Each reason is held separately: the
// Queue runs again when every reason has been released.
func (r *Runner) PauseQueueFor(reason, detail string) {
	r.mu.Lock()
	_, was := r.holds[reason]
	wasPaused := r.pausedLocked()
	r.holds[reason] = detail
	r.mu.Unlock()
	if !was {
		if !wasPaused {
			r.log.Info("torrent queue paused", "reason", reason)
		}
		r.cfg.Bus.Publish(QueuePaused{Reason: reason, Detail: detail})
		r.poke()
	}
}

// ResumeQueueFor releases a reason given to PauseQueueFor.
func (r *Runner) ResumeQueueFor(reason string) {
	r.mu.Lock()
	_, was := r.holds[reason]
	delete(r.holds, reason)
	nowPaused := r.pausedLocked()
	r.mu.Unlock()
	if was && !nowPaused {
		r.cfg.Bus.Publish(QueueResumed{})
	}
	if was {
		r.poke()
	}
}

func (r *Runner) pausedLocked() bool { return len(r.holds) > 0 || r.diskPaused }

// QueuePaused reports whether the Queue is paused and why: ReasonDiskFull
// when the disk guard holds it, else another reason if one is held, else
// ReasonUser.
func (r *Runner) QueuePaused() (paused bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.diskPaused {
		return true, ReasonDiskFull
	}
	best := ""
	for reason := range r.holds {
		if reason == ReasonUser {
			continue
		}
		if best == "" || reason < best {
			best = reason
		}
	}
	switch {
	case best != "":
		return true, best
	case len(r.holds) > 0:
		return true, ReasonUser
	}
	return false, ""
}

func (r *Runner) diskHeld() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.diskPaused
}

// pauseDisk holds the Queue for lack of disk space.
func (r *Runner) pauseDisk(detail string) {
	r.mu.Lock()
	was := r.diskPaused
	r.diskPaused = true
	r.mu.Unlock()
	if !was {
		r.log.Warn("torrent queue paused: not enough free disk space", "detail", detail)
		r.cfg.Bus.Publish(QueuePaused{Reason: ReasonDiskFull, Detail: detail})
		r.poke()
	}
}

func (r *Runner) resumeDisk() {
	r.mu.Lock()
	was := r.diskPaused
	r.diskPaused = false
	nowPaused := r.pausedLocked()
	r.mu.Unlock()
	if was {
		r.log.Info("torrent queue: disk space is back")
		if !nowPaused {
			r.cfg.Bus.Publish(QueueResumed{})
		}
	}
}

func (r *Runner) minFree(ctx context.Context) uint64 {
	if r.cfg.MinFree == nil {
		return DefaultMinFree
	}
	v, err := r.cfg.MinFree(ctx)
	if err != nil {
		r.log.Warn("torrent min_free unavailable, using the default", "err", err)
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
	if err != nil {
		r.log.Warn("torrent disk guard: cannot read free space", "path", root, "err", err)
		return true, ""
	}
	need := uint64(max(remaining, 0)) + r.minFree(ctx)
	if free >= need {
		return true, ""
	}
	return false, fmt.Sprintf("%d bytes free, %d needed", free, need)
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

// checkDisk rechecks the free space for the running torrents and holds the
// Queue when one of them no longer fits.
func (r *Runner) checkDisk(ctx context.Context) {
	if r.diskHeld() {
		return
	}
	settings, err := r.cfg.Settings(ctx)
	if err != nil {
		return
	}
	root := settings.Paths.Torrent.Incomplete
	for _, t := range r.runningTors() {
		st, err := r.engine.Stats(t.hash)
		if err != nil || st.Size <= 0 {
			continue
		}
		if ok, detail := r.diskFits(ctx, root, st.Size-st.DoneBytes); !ok {
			r.pauseDisk(detail)
			return
		}
	}
}

// runningTors lists the torrents the Queue has started.
func (r *Runner) runningTors() []*tor {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*tor
	for _, t := range r.tors {
		if t.running && !t.fetched {
			out = append(out, t)
		}
	}
	return out
}

// holdTTL is how long a Hold lasts unless Release or the Removed event of the
// Download ends it first. It keeps a Download whose removal failed half-way
// from staying parked for ever.
const holdTTL = 2 * time.Minute

// Hold lets the engine go of the torrent of the Download, waits until any
// background step on it has stopped, and keeps the control loop from adding
// it again until Release, the Removed event of the Download, or holdTTL. The
// daemon calls it before deleting the Download's files; it returns at once
// for a Download the engine does not have. ctx bounds the wait.
func (r *Runner) Hold(ctx context.Context, id core.ID) error {
	r.mu.Lock()
	r.held[id] = r.now().Add(holdTTL)
	t := r.tors[id]
	r.mu.Unlock()
	if t == nil {
		return nil
	}
	t.cancel()
	// Waiting for the step slot waits for a running step to end.
	select {
	case t.op <- struct{}{}:
		t.unlock()
	case <-ctx.Done():
		return fmt.Errorf("torrent runner: waiting for the torrent of %s to settle: %w", id, ctx.Err())
	}
	r.drop(ctx, id)
	return nil
}

// Release undoes Hold.
func (r *Runner) Release(id core.ID) {
	r.mu.Lock()
	_, was := r.held[id]
	delete(r.held, id)
	if was {
		r.needSweep = true
	}
	r.mu.Unlock()
	if was {
		r.poke()
	}
}

func (r *Runner) isHeld(id core.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.held[id]
	if !ok {
		return false
	}
	if !r.now().Before(until) {
		delete(r.held, id)
		return false
	}
	return true
}
