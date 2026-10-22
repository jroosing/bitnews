package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// markFetched reports true the first time it is called for t. The first
// caller runs the Fetched step.
func (r *Runner) markFetched(t *tor) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.fetched {
		return false
	}
	t.fetched = true
	return true
}

func (r *Runner) isFetched(id core.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tors[id]
	return t != nil && t.fetched
}

// fetched starts the Fetched step of t unless it was started already.
func (r *Runner) fetched(t *tor) {
	if r.markFetched(t) {
		r.goStep(t, func(ctx context.Context) { r.onFetched(ctx, t) })
	}
}

// onFetched handles a torrent whose pieces are all verified. Seeding goes on
// throughout. The data moves to its complete location first, and only then
// does the Download become Fetched: Post-processing starts at Fetched and
// must find the data where it stays. A crash in between leaves the Download
// Fetching, so after a restart the same step runs again.
func (r *Runner) onFetched(ctx context.Context, t *tor) {
	d, err := r.cfg.Downloads.Get(ctx, t.id)
	if err != nil {
		return
	}
	switch d.State {
	case core.StateFetchingMetadata:
		// The metadata step has not run yet; the sampler comes back.
		r.mu.Lock()
		t.fetched = false
		r.mu.Unlock()
		return
	case core.StateQueued:
		// Complete before the Queue activated it (data was there already).
		if d, err = r.setState(ctx, t.id, core.StateChange{To: core.StateFetching, At: r.now()}); err != nil {
			r.log.Warn("torrent runner: cannot activate a complete download", "id", t.id, "err", err)
			return
		}
	case core.StateFetching:
	default:
		return // Fetched or later: nothing left to do
	}
	if st, err := r.engine.Stats(t.hash); err == nil && st.Size > 0 {
		_ = r.cfg.Downloads.UpdateProgress(ctx, t.id, st.Size, st.Size)
	}
	r.throttle.Flush(t.id)
	r.moveToComplete(ctx, d, t)
	r.persistOne(context.WithoutCancel(ctx), t)
	if ctx.Err() != nil {
		return
	}
	if _, err := r.setState(ctx, t.id, core.StateChange{To: core.StateFetched, At: r.now()}); err != nil {
		if !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrIllegalTransition) {
			r.log.Warn("torrent runner: cannot mark the download Fetched", "id", t.id, "err", err)
		}
		return
	}
	r.cfg.Bus.Publish(core.Fetched{ID: t.id})
	r.poke()
}

// moveToComplete relocates the torrent to <complete>/<category>/<name>
// without a rehash. A failure is not fatal: the torrent keeps seeding where
// it is and Post-processing works from there.
func (r *Runner) moveToComplete(ctx context.Context, d core.Download, t *tor) {
	if r.cfg.Destination == nil {
		return
	}
	rec, err := r.loadRecord(ctx, t.id)
	if err != nil {
		r.log.Warn("torrent runner: cannot read the torrent record, not moving", "id", t.id, "err", err)
		return
	}
	cur := r.dirOf(t)
	if rec.Moved && rec.Location == cur {
		return // moved before a restart
	}
	base, err := r.cfg.Destination(ctx, d)
	if err != nil {
		r.log.Warn("torrent runner: no destination, the torrent stays where it is", "id", t.id, "err", err)
		return
	}
	if sameDir(cur, base) {
		return
	}
	target := fsutil.Unique(base)
	if err := fsutil.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		r.log.Warn("torrent runner: cannot create the destination, the torrent stays where it is", "id", t.id, "err", err)
		return
	}
	// Write the intent first: after a crash the data is either still at cur or
	// already at target, and the record says which one to look for.
	if err := r.updateRecord(ctx, t.id, func(rec *core.TorrentRecord) { rec.Location, rec.Moved = target, false }); err != nil {
		r.log.Warn("torrent runner: cannot record the move, not moving", "id", t.id, "err", err)
		return
	}
	if err := r.engine.Relocate(ctx, t.hash, target); err != nil {
		r.log.Warn("torrent runner: cannot move the torrent to complete, it keeps seeding where it is", "id", t.id, "err", err)
		_ = r.updateRecord(context.WithoutCancel(ctx), t.id, func(rec *core.TorrentRecord) { rec.Location, rec.Moved = "", false })
		return
	}
	ctx = context.WithoutCancel(ctx)
	r.mu.Lock()
	t.dir = target
	r.mu.Unlock()
	if err := r.updateRecord(ctx, t.id, func(rec *core.TorrentRecord) { rec.Location, rec.Moved = target, true }); err != nil {
		r.log.Warn("torrent runner: cannot record the move", "id", t.id, "err", err)
	}
	r.followTorrentFile(ctx, t.id, target)
}

// followTorrentFile points the stored source at the .torrent file in dir,
// where it moved with the rest of the Download's directory.
func (r *Runner) followTorrentFile(ctx context.Context, id core.ID, dir string) {
	src, err := r.cfg.Sources.Get(ctx, id)
	if err != nil || src.TorrentPath == "" {
		return
	}
	path := filepath.Join(dir, filepath.Base(src.TorrentPath))
	if path == src.TorrentPath {
		return
	}
	if err := r.cfg.Sources.SetTorrentPath(ctx, id, path); err != nil {
		r.log.Warn("torrent runner: cannot update the torrent file path", "id", id, "err", err)
	}
}

func (r *Runner) dirOf(t *tor) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return t.dir
}

// sameDir reports whether a and b are the same existing directory.
func sameDir(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}
