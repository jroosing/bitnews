package qbit_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/qbit"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// eng is what the engine says about a torrent in the state tests.
type eng struct {
	Size, DoneBytes  int64
	DownRate, UpRate int64
	// Paused is a Share Limit holding the torrent.
	Paused bool
}

// Live implements view.LiveSource.
func (e eng) Live(context.Context, core.Download) view.Live {
	return view.Live{DownRate: e.DownRate, UpRate: e.UpRate, Torrent: &view.TorrentLive{
		Hash: "00", Registered: true, Size: e.Size, DoneBytes: e.DoneBytes, LimitPaused: e.Paused,
	}}
}

// stateOf maps d and what the engine says through the view module.
func stateOf(t *testing.T, d core.Download, e eng, stopped bool) (string, float64) {
	t.Helper()
	m, err := view.New(view.Config{Live: e})
	require.NoError(t, err)
	return qbit.TorrentState(m.Of(context.Background(), d), stopped)
}

// completeLooking are the states a media manager takes for "finished
// downloading": Sonarr and Radarr import (or count as completed) a torrent in
// any of them.
var completeLooking = []string{"uploading", "stalledUP", "pausedUP", "stoppedUP", "forcedUP", "queuedUP"}

var allStates = []core.State{
	core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching,
	core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed,
	core.State("a state from the future"), core.State(""),
}

