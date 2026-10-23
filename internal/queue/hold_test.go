package queue_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestHoldAbandonsTheFetchAndKeepsTheDownloadFromStartingUntilRelease(t *testing.T) {
	h := newHarness(t)
	id := h.add("a", withState(core.StateFetching))
	h.proto.running[id] = true

	require.NoError(t, h.q.Hold(h.ctx, id))
	h.q.Reconcile(h.ctx)

	assert.Empty(t, h.running())
	assert.Contains(t, h.proto.calls, "abandon "+string(id))
	assert.True(t, h.q.Held(id))

	h.q.Release(id)
	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"a"}, h.running())
	assert.False(t, h.q.Held(id))
}

func TestAHoldExpiresAfterTwoMinutes(t *testing.T) {
	h := newHarness(t)
	id := h.add("a")
	require.NoError(t, h.q.Hold(h.ctx, id))

	h.now = h.now.Add(2*time.Minute + time.Second)
	h.q.Reconcile(h.ctx)

	assert.Equal(t, []string{"a"}, h.running())
}

func TestHoldOfADownloadWithoutAFetchDoesNotAbandonAnything(t *testing.T) {
	h := newHarness(t)
	id := h.add("a")

	require.NoError(t, h.q.Hold(h.ctx, id))

	assert.Empty(t, h.proto.calls)
}

func TestRecoverMovesTheQueuesFetchingDownloadsBackToQueued(t *testing.T) {
	h := newHarness(t)
	mine := h.add("mine", withState(core.StateFetching))
	torrent := h.add("torrent", withState(core.StateFetching), withSource(core.SourceTorrent))

	require.NoError(t, h.q.Recover(h.ctx))

	assert.Equal(t, core.StateQueued, h.state(mine))
	assert.Equal(t, core.StateFetching, h.state(torrent))
}
