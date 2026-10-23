package daemon

import (
	"context"
	"sync"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// tracker follows the bus for the live source: the Post-processing stage
// of each Download (core.PostProcessStage, cleared when the Download is
// Ready, Failed or removed, or leaves Post-processing) and the current rate
// of each Download being fetched (core.DownloadProgress, cleared when it is
// no longer Fetching). It is a cache of events that are not persisted, so it
// starts empty and a Download in the middle of a stage after a restart shows
// no stage until the stage events of its re-run arrive.
type tracker struct {
	mu     sync.Mutex
	stages map[core.ID]core.Stage
	rates  map[core.ID]int64
}

func newTracker() *tracker {
	return &tracker{stages: map[core.ID]core.Stage{}, rates: map[core.ID]int64{}}
}

// stage is the Post-processing stage a Download is in, if known.
func (t *tracker) stage(id core.ID) (core.Stage, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.stages[id]
	return s, ok
}

// rate is the current rate of one Download being fetched, 0 when unknown.
func (t *tracker) rate(id core.ID) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rates[id]
}

// speed is the sum of the rates of the Downloads being fetched.
func (t *tracker) speed() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var sum int64
	for _, r := range t.rates {
		sum += r
	}
	return sum
}

func (t *tracker) apply(ev core.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch e := ev.(type) {
	case core.PostProcessStage:
		t.stages[e.ID] = e.Stage
	case core.DownloadProgress:
		t.rates[e.ID] = e.BytesPerSecond
	case core.DownloadUpdated:
		d := e.Download
		if d.State != core.StatePostProcessing {
			delete(t.stages, d.ID)
		}
		if d.State != core.StateFetching {
			delete(t.rates, d.ID)
		}
	case core.Ready:
		delete(t.stages, e.ID)
	case core.Failed:
		delete(t.stages, e.ID)
		delete(t.rates, e.ID)
	case core.Fetched:
		delete(t.rates, e.ID)
	case core.Removed:
		delete(t.stages, e.ID)
		delete(t.rates, e.ID)
	}
}

func (t *tracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.stages)
	clear(t.rates)
}

// run follows bus until ctx ends.
func (t *tracker) run(ctx context.Context, bus *core.Bus) {
	for ctx.Err() == nil {
		sub := bus.Subscribe(ctx)
		for ev := range sub.Events() {
			t.apply(ev)
		}
		sub.Close()
		// Dropped for being slow: events were missed, start over.
		t.reset()
	}
}
