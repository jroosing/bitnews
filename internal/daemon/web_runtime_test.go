package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	trunner "gitlab.com/kyura4k/gitnews/internal/torrent/runner"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

type fakeUsenetQueue struct {
	paused bool
	reason string
	limit  int64
	calls  []string
}

func (f *fakeUsenetQueue) PauseQueue() {
	f.paused, f.reason = true, "user"
	f.calls = append(f.calls, "pause")
}
func (f *fakeUsenetQueue) ResumeQueue() {
	f.paused, f.reason = false, ""
	f.calls = append(f.calls, "resume")
}
func (f *fakeUsenetQueue) QueuePaused() (bool, string) { return f.paused, f.reason }
func (f *fakeUsenetQueue) SetSpeedLimit(n int64)       { f.limit = n }

type fakeTorrentQueue struct {
	fakeUsenetQueue
	down, up int64
	hashes   map[core.ID]trunner.Info
}

func (f *fakeTorrentQueue) SetRateLimits(down, up int64) { f.down, f.up = down, up }
func (f *fakeTorrentQueue) Info(id core.ID) (trunner.Info, bool) {
	i, ok := f.hashes[id]
	return i, ok
}

type statsEngine struct {
	core.TorrentEngine
	stats map[core.InfoHash]core.TorrentStats
	calls int
}

func (e *statsEngine) Stats(h core.InfoHash) (core.TorrentStats, error) {
	e.calls++
	st, ok := e.stats[h]
	if !ok {
		return core.TorrentStats{}, core.ErrNotFound
	}
	return st, nil
}

type listRepo struct {
	core.DownloadRepo
	byState map[core.State][]core.Download
}

func (r listRepo) List(_ context.Context, q core.DownloadQuery) ([]core.Download, string, error) {
	if q.SourceKind != core.SourceTorrent {
		return nil, "", nil
	}
	return r.byState[q.State], "", nil
}

func hash(b byte) core.InfoHash { return core.InfoHash{b} }

func TestWebRuntimeQueueStateAndControlsFollowTheRunners(t *testing.T) {
	u, tq := &fakeUsenetQueue{}, &fakeTorrentQueue{}
	w := newWebRuntime(u, tq, nil, nil, newTracker())

	assert.Equal(t, web.QueueState{Available: true}, w.QueueState(core.SourceNZB))
	w.PauseQueue(core.SourceNZB)
	w.PauseQueue(core.SourceTorrent)
	assert.Equal(t, web.QueueState{Available: true, Paused: true, Reason: "user"}, w.QueueState(core.SourceNZB))
	assert.True(t, tq.paused)
	w.ResumeQueue(core.SourceTorrent)
	assert.False(t, tq.paused)
	assert.Equal(t, []string{"pause", "resume"}, tq.calls)
	assert.Equal(t, web.QueueState{}, w.QueueState("other"))
}

func TestWebRuntimeWithoutTorrentsHasNoTorrentQueue(t *testing.T) {
	u := &fakeUsenetQueue{}
	w := newWebRuntime(u, nil, nil, nil, newTracker())

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
	u, tq := &fakeUsenetQueue{}, &fakeTorrentQueue{}
	w := newWebRuntime(u, tq, nil, nil, newTracker())

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

func TestWebRuntimeLiveCombinesTrackerAndEngine(t *testing.T) {
	tr := newTracker()
	tr.apply(core.PostProcessStage{ID: "nzb", Stage: core.StageVerifying})
	tr.apply(core.DownloadProgress{ID: "nzb", BytesPerSecond: 1234})
	tq := &fakeTorrentQueue{hashes: map[core.ID]trunner.Info{"tor": {Hash: hash(1), BaseUploaded: 500}}}
	eng := &statsEngine{stats: map[core.InfoHash]core.TorrentStats{hash(1): {Size: 1000, Uploaded: 1500, UpRate: 77, Peers: 4, Seeds: 2}}}
	w := newWebRuntime(&fakeUsenetQueue{}, tq, eng, nil, tr)

	nzb := w.Live("nzb")
	assert.Equal(t, core.StageVerifying, nzb.Stage)
	assert.Equal(t, int64(1234), nzb.DownRate)
	assert.False(t, nzb.Torrent)

	tor := w.Live("tor")
	assert.True(t, tor.Torrent)
	assert.Equal(t, int64(77), tor.UpRate)
	assert.Equal(t, 4, tor.Peers)
	assert.Equal(t, 2, tor.Seeds)
	assert.InDelta(t, 2.0, tor.Ratio, 0.001, "(earlier runs + this one) over the size")

	assert.False(t, w.Live("unknown").Torrent, "not a torrent the runner holds")
}

func TestWebRuntimeRatesSumTheTrackerAndEveryTorrentUpload(t *testing.T) {
	tr := newTracker()
	tr.apply(core.DownloadProgress{ID: "a", BytesPerSecond: 100})
	tr.apply(core.DownloadProgress{ID: "b", BytesPerSecond: 50})
	tq := &fakeTorrentQueue{hashes: map[core.ID]trunner.Info{"t1": {Hash: hash(1)}, "t2": {Hash: hash(2)}}}
	eng := &statsEngine{stats: map[core.InfoHash]core.TorrentStats{hash(1): {UpRate: 10}, hash(2): {UpRate: 5}}}
	repo := listRepo{byState: map[core.State][]core.Download{
		core.StateFetched: {{ID: "t1"}}, core.StateReady: {{ID: "t2"}, {ID: "gone"}},
	}}
	now := time.Now()
	w := newWebRuntime(&fakeUsenetQueue{}, tq, eng, repo, tr)
	w.now = func() time.Time { return now }

	down, up := w.Rates()

	assert.Equal(t, int64(150), down)
	assert.Equal(t, int64(15), up, "a seeding Ready torrent uploads too")
	callsBefore := eng.calls
	_, _ = w.Rates()
	assert.Equal(t, callsBefore, eng.calls, "cached for a second")
	now = now.Add(2 * time.Second)
	_, up = w.Rates()
	require.Greater(t, eng.calls, callsBefore)
	assert.Equal(t, int64(15), up)
}
