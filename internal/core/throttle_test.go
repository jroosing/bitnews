package core_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// fakeClock is a manually advanced clock for throttle tests.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestThrottle() (*core.ProgressThrottle, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return core.NewProgressThrottle(clock.Now, time.Second), clock
}

func TestThrottleAllowsAtMostOnePerIntervalPerDownload(t *testing.T) {
	th, clock := newTestThrottle()

	assert.True(t, th.Allow("a", 10, 100), "first report passes")
	clock.Advance(999 * time.Millisecond)
	assert.False(t, th.Allow("a", 20, 100), "within the interval")
	clock.Advance(time.Millisecond)
	assert.True(t, th.Allow("a", 30, 100), "interval elapsed")
	assert.False(t, th.Allow("a", 40, 100), "new interval started at the last allowed report")
}

func TestThrottleTracksDownloadsIndependently(t *testing.T) {
	th, _ := newTestThrottle()

	assert.True(t, th.Allow("a", 10, 100))
	assert.True(t, th.Allow("b", 10, 100))
	assert.False(t, th.Allow("a", 20, 100))
	assert.False(t, th.Allow("b", 20, 100))
}

func TestThrottleAlwaysAllowsTheFinalValue(t *testing.T) {
	th, _ := newTestThrottle()

	assert.True(t, th.Allow("a", 10, 100))
	assert.True(t, th.Allow("a", 100, 100), "done == total passes inside the interval")
}

func TestThrottleDoesNotTreatUnknownTotalAsFinal(t *testing.T) {
	th, _ := newTestThrottle()

	assert.True(t, th.Allow("a", 0, 0))
	assert.False(t, th.Allow("a", 0, 0), "total 0 means unknown, not finished")
}

func TestThrottleFlushMakesNextReportPass(t *testing.T) {
	th, _ := newTestThrottle()

	assert.True(t, th.Allow("a", 10, 100))
	assert.True(t, th.Allow("b", 10, 100))
	th.Flush("a")

	assert.True(t, th.Allow("a", 20, 100), "flushed Download reports at once")
	assert.False(t, th.Allow("a", 30, 100), "and is throttled again afterwards")
	assert.False(t, th.Allow("b", 20, 100), "other Downloads are unaffected")
}
