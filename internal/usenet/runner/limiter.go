package runner

import (
	"context"
	"sync"
	"time"
)

// minSleep is the shortest wait worth a timer: shorter delays are carried as
// debt, so a very high limit does not cost a timer per line.
const minSleep = 2 * time.Millisecond

// limiter is a token bucket without burst, shared by all connections. Each
// call reserves the time its bytes take at the current rate and waits for
// its turn, so the long-run rate equals the limit however many connections
// call it.
type limiter struct {
	clock Clock

	mu   sync.Mutex
	rate int64     // bytes per second; <= 0 means unlimited
	next time.Time // when the bytes reserved so far have been "sent"
}

func newLimiter(clock Clock) *limiter { return &limiter{clock: clock} }

func (l *limiter) setRate(bytesPerSecond int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate = bytesPerSecond
	l.next = time.Time{}
}

// wait reserves n bytes and sleeps until it is their turn, or ctx ends.
func (l *limiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	if l.rate <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := l.clock.Now()
	if l.next.Before(now) {
		l.next = now
	}
	delay := l.next.Sub(now)
	l.next = l.next.Add(time.Duration(float64(n) / float64(l.rate) * float64(time.Second)))
	l.mu.Unlock()

	if delay < minSleep {
		return ctx.Err()
	}
	t := l.clock.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetSpeedLimit limits the bytes received from the Servers, all Downloads
// together, to bytesPerSecond. Zero or less removes the limit. It applies at
// once, also to running fetches.
func (r *Runner) SetSpeedLimit(bytesPerSecond int64) {
	r.lim.setRate(bytesPerSecond)
}
