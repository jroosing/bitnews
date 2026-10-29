package core_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/core"
)

func TestRateWindowAveragesOverTheLastFiveSeconds(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := core.NewRateWindow(5 * time.Second)
	assert.Zero(t, w.Rate(t0), "no samples")
	for i := 0; i <= 7; i++ {
		w.Add(t0.Add(time.Duration(i)*time.Second), int64(i)*1000)
	}

	assert.Equal(t, int64(1000), w.Rate(t0.Add(7*time.Second)))
	assert.Zero(t, w.Rate(t0.Add(30*time.Second)), "stalled")
}

func TestRateWindowFollowsASpeedChange(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := core.NewRateWindow(5 * time.Second)
	total := int64(0)
	for i := range 10 { // 1000 B/s for 10 s
		total += 1000
		w.Add(t0.Add(time.Duration(i)*time.Second), total)
	}
	for i := 10; i < 20; i++ { // then 4000 B/s
		total += 4000
		w.Add(t0.Add(time.Duration(i)*time.Second), total)
	}

	assert.Equal(t, int64(4000), w.Rate(t0.Add(19*time.Second)))
}

// Data arrives in bursts: a second without bytes in the middle of a transfer
// is not a stop.
func TestRateWindowDoesNotDropToZeroForAQuietSecond(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := core.NewRateWindow(core.DefaultRateWindow)
	for i, total := range []int64{0, 2000, 4000, 4000, 8000} { // nothing in second 3
		w.Add(t0.Add(time.Duration(i)*time.Second), total)
	}

	assert.Equal(t, int64(2000), w.Rate(t0.Add(4*time.Second)))
	w.Add(t0.Add(5*time.Second), 8000)
	assert.Equal(t, int64(1600), w.Rate(t0.Add(5*time.Second)), "a quiet second lowers the average, it does not zero it")
}

// A fast Usenet download reports an Article every few milliseconds; the
// rate must hold steady, never read 0 while bytes pour in.
func TestRateWindowKeepsMeasuringWhenSamplesComeEveryFewMilliseconds(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := core.NewRateWindow(core.DefaultRateWindow)
	var total int64
	for i := range 1000 { // a sample every 10 ms for 10 s at 1 MB/s
		total += 10_000
		w.Add(t0.Add(time.Duration(i)*10*time.Millisecond), total)
		if i > 0 && i%100 == 0 {
			assert.InDelta(t, 1_000_000, w.Rate(t0.Add(time.Duration(i)*10*time.Millisecond)), 20_000, "after %d ms", i*10)
		}
	}
}

// A counter read once a second (the torrent engine) spreads each delta over
// the second it covers, so a reading on a second boundary does not count the
// previous second's bytes as this one's.
func TestRateWindowSpreadsASparseReadingOverTheTimeItCovers(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 500_000_000, time.UTC) // half past a second
	w := core.NewRateWindow(4 * time.Second)
	for i := 0; i <= 20; i++ {
		w.Add(t0.Add(time.Duration(i)*time.Second), int64(i)*3000)
	}

	assert.InDelta(t, 3000, w.Rate(t0.Add(20*time.Second)), 1)
}

// The first bytes of a transfer can come in one burst; dividing by the few
// milliseconds since the start would show a huge spike.
func TestRateWindowDoesNotSpikeOnTheFirstBurst(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := core.NewRateWindow(core.DefaultRateWindow)
	w.Add(t0, 0)
	w.Add(t0.Add(10*time.Millisecond), 1_000_000)

	assert.LessOrEqual(t, w.Rate(t0.Add(10*time.Millisecond)), int64(1_000_000))
}

// A gap of an hour between readings costs no more work than the window.
func TestRateWindowCopesWithALongGap(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := core.NewRateWindow(core.DefaultRateWindow)
	w.Add(t0, 0)
	w.Add(t0.Add(time.Hour), 3600*1000)
	w.Add(t0.Add(time.Hour+time.Second), 3600*1000+1000)

	assert.InDelta(t, 1000, w.Rate(t0.Add(time.Hour+time.Second)), 1)
}
