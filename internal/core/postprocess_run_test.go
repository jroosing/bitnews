package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// ppGate is a repairer body that blocks until released and records how many
// callers were inside at once.
type ppGate struct {
	mu      sync.Mutex
	cur     int
	max     int
	entered chan core.ID
	release chan struct{}
}

func newPPGate() *ppGate {
	return &ppGate{entered: make(chan core.ID, 100), release: make(chan struct{})}
}

func (g *ppGate) repair(ctx context.Context, dir string) (core.RepairOutcome, error) {
	g.mu.Lock()
	g.cur++
	g.max = max(g.max, g.cur)
	g.mu.Unlock()
	g.entered <- core.ID(dir)
	defer func() {
		g.mu.Lock()
		g.cur--
		g.mu.Unlock()
	}()
	select {
	case <-g.release:
	case <-ctx.Done():
		return core.RepairOutcome{}, ctx.Err()
	}
	return core.RepairOutcome{HasPAR2: true}, nil
}

func (g *ppGate) maxSeen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.max
}

func waitEntered(t *testing.T, g *ppGate, n int) {
	t.Helper()
	for range n {
		select {
		case <-g.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("stage not entered in time")
		}
	}
}

func assertNoneEntered(t *testing.T, g *ppGate) {
	t.Helper()
	select {
	case <-g.entered:
		t.Fatal("more Downloads in a stage than the concurrency limit allows")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestProcessEnforcesTheConcurrencyLimit(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.Concurrency = 2 })
	g := newPPGate()
	h.repairer.fn = g.repair
	var ids []core.ID
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		ids = append(ids, h.add(n, core.StateFetched, core.SourceNZB).ID)
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() { assert.NoError(t, h.pp.Process(context.Background(), id)) })
	}
	waitEntered(t, g, 2)
	assertNoneEntered(t, g)
	waiting := 0
	for _, id := range ids {
		if h.get(id).State == core.StateFetched {
			waiting++
		}
	}
	assert.Equal(t, 3, waiting, "Downloads waiting for a slot stay Fetched")

	close(g.release)
	wg.Wait()

	assert.Equal(t, 2, g.maxSeen())
	for _, id := range ids {
		assert.Equal(t, core.StateReady, h.get(id).State)
	}
}

func TestProcessWaitingForASlotStopsWhenCancelled(t *testing.T) {
	h := newPPHarness(t) // concurrency 1
	g := newPPGate()
	h.repairer.fn = g.repair
	first := h.add("a", core.StateFetched, core.SourceNZB)
	second := h.add("b", core.StateFetched, core.SourceNZB)
	go func() { _ = h.pp.Process(context.Background(), first.ID) }()
	waitEntered(t, g, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- h.pp.Process(ctx, second.ID) }()

	cancel()

	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, core.StateFetched, h.get(second.ID).State)
	close(g.release)
}

func TestProcessRefusesADownloadAlreadyBeingProcessed(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.Concurrency = 2 })
	g := newPPGate()
	h.repairer.fn = g.repair
	d := h.add("a", core.StateFetched, core.SourceNZB)
	go func() { _ = h.pp.Process(context.Background(), d.ID) }()
	waitEntered(t, g, 1)

	err := h.pp.Process(context.Background(), d.ID)

	require.ErrorIs(t, err, core.ErrAlreadyPostProcessing)
	close(g.release)
}

func TestRecoverReprocessesStuckDownloadsFromTheFirstStage(t *testing.T) {
	h := newPPHarness(t)
	h.unpacker.found = true
	stuck := h.add("stuck", core.StatePostProcessing, core.SourceNZB)
	fetched := h.add("fetched", core.StateFetched, core.SourceNZB)
	ready := h.add("ready", core.StateReady, core.SourceNZB)

	require.NoError(t, h.pp.Recover(context.Background()))

	assert.Equal(t, core.StateReady, h.get(stuck.ID).State)
	assert.Equal(t, []string{"repair", "detect", "unpack", "finish"}, h.log.all(), "from verifying, once")
	assert.Equal(t, core.StateFetched, h.get(fetched.ID).State, "Recover only touches PostProcessing")
	assert.Equal(t, core.StateReady, h.get(ready.ID).State)
	// No second Fetched -> PostProcessing for the stuck Download.
	changes := h.repo.changes()
	require.Len(t, changes, 1)
	assert.Equal(t, core.StateReady, changes[0].To)
}

