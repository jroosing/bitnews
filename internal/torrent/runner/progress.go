package runner

import (
	"context"
	"errors"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// sample reads the stats of the torrents the Queue runs, publishes
// throttled progress events and writes DoneBytes at most every
// Config.DoneBytesInterval.
func (r *Runner) sample(ctx context.Context) {
	for _, t := range r.runningTors() {
		st, err := r.engine.Stats(t.hash)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				r.log.Warn("torrent runner: the engine lost a torrent", "id", t.id)
			}
			continue
		}
		if st.Size > 0 && st.DoneBytes >= st.Size {
			r.fetched(t) // the event may have been dropped
			continue
		}
		if st.Size > 0 && r.throttle.Allow(t.id, st.DoneBytes, st.Size) {
			r.cfg.Bus.Publish(core.DownloadProgress{
				ID: t.id, DoneBytes: st.DoneBytes, TotalBytes: st.Size, BytesPerSecond: st.DownRate,
			})
		}
		r.storeDoneBytes(ctx, t, st)
	}
}

// storeDoneBytes writes the progress of t unless that was done lately.
func (r *Runner) storeDoneBytes(ctx context.Context, t *tor, st core.TorrentStats) {
	if st.Size <= 0 {
		return // nothing known yet
	}
	now := r.now()
	r.mu.Lock()
	due := t.lastWrite.IsZero() || now.Sub(t.lastWrite) >= r.cfg.DoneBytesInterval
	if due {
		t.lastWrite = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	if err := r.cfg.Downloads.UpdateProgress(ctx, t.id, st.DoneBytes, st.Size); err != nil && ctx.Err() == nil &&
		!errors.Is(err, core.ErrNotFound) {
		r.log.Warn("torrent runner: cannot store progress", "id", t.id, "err", err)
	}
}
