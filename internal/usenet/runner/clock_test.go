package runner_test

import (
	"sync"
	"time"

	"github.com/jroosing/bitnews/internal/usenet/nntp"
)

// fakeClock is a controllable runner.Clock. In auto mode a timer fires at
// once and moves the clock to its deadline, so code that sleeps (the speed
// limiter) runs in virtual time. step makes every Now call advance the
// clock.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	auto bool
	step time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

func (c *fakeClock) elapsed(since time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(since)
}

func (c *fakeClock) NewTimer(d time.Duration) nntp.Timer {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.auto || d <= 0 {
		if dl := c.now.Add(d); dl.After(c.now) {
			c.now = dl
		}
		ch <- c.now
	}
	return &fakeTimer{ch: ch}
}

type fakeTimer struct{ ch chan time.Time }

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool { return false }
