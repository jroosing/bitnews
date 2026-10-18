package runner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRateWindowAveragesOverTheLastFiveSeconds(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := newRateWindow(5 * time.Second)
	assert.Zero(t, w.rate(t0), "no samples")
	for i := 0; i <= 7; i++ {
		w.add(t0.Add(time.Duration(i)*time.Second), int64(i)*1000)
	}

	assert.Equal(t, int64(1000), w.rate(t0.Add(7*time.Second)))
	assert.Zero(t, w.rate(t0.Add(30*time.Second)), "stalled")
}

func TestRateWindowFollowsASpeedChange(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := newRateWindow(5 * time.Second)
	total := int64(0)
	for i := range 10 { // 1000 B/s for 10 s
		total += 1000
		w.add(t0.Add(time.Duration(i)*time.Second), total)
	}
	for i := 10; i < 20; i++ { // then 4000 B/s
		total += 4000
		w.add(t0.Add(time.Duration(i)*time.Second), total)
	}

	assert.Equal(t, int64(4000), w.rate(t0.Add(19*time.Second)))
}
