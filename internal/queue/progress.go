package queue

import (
	"context"
	"errors"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// DefaultDoneBytesInterval is how often a running Download's DoneBytes is
// stored when Config.DoneBytesInterval is zero.
const DefaultDoneBytesInterval = 2 * time.Second

// Progress reports how far a running Download of the Queue has come, for
// both protocols alike: it publishes a core.DownloadProgress at most once per
// progress interval per Download (the one with done == total always goes
// out) and stores DoneBytes and TotalBytes at most once per
// Config.DoneBytesInterval. It reports whether it published.
func (q *Queue) Progress(ctx context.Context, id core.ID, done, total, rate int64) bool {
	published := q.throttle.Allow(id, done, total)
	if published {
		q.cfg.Bus.Publish(core.DownloadProgress{ID: id, DoneBytes: done, TotalBytes: total, BytesPerSecond: rate})
	}
	now := q.cfg.Now()
	q.mu.Lock()
	last, seen := q.stored[id]
	due := !seen || now.Sub(last) >= q.cfg.DoneBytesInterval
	if due {
		q.stored[id] = now
	}
	q.mu.Unlock()
	if due {
		if err := q.cfg.Downloads.UpdateProgress(ctx, id, done, total); err != nil && ctx.Err() == nil &&
			!errors.Is(err, core.ErrNotFound) {
			q.log.Warn("queue: cannot store progress", "id", id, "err", err)
		}
	}
	return published
}

// ForgetProgress drops what Progress remembers of a Download that stopped
// running, so its next run publishes and stores at once.
func (q *Queue) ForgetProgress(id core.ID) {
	q.throttle.Flush(id)
	q.mu.Lock()
	delete(q.stored, id)
	q.mu.Unlock()
}
