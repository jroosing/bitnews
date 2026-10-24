package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/core"
	trunner "github.com/jroosing/bitnews/internal/torrent/runner"
)

type fakeFacts struct {
	facts map[core.ID]trunner.Facts
	calls int
}

func (f *fakeFacts) Facts(_ context.Context, id core.ID) (trunner.Facts, bool, error) {
	f.calls++
	x, ok := f.facts[id]
	return x, ok, nil
}

func TestLiveSourceTakesStageAndRateFromTheTracker(t *testing.T) {
	tr := newTracker()
	tr.apply(core.PostProcessStage{ID: "nzb", Stage: core.StageRepairing})
	tr.apply(core.DownloadProgress{ID: "nzb", BytesPerSecond: 1234})
	l := liveSource{track: tr}

	live := l.Live(context.Background(), core.Download{ID: "nzb", SourceKind: core.SourceNZB})

	assert.Equal(t, core.StageRepairing, live.Stage)
	assert.True(t, live.HaveStage)
	assert.Equal(t, int64(1234), live.DownRate)
	assert.Nil(t, live.Torrent)
}

func TestLiveSourceDoesNotAskTheTorrentRunnerAboutAnNZB(t *testing.T) {
	facts := &fakeFacts{}
	l := liveSource{track: newTracker(), torrents: facts}

	l.Live(context.Background(), core.Download{ID: "nzb", SourceKind: core.SourceNZB})

	assert.Zero(t, facts.calls)
}

func TestLiveSourceMapsTheTorrentFactsAndPrefersTheEnginesRates(t *testing.T) {
	own := 2.0
	facts := &fakeFacts{facts: map[core.ID]trunner.Facts{"tor": {
		Hash: core.InfoHash{1}, Registered: true, Live: true, Size: 2000, DoneBytes: 500,
		DownRate: 77, UpRate: 55, Peers: 4, Seeds: 2, Private: true,
		Uploaded: 3000, SeedingTime: time.Hour, OwnRatioLimit: &own, LimitPaused: true,
	}}}
	tr := newTracker()
	tr.apply(core.DownloadProgress{ID: "tor", BytesPerSecond: 9})
	l := liveSource{track: tr, torrents: facts}

	live := l.Live(context.Background(), core.Download{ID: "tor", SourceKind: core.SourceTorrent})

	assert.Equal(t, int64(77), live.DownRate)
	assert.Equal(t, int64(55), live.UpRate)
	if assert.NotNil(t, live.Torrent) {
		assert.Equal(t, core.InfoHash{1}.String(), live.Torrent.Hash)
		assert.True(t, live.Torrent.Registered)
		assert.Equal(t, int64(2000), live.Torrent.Size)
		assert.Equal(t, int64(500), live.Torrent.DoneBytes)
		assert.Equal(t, 4, live.Torrent.Peers)
		assert.True(t, live.Torrent.Private)
		assert.Equal(t, int64(3000), live.Torrent.Uploaded)
		assert.Equal(t, &own, live.Torrent.OwnRatio)
		assert.True(t, live.Torrent.LimitPaused)
	}
}

func TestLiveSourceKeepsTheTrackerRateWhenTheEngineDidNotAnswer(t *testing.T) {
	facts := &fakeFacts{facts: map[core.ID]trunner.Facts{"tor": {Hash: core.InfoHash{1}, Size: 100}}}
	tr := newTracker()
	tr.apply(core.DownloadProgress{ID: "tor", BytesPerSecond: 9})
	l := liveSource{track: tr, torrents: facts}

	live := l.Live(context.Background(), core.Download{ID: "tor", SourceKind: core.SourceTorrent})

	assert.Equal(t, int64(9), live.DownRate)
	assert.NotNil(t, live.Torrent)
}

func TestLiveSourceWithoutATorrentSideHasNoTorrentFacts(t *testing.T) {
	l := liveSource{track: newTracker()}

	live := l.Live(context.Background(), core.Download{ID: "tor", SourceKind: core.SourceTorrent})

	assert.Nil(t, live.Torrent)
}
