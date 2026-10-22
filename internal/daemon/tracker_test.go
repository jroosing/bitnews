package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestTrackerKeepsTheStageUntilTheDownloadIsDone(t *testing.T) {
	tr := newTracker()
	tr.apply(core.PostProcessStage{ID: "a", Stage: core.StageRepairing})
	st, ok := tr.stage("a")
	assert.True(t, ok)
	assert.Equal(t, core.StageRepairing, st)

	tr.apply(core.DownloadUpdated{Download: core.Download{ID: "a", State: core.StatePostProcessing}})
	_, ok = tr.stage("a")
	assert.True(t, ok, "still post-processing")
	tr.apply(core.DownloadUpdated{Download: core.Download{ID: "a", State: core.StateFetching}})
	_, ok = tr.stage("a")
	assert.False(t, ok, "handed back to the fetch side")

	for _, ev := range []core.Event{core.Ready{ID: "b"}, core.Failed{ID: "b"}, core.Removed{ID: "b"}} {
		tr.apply(core.PostProcessStage{ID: "b", Stage: core.StageUnpacking})
		tr.apply(ev)
		_, ok = tr.stage("b")
		assert.False(t, ok, "%T clears the stage", ev)
	}
}

func TestTrackerSumsTheRatesOfDownloadsBeingFetched(t *testing.T) {
	tr := newTracker()
	tr.apply(core.DownloadProgress{ID: "a", BytesPerSecond: 100})
	tr.apply(core.DownloadProgress{ID: "b", BytesPerSecond: 50})
	assert.EqualValues(t, 150, tr.speed())

	tr.apply(core.Fetched{ID: "a"})
	tr.apply(core.DownloadUpdated{Download: core.Download{ID: "b", State: core.StateQueued}})
	assert.EqualValues(t, 0, tr.speed())
}
