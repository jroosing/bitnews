package queue_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/queue"
)

const root = "/data/usenet/incomplete"

func TestADownloadThatDoesNotFitPausesTheQueueForDiskFull(t *testing.T) {
	h := newHarness(t)
	h.minFree = 100
	other := h.add("other", withState(core.StateFetching))
	h.proto.running[other] = true
	h.add("big", withPriority(core.DownloadPriorityHigh), withSize(5000, 0))
	h.free[root] = 1000

	h.q.Reconcile(h.ctx)

	assert.Empty(t, h.running(), "every fetch stops while the disk is short")
	assert.Equal(t, core.StateQueued, h.state(other))
	paused, reason, detail := h.q.Paused()
	assert.True(t, paused)
	assert.Equal(t, queue.ReasonDiskFull, reason)
	assert.Equal(t, "1000 bytes free, 5100 needed", detail)
	assert.Equal(t, []string{"paused nzb disk_full"}, h.queueEvents())
}

func TestTheDiskGuardReleasesItselfOnceTheNextDownloadFits(t *testing.T) {
	h := newHarness(t)
	h.add("big", withSize(5000, 0))
	h.free[root] = 1000
	h.q.Reconcile(h.ctx)
	h.queueEvents()

	h.free[root] = 10000
	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"big"}, h.running())
	assert.Equal(t, []string{"resumed nzb"}, h.queueEvents())
}

func TestTheDiskGuardDoesNotAnnounceAResumeWhileTheUserStillPauses(t *testing.T) {
	h := newHarness(t)
	h.add("big", withSize(5000, 0))
	h.free[root] = 1000
	h.q.Reconcile(h.ctx)
	h.q.Pause(queue.ReasonUser, "")
	h.queueEvents()

	h.free[root] = 10000
	h.q.Reconcile(h.ctx)

	assert.Empty(t, h.running())
	assert.Empty(t, h.queueEvents())
	_, reason, _ := h.q.Paused()
	assert.Equal(t, queue.ReasonUser, reason)
}

func TestTheDiskGuardPassesWhenFreeSpaceCannotBeRead(t *testing.T) {
	for name, err := range map[string]error{"unsupported": fsutil.ErrUnsupported, "error": errBoom} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.add("a", withSize(5000, 0))
			h.freeErr = err

			h.q.Reconcile(h.ctx)

			assert.Equal(t, []string{"a"}, h.running())
		})
	}
}

func TestFitsPausesTheQueueWhenARunningFetchNoLongerFits(t *testing.T) {
	h := newHarness(t)
	h.free[root] = 1000

	assert.True(t, h.q.Fits(h.ctx, 500))
	assert.False(t, h.q.Fits(h.ctx, 5000))

	_, reason, _ := h.q.Paused()
	assert.Equal(t, queue.ReasonDiskFull, reason)
}

func TestDiskFullPausesTheQueue(t *testing.T) {
	h := newHarness(t)

	h.q.DiskFull("no space left on device")

	paused, reason, detail := h.q.Paused()
	assert.True(t, paused)
	assert.Equal(t, queue.ReasonDiskFull, reason)
	assert.Equal(t, "no space left on device", detail)
}

func TestADiskFullPauseHoldsWhileTheDownloadStillWouldNotFit(t *testing.T) {
	h := newHarness(t)
	id := h.add("big", withState(core.StateFetching), withSize(1000, 0))
	h.proto.running[id] = true
	h.free[root] = 10

	h.q.DiskFull("no space left on device") // the fetch failed with ENOSPC
	h.q.Reconcile(h.ctx)

	paused, reason, _ := h.q.Paused()
	assert.True(t, paused, "it does not release itself while the next Download needs more than is free")
	assert.Equal(t, queue.ReasonDiskFull, reason)
	assert.Empty(t, h.running())
}
