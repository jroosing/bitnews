package core

import (
	"sync"
	"time"
)

// ProgressThrottle limits how often progress is reported per Download. Engines
// call Allow on every progress update and publish a DownloadProgress event to
// the Bus only when it returns true:
//
//	if throttle.Allow(d.ID, done, total) {
//		bus.Publish(DownloadProgress{ID: d.ID, DoneBytes: done, TotalBytes: total, BytesPerSecond: rate})
//	}
//
// An engine calls Flush when a Download stops fetching or is removed, so that
// its next report passes at once and the throttle forgets the Download. It is
// safe for concurrent use.
type ProgressThrottle struct {
	now      func() time.Time
	interval time.Duration

	mu   sync.Mutex
	last map[ID]time.Time // when each Download last passed
}

// DefaultProgressInterval is the throttle interval the design asks for: at
// most one progress event per second per Download.
const DefaultProgressInterval = time.Second

// NewProgressThrottle returns a throttle that lets one report per interval
// through for each Download, reading time from now.
func NewProgressThrottle(now func() time.Time, interval time.Duration) *ProgressThrottle {
	return &ProgressThrottle{now: now, interval: interval, last: make(map[ID]time.Time)}
}

// Allow reports whether the progress of Download id should be published now.
// It returns true for the first report, once the interval has passed since the
// last report that passed, and always for the final value: done equal to a
// known (non-zero) total.
func (t *ProgressThrottle) Allow(id ID, done, total int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if total > 0 && done == total {
		delete(t.last, id)
		return true
	}
	now := t.now()
	if last, ok := t.last[id]; ok && now.Sub(last) < t.interval {
		return false
	}
	t.last[id] = now
	return true
}

// Flush forgets Download id, so that its next report passes at once. It also
// frees the throttle's memory for Downloads that never reach their total.
func (t *ProgressThrottle) Flush(id ID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, id)
}
