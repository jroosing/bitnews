package queue_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/core"
)

func TestReconcileStartsTheBestRankedDownloadsUpToTheLimit(t *testing.T) {
	h := newHarness(t)
	a := h.add("a")
	b := h.add("b", withPriority(core.DownloadPriorityHigh))
	c := h.add("c")

	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"a", "b"}, h.running())
	assert.Equal(t, core.StateFetching, h.state(a))
	assert.Equal(t, core.StateFetching, h.state(b))
	assert.Equal(t, core.StateQueued, h.state(c))
	assert.Equal(t, []string{"start b fetching", "start a fetching"}, h.proto.calls)
}

func TestReconcileStopsAFetchingDownloadThatIsNoLongerChosen(t *testing.T) {
	h := newHarness(t)
	h.settings.Queues.Usenet = 1
	low := h.add("low", withState(core.StateFetching))
	h.proto.running[low] = true
	h.add("high", withPriority(core.DownloadPriorityHigh))

	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"high"}, h.running())
	assert.Equal(t, core.StateQueued, h.state(low))
}

func TestReconcileRestartsAFetchingDownloadWithoutARunningFetch(t *testing.T) {
	h := newHarness(t)
	orphan := h.add("orphan", withState(core.StateFetching))

	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"orphan"}, h.running())
	assert.Equal(t, core.StateFetching, h.state(orphan))
	assert.Equal(t, []string{"start orphan fetching"}, h.proto.calls)
}

func TestReconcileLeavesADownloadTheProtocolSaysIsNotForNow(t *testing.T) {
	h := newHarness(t)
	cooling := h.add("cooling", withState(core.StateFetching))
	h.proto.notNow[cooling] = true

	h.q.Reconcile(h.ctx)

	assert.Empty(t, h.running())
	assert.Equal(t, core.StateFetching, h.state(cooling))
}

func TestReconcileStartsNothingWhenTheProtocolCannotStartButStillStops(t *testing.T) {
	h := newHarness(t)
	h.settings.Queues.Usenet = 1
	low := h.add("low", withState(core.StateFetching))
	h.proto.running[low] = true
	h.add("high", withPriority(core.DownloadPriorityHigh))
	h.proto.canStart = false

	h.q.Reconcile(h.ctx)

	assert.Empty(t, h.running())
	assert.Equal(t, core.StateQueued, h.state(low))
}

func TestReconcileIgnoresExcludedDownloadsAndOtherKinds(t *testing.T) {
	h := newHarness(t)
	h.settings.Queues.Usenet = 1
	seeding := h.add("seeding", withState(core.StateFetching))
	h.proto.excluded[seeding] = true
	h.add("torrent", withSource(core.SourceTorrent))
	h.add("next")

	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"next"}, h.running())
	assert.Equal(t, core.StateFetching, h.state(seeding))
}

func TestReconcileStopsAQueuedDownloadWhoseFetchRunsButWasNotChosen(t *testing.T) {
	h := newHarness(t)
	h.settings.Queues.Usenet = 1
	h.add("first")
	late := h.add("late")
	h.proto.running[late] = true

	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"first"}, h.running())
	assert.Equal(t, core.StateQueued, h.state(late))
}

func TestReconcileNeverStartsAPausedDownload(t *testing.T) {
	h := newHarness(t)
	h.add("paused", paused())

	h.q.Reconcile(h.ctx)

	assert.Empty(t, h.running())
}