func TestRestartInTheMiddleOfAStageReentersAtVerifying(t *testing.T) {
	h := newPPHarness(t)
	h.unpacker.found = true
	ctx, cancel := context.WithCancel(context.Background())
	var interrupt sync.Once
	h.unpacker.unpackFn = func(ctx context.Context, _, _ string, _ core.UnpackOptions) (core.UnpackResult, error) {
		var first bool
		interrupt.Do(func() { first = true })
		if first { // the daemon stops during the first unpack
			cancel()
			<-ctx.Done()
			return core.UnpackResult{}, ctx.Err()
		}
		return core.UnpackResult{}, nil
	}
	d := h.add("x", core.StateFetched, core.SourceNZB)
	require.ErrorIs(t, h.pp.Process(ctx, d.ID), context.Canceled)
	require.Equal(t, core.StatePostProcessing, h.get(d.ID).State)

	require.NoError(t, h.pp.Recover(context.Background()))

	assert.Equal(t, core.StateReady, h.get(d.ID).State)
	assert.Equal(t,
		[]string{"repair", "detect", "unpack", "repair", "detect", "unpack", "finish"},
		h.log.all())
}

func TestRecoverHonoursTheConcurrencyLimit(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.Concurrency = 2 })
	g := newPPGate()
	h.repairer.fn = g.repair
	for _, n := range []string{"a", "b", "c", "d"} {
		h.add(n, core.StatePostProcessing, core.SourceNZB)
	}
	done := make(chan error)
	go func() { done <- h.pp.Recover(context.Background()) }()

	waitEntered(t, g, 2)
	assertNoneEntered(t, g)
	close(g.release)

	require.NoError(t, <-done)
	assert.Equal(t, 2, g.maxSeen())
}

func TestRecoverStopsOnCancelledContext(t *testing.T) {
	h := newPPHarness(t)
	h.add("a", core.StatePostProcessing, core.SourceNZB)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := h.pp.Recover(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, h.log.all())
}

func startRun(t *testing.T, h *ppHarness) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.pp.Run(ctx) }()
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Error("Run did not return after cancel")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func eventuallyState(t *testing.T, h *ppHarness, id core.ID, want core.State) {
	t.Helper()
	require.Eventually(t, func() bool { return h.get(id).State == want }, 5*time.Second, 5*time.Millisecond)
}

func TestRunProcessesFetchedDownloadsAndThoseThatBecomeFetchedLater(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.PollInterval = time.Hour })
	early := h.add("early", core.StateFetched, core.SourceNZB)
	stuck := h.add("stuck", core.StatePostProcessing, core.SourceNZB)
	stop := startRun(t, h)

	eventuallyState(t, h, early.ID, core.StateReady)
	eventuallyState(t, h, stuck.ID, core.StateReady)
	late := h.add("late", core.StateFetched, core.SourceNZB)
	h.bus.Publish(core.Fetched{ID: late.ID})
	eventuallyState(t, h, late.ID, core.StateReady)

	require.ErrorIs(t, stop(), context.Canceled)
}

func TestRunNeverExceedsTheConcurrencyLimitAndDrainsTheBacklog(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.Concurrency = 2; c.PollInterval = time.Hour })
	g := newPPGate()
	h.repairer.fn = g.repair
	var ids []core.ID
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		ids = append(ids, h.add(n, core.StateFetched, core.SourceNZB).ID)
	}
	startRun(t, h)

	waitEntered(t, g, 2)
	assertNoneEntered(t, g)
	close(g.release)

	for _, id := range ids {
		eventuallyState(t, h, id, core.StateReady)
	}
	assert.Equal(t, 2, g.maxSeen())
}

func TestRunPollsForFetchedDownloadsItWasNotToldAbout(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.PollInterval = 10 * time.Millisecond })
	startRun(t, h)
	d := h.add("silent", core.StateFetched, core.SourceNZB) // no Fetched event

	eventuallyState(t, h, d.ID, core.StateReady)
}

func TestRunStopsRunningDownloadsOnCancelAndLeavesThemPostProcessing(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.PollInterval = time.Hour })
	g := newPPGate()
	h.repairer.fn = g.repair
	d := h.add("a", core.StateFetched, core.SourceNZB)
	stop := startRun(t, h)
	waitEntered(t, g, 1)

	require.ErrorIs(t, stop(), context.Canceled)

	assert.Equal(t, core.StatePostProcessing, h.get(d.ID).State)
	for _, ev := range h.events() {
		assert.NotContains(t, []core.EventKind{core.KindReady, core.KindFailed}, ev.Kind())
	}
}

func TestRecoverAfterACrashBetweenFinishAndReadyRecordsReady(t *testing.T) {
	// The first run finished (the working directory was moved away) and
	// died before the Ready update. Finish is idempotent: it finds the
	// moved directory again.
	fs := &ppFS{exists: false}
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.FS = fs })
	d := h.add("x", core.StatePostProcessing, core.SourceNZB)

	require.NoError(t, h.pp.Recover(context.Background()))

	got := h.get(d.ID)
	assert.Equal(t, core.StateReady, got.State)
	assert.Equal(t, "/complete/x", got.OutputPath)
	assert.Equal(t, []string{"finish"}, h.log.all())
}
