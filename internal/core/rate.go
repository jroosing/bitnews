package core

import "time"

// DefaultRateWindow is the span transfer rates are averaged over: long
// enough that a quiet second (a choked peer, a slow Article) does not read as
// a stop, short enough to follow a change of speed within a few seconds.
const DefaultRateWindow = 10 * time.Second

// RateWindow measures a transfer rate as the average over a sliding window,
// the way NZBGet's speed meter does: a ring of one-second slots, each holding
// the bytes received in its second. Add takes cumulative byte counts at any
// pace (every Article of a fast Usenet download, or once a second from a
// torrent's counters) and spreads each increase over the time since the
// previous count, so a reading on a second boundary is not counted as the
// next second's. The memory is one number per second of the window. It is
// not safe for concurrent use; its owner locks.
type RateWindow struct {
	slots []float64 // bytes received in a second, at index sec % len(slots)

	started   bool
	first     time.Time // the first Add
	lastAt    time.Time // the latest Add
	lastTotal int64
	newest    int64 // unix second of the newest slot in use
}

// NewRateWindow returns a RateWindow over span, rounded up to whole seconds.
func NewRateWindow(span time.Duration) *RateWindow {
	n := max(int((span+time.Second-1)/time.Second), 1)
	return &RateWindow{slots: make([]float64, n)}
}

// Add records the cumulative byte count at time at. A count lower than the
// last one (a counter that started over) adds nothing.
func (w *RateWindow) Add(at time.Time, total int64) {
	if !w.started {
		w.started, w.first, w.lastAt, w.lastTotal, w.newest = true, at, at, total, at.Unix()
		return
	}
	if at.Before(w.lastAt) {
		at = w.lastAt
	}
	delta := total - w.lastTotal
	from := w.lastAt
	w.lastAt, w.lastTotal = at, total
	w.advance(at.Unix())
	if delta > 0 {
		w.spread(from, at, float64(delta))
	}
}

// spread shares bytes received between from and to among the slots of the
// seconds that interval covers, in proportion to the overlap. Only the
// seconds still in the window get their share; the rest has aged out.
func (w *RateWindow) spread(from, to time.Time, bytes float64) {
	dur := to.Sub(from)
	if dur <= 0 {
		w.slots[w.index(to.Unix())] += bytes
		return
	}
	n := int64(len(w.slots))
	for sec := max(from.Unix(), to.Unix()-n+1); sec <= to.Unix(); sec++ {
		start, end := time.Unix(sec, 0), time.Unix(sec+1, 0)
		if from.After(start) {
			start = from
		}
		if to.Before(end) {
			end = to
		}
		if end.After(start) {
			w.slots[w.index(sec)] += bytes * float64(end.Sub(start)) / float64(dur)
		}
	}
}

// advance moves the ring to second sec, clearing the slots of the seconds
// that pass out of the window.
func (w *RateWindow) advance(sec int64) {
	n := int64(len(w.slots))
	for s := w.newest + 1; s <= sec && s <= w.newest+n; s++ {
		w.slots[w.index(s)] = 0
	}
	w.newest = max(w.newest, sec)
}

func (w *RateWindow) index(sec int64) int {
	n := int64(len(w.slots))
	return int(((sec % n) + n) % n)
}

// Rate returns bytes per second over the window ending at now: the bytes in
// its slots divided by the time they cover, at least a second, so the first
// burst of a transfer does not read as a spike. It is zero before any bytes
// and once nothing came for a whole window.
func (w *RateWindow) Rate(now time.Time) int64 {
	if !w.started {
		return 0
	}
	w.advance(now.Unix())
	var sum float64
	for _, b := range w.slots {
		sum += b
	}
	start := time.Unix(w.newest-int64(len(w.slots))+1, 0)
	if w.first.After(start) {
		start = w.first
	}
	return int64(sum / max(now.Sub(start), time.Second).Seconds())
}
