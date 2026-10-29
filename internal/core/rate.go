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

// mergeStep bounds the memory: samples are kept at least this far apart,
// except the newest, which moves with every Add until it is mergeStep past the
// one before it.
const mergeStep = 100 * time.Millisecond

// Add records the cumulative byte count at time at. Merging measures the
// distance to the sample before the newest, never to the newest itself:
// otherwise samples that come faster than mergeStep would keep replacing a
// single sample, and a window of one sample has no rate.
func (w *RateWindow) Add(at time.Time, total int64) {
	if n := len(w.samples); n >= 2 && at.Sub(w.samples[n-2].at) < mergeStep {
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