// TestACompleteLookingStateIsReportedForReadyOnly walks every combination of
// State, paused flags, priority, rates and API generation through the view
// module, and proves that
// the states a media manager imports from, and a progress of 1, appear for
// Ready Downloads and for nothing else. The Post-processing stage does not
// matter to the mapping, so it is not a dimension: every stage of
// PostProcessing is one row here.
func TestACompleteLookingStateIsReportedForReadyOnly(t *testing.T) {
	t.Parallel()
	combos := 0
	for _, st := range allStates {
		for _, paused := range []bool{false, true} {
			for _, viewPaused := range []bool{false, true} {
				for _, prio := range []core.DownloadPriority{core.DownloadPriorityNormal, core.DownloadPriorityForce, core.DownloadPriorityLow} {
					for _, up := range []int64{0, 7} {
						for _, down := range []int64{0, 7} {
							for _, done := range []int64{0, 600, 1000} { // 1000 = every byte, as at the end of the fetch
								for _, stopped := range []bool{false, true} {
									combos++
									d := core.Download{State: st, Paused: paused, Priority: prio, TotalBytes: 1000, DoneBytes: done}
									e := eng{Size: 1000, DoneBytes: done, UpRate: up, DownRate: down, Paused: viewPaused}
									name := fmt.Sprintf("%s paused=%v/%v prio=%s up=%d down=%d done=%d stopped=%v", st, paused, viewPaused, prio, up, down, done, stopped)

									state, progress := stateOf(t, d, e, stopped)

									isReady := st == core.StateReady
									assert.Equal(t, isReady, slices.Contains(completeLooking, state), "%s: state %q", name, state)
									assert.Equal(t, isReady, progress == 1, "%s: progress %v", name, progress)
									assert.GreaterOrEqual(t, progress, 0.0, name)
									assert.LessOrEqual(t, progress, 1.0, name)
									if st == core.StateFetched || st == core.StatePostProcessing {
										assert.Equal(t, "checkingUP", state, name)
										assert.InDelta(t, qbit.HoldOffProgress, progress, 0, name)
									}
									if st == core.StateFailed {
										assert.Equal(t, "error", state, name)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	require.Greater(t, combos, 1000)
}

func TestTheStateTableForTheKnownStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		d         core.Download
		e         eng
		stopped   bool
		state     string
		progress  float64
		progDelta float64
	}{
		{"magnet without metadata", core.Download{State: core.StateFetchingMetadata}, eng{}, false, "metaDL", 0, 0},
		{"added", core.Download{State: core.StateAdded}, eng{}, false, "queuedDL", 0, 0},
		{"queued", core.Download{State: core.StateQueued, TotalBytes: 100}, eng{Size: 100}, false, "queuedDL", 0, 0},
		{"queued and paused", core.Download{State: core.StateQueued, Paused: true}, eng{}, false, "pausedDL", 0, 0},
		{"queued and paused, API 2.11", core.Download{State: core.StateQueued, Paused: true}, eng{}, true, "stoppedDL", 0, 0},
		{"queued, forced", core.Download{State: core.StateQueued, Priority: core.DownloadPriorityForce}, eng{}, false, "forcedDL", 0, 0},
		{"fetching with data", core.Download{State: core.StateFetching, TotalBytes: 1000, DoneBytes: 250}, eng{Size: 1000, DoneBytes: 250, DownRate: 9}, false, "downloading", 0.25, 1e-9},
		{"fetching without data coming", core.Download{State: core.StateFetching, TotalBytes: 1000, DoneBytes: 250}, eng{Size: 1000, DoneBytes: 250}, false, "stalledDL", 0.25, 1e-9},
		{"fetching, paused", core.Download{State: core.StateFetching, Paused: true, TotalBytes: 1000, DoneBytes: 500}, eng{Size: 1000, DoneBytes: 500}, false, "pausedDL", 0.5, 1e-9},
		{"fetching, forced", core.Download{State: core.StateFetching, Priority: core.DownloadPriorityForce}, eng{DownRate: 1}, false, "forcedDL", 0, 0},
		{"fetched", core.Download{State: core.StateFetched, TotalBytes: 100, DoneBytes: 100}, eng{Size: 100, DoneBytes: 100}, false, "checkingUP", 0.9999, 1e-12},
		{"post-processing", core.Download{State: core.StatePostProcessing, TotalBytes: 100, DoneBytes: 100}, eng{Size: 100, DoneBytes: 100, UpRate: 5}, false, "checkingUP", 0.9999, 1e-12},
		{"ready and seeding", core.Download{State: core.StateReady, TotalBytes: 100, DoneBytes: 100}, eng{Size: 100, UpRate: 5}, false, "uploading", 1, 0},
		{"ready and idle", core.Download{State: core.StateReady}, eng{}, false, "stalledUP", 1, 0},
		{"ready, share limit reached", core.Download{State: core.StateReady}, eng{Paused: true}, false, "pausedUP", 1, 0},
		{"ready, share limit reached, API 2.11", core.Download{State: core.StateReady}, eng{Paused: true}, true, "stoppedUP", 1, 0},
		{"ready, forced", core.Download{State: core.StateReady, Priority: core.DownloadPriorityForce}, eng{}, false, "forcedUP", 1, 0},
		{"failed keeps the real progress", core.Download{State: core.StateFailed, TotalBytes: 100, DoneBytes: 40}, eng{Size: 100, DoneBytes: 40}, false, "error", 0.4, 1e-9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state, progress := stateOf(t, tc.d, tc.e, tc.stopped)

			assert.Equal(t, tc.state, state)
			assert.InDelta(t, tc.progress, progress, tc.progDelta)
		})
	}
}

func TestAReadyDownloadWithoutAnEngineSizeStillSaysOne(t *testing.T) {
	t.Parallel()

	_, progress := stateOf(t, core.Download{State: core.StateReady}, eng{}, false)

	assert.Equal(t, 1.0, progress)
}

// The same rule through the whole adapter: what Sonarr and Radarr see in the
// window between Fetched and Ready.
func TestTheImportHoldOffThroughTheAPI(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("fetched", inState(core.StateFetched), sized(1000, 1000))
	s.seed("unpacking", inState(core.StatePostProcessing), sized(1000, 1000))
	s.seed("seeding", ready("/data/torrents/tv/seeding"), sized(1000, 1000), func(_ *core.Download, v *torrent) { v.UpRate = 5 })
	sid := s.login()

	byName := map[string]torrentJSON{}
	for _, tj := range s.info(sid, "") {
		byName[tj.Name] = tj
	}

	for _, n := range []string{"fetched", "unpacking"} {
		assert.Equal(t, "checkingUP", byName[n].State, n)
		assert.Less(t, byName[n].Progress, 1.0, n)
		assert.InDelta(t, 0.9999, byName[n].Progress, 1e-9, n)
		assert.Positive(t, byName[n].AmountLeft, n+": something is left to do")
		assert.Equal(t, int64(-1), byName[n].CompletionOn, n)
	}
	assert.Equal(t, "uploading", byName["seeding"].State)
	assert.Equal(t, 1.0, byName["seeding"].Progress)
	assert.Zero(t, byName["seeding"].AmountLeft)
	assert.Positive(t, byName["seeding"].CompletionOn)
}
