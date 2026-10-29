package core

import "time"

// DefaultRateWindow is the span transfer rates are averaged over: long
// enough that a quiet second (a choked peer, a slow Article) does not read as
// a stop, short enough to follow a change of speed.
const DefaultRateWindow = 5 * time.Second

// RateWindow measures a transfer rate as the average over a sliding window.
// It is not safe for concurrent use; its owner locks.
type RateWindow struct {
	span    time.Duration
	samples []rateSample
}

type rateSample struct {
	at    time.Time
	total int64
}

// NewRateWindow returns a RateWindow over span.
func NewRateWindow(span time.Duration) *RateWindow { return &RateWindow{span: span} }

// Add records the cumulative byte count at time at. Samples closer than
// 100 ms to the previous one replace it, which bounds the memory.
func (w *RateWindow) Add(at time.Time, total int64) {
	if n := len(w.samples); n > 0 && at.Sub(w.samples[n-1].at) < 100*time.Millisecond {
		w.samples[n-1] = rateSample{at, total}
	} else {
		w.samples = append(w.samples, rateSample{at, total})
	}
	w.trim(at)
}

func (w *RateWindow) trim(now time.Time) {
	i := 0
	for i < len(w.samples) && now.Sub(w.samples[i].at) > w.span {
		i++
	}
	w.samples = w.samples[i:]
}

// Rate returns bytes per second over the samples within the window ending at
// now; zero when fewer than two samples are inside it.
func (w *RateWindow) Rate(now time.Time) int64 {
	w.trim(now)
	if len(w.samples) < 2 {
		return 0
	}
	first, last := w.samples[0], w.samples[len(w.samples)-1]
	el := last.at.Sub(first.at)
	if el <= 0 {
		return 0
	}
	return int64(float64(last.total-first.total) / el.Seconds())
}
