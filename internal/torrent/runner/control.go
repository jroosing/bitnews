package runner

import (
	"context"
	"fmt"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/queue"
)

// reconcile is one pass of the control loop: the Queue makes the set of
// running torrents match its policy. It is only called from Run's goroutine.
func (r *Runner) reconcile(ctx context.Context) { r.q.Reconcile(ctx) }

// protocol is what the runner gives its Queue.
type protocol struct{ r *Runner }

var _ queue.Protocol = protocol{}

// Prepare keeps the magnets that wait for their metadata in the engine; they
// are not part of the Queue's admission yet.
func (p protocol) Prepare(ctx context.Context) bool {
	p.r.reconcileMetadata(ctx)
	return true
}

// Exclude leaves out the torrents that are complete: they are Seeding
// already and only wait for their Fetched step to end.
func (p protocol) Exclude(id core.ID) bool { return p.r.isFetched(id) }

// Running reports whether the engine has the torrent of the Download, paused
// for the Queue or not: that is what Stop pauses, what Abandon drops and
// what a Fetching Download without one is restarted for.
func (p protocol) Running(id core.ID) bool { return p.r.lookup(id) != nil }

// Start adds the torrent of d to the engine, or resumes it there. A torrent
// that cannot be added is handled by addFailed (retried, or the Download
// fails).
func (p protocol) Start(ctx context.Context, d core.Download) error {
	t, err := p.r.ensureRegistered(ctx, d, true)
	if err != nil {
		p.r.addFailed(ctx, d, err)
		return nil
	}
	p.r.setRunning(t, true)
	return nil
}

// Stop pauses the torrent in the engine, which keeps its pieces.
func (p protocol) Stop(_ context.Context, id core.ID) {
	if t := p.r.lookup(id); t != nil {
		p.r.setRunning(t, false)
	}
}

// Abandon lets the engine go of the torrent (its files stay) once any
// background step on it has stopped.
func (p protocol) Abandon(ctx context.Context, id core.ID) error {
	t := p.r.lookup(id)
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
	p.r.drop(ctx, id)
	return nil
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

// Hold lets the engine go of the torrent of the Download, waits until any
// background step on it has stopped, and keeps the control loop from adding
// it again until Release, the Removed event of the Download, or the Queue's
// hold TTL. The daemon calls it before deleting the Download's files; it
// returns at once for a Download the engine does not have. ctx bounds the
// wait.
func (r *Runner) Hold(ctx context.Context, id core.ID) error { return r.q.Hold(ctx, id) }

// Release undoes Hold. A Seeding torrent is added to the engine again by the
// next sweep.
func (r *Runner) Release(id core.ID) {
	if !r.q.Release(id) {
		return
	}
	r.mu.Lock()
	r.needSweep = true
	r.mu.Unlock()
	r.poke()
}
