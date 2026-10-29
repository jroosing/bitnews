package queue_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// progressEvents drains the bus and returns the DownloadProgress events.
func (h *harness) progressEvents() []core.DownloadProgress {
	var out []core.DownloadProgress
	for {
		select {
		case ev := <-h.events.Events():
			if p, ok := ev.(core.DownloadProgress); ok {
				out = append(out, p)
			}
		default:
			return out
		}
	}
}

func (h *harness) doneBytes(id core.ID) int64 {
	h.t.Helper()
	d, err := h.downloads.Get(h.ctx, id)
	require.NoError(h.t, err)
	return d.DoneBytes
}

func TestProgressIsPublishedThrottledAndTheLastOneAlways(t *testing.T) {
	h := newHarness(t)
	id := h.add("a", withState(core.StateFetching))

	assert.True(t, h.q.Progress(h.ctx, id, 100, 1000, 50))
	assert.False(t, h.q.Progress(h.ctx, id, 200, 1000, 50), "too soon")
	assert.True(t, h.q.Progress(h.ctx, id, 1000, 1000, 50), "the final value always goes out")
	h.now = h.now.Add(time.Second)
	assert.True(t, h.q.Progress(h.ctx, id, 500, 1000, 50))

	got := h.progressEvents()
	require.Len(t, got, 3)
	assert.Equal(t, core.DownloadProgress{ID: id, DoneBytes: 100, TotalBytes: 1000, BytesPerSecond: 50}, got[0])
}

func TestProgressStoresDoneBytesAtMostOncePerInterval(t *testing.T) {
	h := newHarness(t)
	id := h.add("a", withState(core.StateFetching))

	h.q.Progress(h.ctx, id, 100, 1000, 0)
	h.q.Progress(h.ctx, id, 300, 1000, 0)
	assert.EqualValues(t, 100, h.doneBytes(id), "the second write waits for the interval")

	h.now = h.now.Add(3 * time.Second)
	h.q.Progress(h.ctx, id, 400, 1000, 0)
	assert.EqualValues(t, 400, h.doneBytes(id))
}

func TestForgetProgressStartsTheDownloadAfresh(t *testing.T) {
	h := newHarness(t)
	id := h.add("a", withState(core.StateFetching))
	h.q.Progress(h.ctx, id, 100, 1000, 0)

	h.q.ForgetProgress(id)

	assert.True(t, h.q.Progress(h.ctx, id, 150, 1000, 0), "a new run publishes at once")
	assert.EqualValues(t, 150, h.doneBytes(id), "and stores at once")
}
