package sabnzbd_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/sabnzbd"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

var allStates = []core.State{
	core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching,
	core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed,
}

// stageCases are every stage the live source can answer with: none, each real
// stage, and a value from a future version.
var stageCases = []struct {
	name  string
	stage core.Stage
	have  bool
}{
	{"unknown", "", false},
	{"verifying", core.StageVerifying, true},
	{"repairing", core.StageRepairing, true},
	{"unpacking", core.StageUnpacking, true},
	{"finishing", core.StageFinishing, true},
	{"future stage", "reticulating", true},
}

// stageLive answers one stage for every Download.
type stageLive struct {
	stage core.Stage
	have  bool
}

func (l stageLive) Live(context.Context, core.Download) view.Live {
	return view.Live{Stage: l.stage, HaveStage: l.have}
}

// classify builds the View of d with the given stage and translates it.
func classify(t *testing.T, d core.Download, stage core.Stage, have bool) (bool, string) {
	t.Helper()
	m, err := view.New(view.Config{Live: stageLive{stage, have}})
	require.NoError(t, err)
	return sabnzbd.Classify(m.Of(context.Background(), d))
}

// TestStatusMappingTable covers the status mapping table of the M4 design
// for every State, every stage and both values of the paused flag, through
// the view module.
func TestStatusMappingTable(t *testing.T) {
	postProcessing := map[core.Stage]string{
		core.StageVerifying: "Verifying", core.StageRepairing: "Repairing",
		core.StageUnpacking: "Extracting", core.StageFinishing: "Moving",
	}
	for _, st := range allStates {
		for _, sc := range stageCases {
			for _, paused := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/paused=%v", st, sc.name, paused), func(t *testing.T) {
					d := core.Download{State: st, Paused: paused}

					inQueue, status := classify(t, d, sc.stage, sc.have)

					wantQueue := st == core.StateAdded || st == core.StateFetchingMetadata || st == core.StateQueued || st == core.StateFetching
					assert.Equal(t, wantQueue, inQueue, "which list")
					assert.Equal(t, st == core.StateReady, status == "Completed",
						"Completed only for Ready, got %q", status)
					assert.Equal(t, st == core.StateFailed, status == "Failed")
					switch st {
					case core.StateAdded, core.StateFetchingMetadata, core.StateQueued:
						assert.Equal(t, map[bool]string{false: "Queued", true: "Paused"}[paused], status)
					case core.StateFetching:
						assert.Equal(t, map[bool]string{false: "Downloading", true: "Paused"}[paused], status)
					case core.StateFetched:
						assert.Equal(t, "Verifying", status)
					case core.StatePostProcessing:
						want, known := postProcessing[sc.stage]
						if !sc.have || !known {
							want = "Verifying"
						}
						assert.Equal(t, want, status)
					case core.StateReady:
						assert.Equal(t, "Completed", status)
					case core.StateFailed:
						assert.Equal(t, "Failed", status)
					}
				})
			}
		}
	}
}

func TestAnUnknownStateIsNeverCompleted(t *testing.T) {
	inQueue, status := classify(t, core.Download{State: "teleporting"}, "", false)

	assert.False(t, inQueue)
	assert.Equal(t, "Queued", status)
}

// TestOverHTTPNoDownloadIsCompletedBeforeReady seeds one Download per State
// and stage and reads both lists: only the Ready one may say Completed, and
// the queue list never holds a history State.
func TestOverHTTPNoDownloadIsCompletedBeforeReady(t *testing.T) {
	s := newStack(t)
	byID := map[string]core.State{}
	for _, st := range allStates {
		if st == core.StateFetchingMetadata {
			continue // torrent-only: never an NZB in the store
		}
		for _, sc := range stageCases {
			mut := []func(*core.Download){inState(st)}
			if st == core.StateReady {
				mut = append(mut, ready("/data/out"))
			}
			if st == core.StateFailed {
				mut = append(mut, failed(core.FailureInternal, ""))
			}
			d := s.seed(fmt.Sprintf("%s-%s", st, sc.name), mut...)
			if sc.have {
				s.setStage(d.ID, sc.stage)
			}
			byID[nzo(d)] = st
		}
	}

	var q struct {
		Queue struct {
			Slots []struct {
				NzoID  string `json:"nzo_id"`
				Status string `json:"status"`
			} `json:"slots"`
		} `json:"queue"`
	}
	var h struct {
		History struct {
			Slots []struct {
				NzoID  string `json:"nzo_id"`
				Status string `json:"status"`
			} `json:"slots"`
		} `json:"history"`
	}
	body(t, s.call("mode=queue"), &q)
	body(t, s.call("mode=history"), &h)

	require.Len(t, q.Queue.Slots, 3*len(stageCases), "Added, Queued and Fetching")
	for _, slot := range q.Queue.Slots {
		assert.Contains(t, []core.State{core.StateAdded, core.StateQueued, core.StateFetching}, byID[slot.NzoID])
		assert.NotEqual(t, "Completed", slot.Status)
	}
	require.Len(t, h.History.Slots, 4*len(stageCases), "Fetched, PostProcessing, Ready, Failed")
	for _, slot := range h.History.Slots {
		assert.Equal(t, byID[slot.NzoID] == core.StateReady, slot.Status == "Completed", "%s %s", byID[slot.NzoID], slot.Status)
	}
}

