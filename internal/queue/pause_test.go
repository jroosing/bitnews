package queue_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/queue"
)

func TestAPausedQueueStopsItsFetchesAndStartsNothing(t *testing.T) {
	h := newHarness(t)
	running := h.add("running", withState(core.StateFetching))
	h.proto.running[running] = true
	h.add("waiting")

	h.q.Pause(queue.ReasonUser, "")
	h.q.Reconcile(h.ctx)

	assert.Empty(t, h.running())
	assert.Equal(t, core.StateQueued, h.state(running))
	paused, reason, _ := h.q.Paused()
	assert.True(t, paused)
	assert.Equal(t, queue.ReasonUser, reason)
	assert.Equal(t, []string{"paused nzb user"}, h.queueEvents())
}

func TestEachPauseReasonIsHeldSeparately(t *testing.T) {
	h := newHarness(t)
	h.add("a")

	h.q.Pause(queue.ReasonUser, "")
	h.q.Pause(queue.ReasonBindUnavailable, "wg0 is down")
	h.q.Resume(queue.ReasonBindUnavailable)
	h.q.Reconcile(h.ctx)

	assert.Empty(t, h.running(), "the user pause still holds")
	paused, reason, _ := h.q.Paused()
	assert.True(t, paused)
	assert.Equal(t, queue.ReasonUser, reason)

	h.q.Resume(queue.ReasonUser)
	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"a"}, h.running())
	assert.Equal(t, []string{"paused nzb user", "paused nzb bind_unavailable", "resumed nzb"}, h.queueEvents(),
		"resumed only once the last reason is released")
}

func TestPausedReportsTheMostSpecificReason(t *testing.T) {
	h := newHarness(t)

	h.q.Pause(queue.ReasonUser, "")
	h.q.Pause(queue.ReasonBindUnavailable, "wg0 is down")

	paused, reason, detail := h.q.Paused()
	assert.True(t, paused)
	assert.Equal(t, queue.ReasonBindUnavailable, reason)
	assert.Equal(t, "wg0 is down", detail)
}

func TestPauseAndResumeAreIdempotentAndWakeTheRunner(t *testing.T) {
	h := newHarness(t)

	h.q.Pause(queue.ReasonUser, "")
	h.q.Pause(queue.ReasonUser, "")
	h.q.Resume(queue.ReasonUser)
	h.q.Resume(queue.ReasonUser)

	assert.Equal(t, []string{"paused nzb user", "resumed nzb"}, h.queueEvents())
	assert.Equal(t, 2, h.woken)
	paused, _, _ := h.q.Paused()
	assert.False(t, paused)
}
