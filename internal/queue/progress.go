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

// DefaultDiskCheckInterval is how often a running Download rechecks the
// free space when Config.DiskCheckInterval is zero.
const DefaultDiskCheckInterval = 30 * time.Second

// Progress reports how far a running Download of the Queue has come, for
// both protocols alike: it publishes a core.DownloadProgress at most once per
// progress interval per Download (the one with done == total always goes
// out) and stores DoneBytes and TotalBytes at most once per
// Config.DoneBytesInterval. Every Config.DiskCheckInterval it applies the
// disk guard to the bytes still to come (CheckDisk). It reports whether it
// published.
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
	q.CheckDisk(ctx, id, max(total-done, 0))
	return published
}

// CheckDisk applies the disk guard to a running fetch of the Download that
// still needs remaining bytes, at most once per Config.DiskCheckInterval
// (the first call only starts the interval: the fetch was checked when it
// started). When the incomplete root no longer has remaining plus min_free
// bytes free, it pauses the Queue with ReasonDiskFull, which stops the
// fetches on the next pass, and returns false. Progress calls it; call it
// directly only for a fetch that reports no progress.
func (q *Queue) CheckDisk(ctx context.Context, id core.ID, remaining int64) bool {
	now := q.cfg.Now()
	q.mu.Lock()
	last, seen := q.diskChecked[id]
	due := seen && now.Sub(last) >= q.cfg.DiskCheckInterval
	if !seen || due {
		q.diskChecked[id] = now
	}
	q.mu.Unlock()
	if !due {
		return true
	}
	settings, err := q.cfg.Settings(ctx)
	if err != nil {
		return true
	}
	ok, detail := q.fits(ctx, q.root(settings), remaining)
	if !ok {
		q.Pause(ReasonDiskFull, detail)
	}
	return ok
}

// ForgetProgress drops what Progress remembers of a Download that stopped
// running, so its next run publishes and stores at once.
func (q *Queue) ForgetProgress(id core.ID) {
	q.throttle.Flush(id)
	q.mu.Lock()
	delete(q.stored, id)
	delete(q.diskChecked, id)
	q.mu.Unlock()
}