// TestPostProcessingStatusFollowsTheStageThroughTheView: the history status
// and action line follow the stage the view's live source reports.
func TestPostProcessingStatusFollowsTheStageThroughTheView(t *testing.T) {
	s := newStack(t)
	d := s.seed("Show.S01E01", inState(core.StatePostProcessing))
	statusOf := func() string {
		var h struct {
			History struct {
				Slots []struct {
					Status     string `json:"status"`
					ActionLine string `json:"action_line"`
				} `json:"slots"`
			} `json:"history"`
		}
		body(t, s.call("mode=history"), &h)
		require.Len(t, h.History.Slots, 1)
		return h.History.Slots[0].Status + "/" + h.History.Slots[0].ActionLine
	}

	assert.Equal(t, "Verifying/Verifying", statusOf(), "no stage known yet")
	s.setStage(d.ID, core.StageRepairing)
	assert.Equal(t, "Repairing/Repairing", statusOf())
	s.setStage(d.ID, core.StageUnpacking)
	assert.Equal(t, "Extracting/Extracting", statusOf())
	s.setStage(d.ID, core.StageFinishing)
	assert.Equal(t, "Moving/Moving", statusOf())
}

func TestPriorityMappingBothDirections(t *testing.T) {
	for _, tc := range []struct {
		sab    int
		prio   core.DownloadPriority
		paused bool
	}{
		{2, core.DownloadPriorityForce, false},
		{1, core.DownloadPriorityHigh, false},
		{0, core.DownloadPriorityNormal, false},
		{-1, core.DownloadPriorityLow, false},
		{-100, core.DownloadPriorityNormal, false}, // default
		{-2, core.DownloadPriorityNormal, true},    // paused
		{99, core.DownloadPriorityNormal, false},   // unknown
	} {
		prio, paused := sabnzbd.PriorityFromSAB(tc.sab)
		assert.Equal(t, tc.prio, prio, "SAB %d", tc.sab)
		assert.Equal(t, tc.paused, paused, "SAB %d", tc.sab)
	}
	for sab, prio := range map[int]core.DownloadPriority{
		2: core.DownloadPriorityForce, 1: core.DownloadPriorityHigh, 0: core.DownloadPriorityNormal, -1: core.DownloadPriorityLow,
	} {
		assert.Equal(t, sab, sabnzbd.PriorityToSAB(prio))
		back, _ := sabnzbd.PriorityFromSAB(sabnzbd.PriorityToSAB(prio))
		assert.Equal(t, prio, back, "round trip")
	}
}

// TestFailureTextIsHumanReadable covers SAB's wording; cleaning the detail
// is the view's (view.CleanDetail), and the redacted fail_message is checked
// over HTTP in the history tests.
func TestFailureTextIsHumanReadable(t *testing.T) {
	for _, reason := range []core.FailureReason{
		core.FailureMissingArticles, core.FailureRepairInsufficient, core.FailureArchiveUnsupported,
		core.FailureArchivePassword, core.FailureArchiveCorrupt, core.FailureDiskFull,
		core.FailurePathViolation, core.FailureSourceInvalid, core.FailureInternal, "something new",
	} {
		msg := sabnzbd.FailureText(view.Failure{Reason: reason})
		assert.NotEmpty(t, msg, reason)
		assert.NotContains(t, msg, string(reason), "the machine name is not for humans")
	}
	assert.Contains(t, sabnzbd.FailureText(view.Failure{Reason: core.FailureArchivePassword}), "password")
	assert.Equal(t, "Download failed: disk on fire",
		sabnzbd.FailureText(view.Failure{Reason: core.FailureInternal, Detail: "disk on fire"}))
}
