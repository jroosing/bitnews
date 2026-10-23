package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/core"
	trunner "gitlab.com/kyura4k/gitnews/internal/torrent/runner"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// fakeQueue is a queueControl that remembers what it was asked.
type fakeQueue struct {
	paused bool
	reason string
	detail string
	calls  []string
}

func (f *fakeQueue) Pause(reason, detail string) {
	f.paused, f.reason, f.detail = true, reason, detail
	f.calls = append(f.calls, "pause "+reason)
}
func (f *fakeQueue) Resume(reason string) {
	f.paused, f.reason = false, ""
	f.calls = append(f.calls, "resume "+reason)
}
func (f *fakeQueue) Paused() (bool, string, string) { return f.paused, f.reason, f.detail }

type fakeUsenetLimiter struct{ limit int64 }

func (f *fakeUsenetLimiter) SetSpeedLimit(n int64) { f.limit = n }

type fakeTorrentRunner struct {
	down, up int64
	upRate   int64
	facts    map[core.ID]trunner.Facts
}

func (f *fakeTorrentRunner) SetRateLimits(down, up int64) { f.down, f.up = down, up }
func (f *fakeTorrentRunner) UploadRate() int64            { return f.upRate }
func (f *fakeTorrentRunner) Facts(_ context.Context, id core.ID) (trunner.Facts, bool, error) {
	fa, ok := f.facts[id]
	return fa, ok, nil
}

func queues(u, t *fakeQueue) map[core.SourceKind]queueControl {
	m := map[core.SourceKind]queueControl{core.SourceNZB: u}
	if t != nil {
		m[core.SourceTorrent] = t
	}
	return m
}

func TestWebRuntimeQueueStateAndControlsFollowTheRunners(t *testing.T) {
	u, tq := &fakeQueue{}, &fakeQueue{}
	w := newWebRuntime(queues(u, tq), &fakeUsenetLimiter{}, &fakeTorrentRunner{}, newTracker())

	assert.Equal(t, web.QueueState{Available: true}, w.QueueState(core.SourceNZB))
	w.PauseQueue(core.SourceNZB)
	w.PauseQueue(core.SourceTorrent)
	assert.Equal(t, web.QueueState{Available: true, Paused: true, Reason: "user"}, w.QueueState(core.SourceNZB))
	assert.True(t, tq.paused)
	w.ResumeQueue(core.SourceTorrent)
	assert.False(t, tq.paused)
	assert.Equal(t, []string{"pause user", "resume user"}, tq.calls)
	assert.Equal(t, web.QueueState{}, w.QueueState("other"))
}

func TestWebRuntimeWithoutTorrentsHasNoTorrentQueue(t *testing.T) {
	u := &fakeQueue{}
	w := newWebRuntime(queues(u, nil), &fakeUsenetLimiter{}, nil, newTracker())

	assert.Equal(t, web.QueueState{}, w.QueueState(core.SourceTorrent), "not available")
	w.PauseQueue(core.SourceTorrent)
	w.SetSpeedLimit(core.SourceTorrent, 5, 5)
	assert.Empty(t, u.calls, "nothing reaches the Usenet runner")
	down, up := w.SpeedLimits(core.SourceTorrent)
	assert.Zero(t, down+up)
	rd, ru := w.Rates()
	assert.Zero(t, rd+ru)
	assert.Equal(t, web.DownloadLive{}, w.Live("x"))
}

func TestWebRuntimeSpeedLimitsReachTheRunnersAndAreRemembered(t *testing.T) {
	u, tq := &fakeUsenetLimiter{}, &fakeTorrentRunner{}
	w := newWebRuntime(queues(&fakeQueue{}, &fakeQueue{}), u, tq, newTracker())

	w.SetSpeedLimit(core.SourceNZB, 5<<20, 99)
	w.SetSpeedLimit(core.SourceTorrent, 2<<20, 1<<20)

	assert.Equal(t, int64(5<<20), u.limit)
	assert.Equal(t, [2]int64{2 << 20, 1 << 20}, [2]int64{tq.down, tq.up})
	down, up := w.SpeedLimits(core.SourceNZB)
	assert.Equal(t, int64(5<<20), down)
	assert.Zero(t, up, "Usenet has no upload limit")
	down, up = w.SpeedLimits(core.SourceTorrent)
	assert.Equal(t, [2]int64{2 << 20, 1 << 20}, [2]int64{down, up})

	w.SetSpeedLimit(core.SourceNZB, -5, 0)
	assert.Zero(t, u.limit, "a negative limit is no limit")
}

func TestWebRuntimeLiveCombinesTrackerAndTorrentFacts(t *testing.T) {
	tr := newTracker()
	tr.apply(core.PostProcessStage{ID: "nzb", Stage: core.StageVerifying})
	tr.apply(core.DownloadProgress{ID: "nzb", BytesPerSecond: 1234})
	tq := &fakeTorrentRunner{facts: map[core.ID]trunner.Facts{
		"tor":    {Registered: true, Live: true, UpRate: 77, Peers: 4, Seeds: 2, Ratio: 2},
		"stored": {Uploaded: 500, Ratio: 0.5}, // not in the engine
	}}
	w := newWebRuntime(queues(&fakeQueue{}, &fakeQueue{}), &fakeUsenetLimiter{}, tq, tr)

	nzb := w.Live("nzb")
	assert.Equal(t, core.StageVerifying, nzb.Stage)
	assert.Equal(t, int64(1234), nzb.DownRate)
	assert.False(t, nzb.Torrent)

	tor := w.Live("tor")
	assert.True(t, tor.Torrent)
	assert.Equal(t, int64(77), tor.UpRate)
	assert.Equal(t, 4, tor.Peers)
	assert.Equal(t, 2, tor.Seeds)
	assert.InDelta(t, 2.0, tor.Ratio, 0.001)

	assert.False(t, w.Live("stored").Torrent, "no live numbers for a torrent outside the engine")
	assert.False(t, w.Live("unknown").Torrent, "not a torrent")
}

func TestWebRuntimeRatesSumTheTrackerAndTheTorrentUpload(t *testing.T) {
	tr := newTracker()
	tr.apply(core.DownloadProgress{ID: "a", BytesPerSecond: 100})
	tr.apply(core.DownloadProgress{ID: "b", BytesPerSecond: 50})
	w := newWebRuntime(queues(&fakeQueue{}, &fakeQueue{}), &fakeUsenetLimiter{}, &fakeTorrentRunner{upRate: 15}, tr)

	down, up := w.Rates()

	assert.Equal(t, int64(150), down)
	assert.Equal(t, int64(15), up)
}
