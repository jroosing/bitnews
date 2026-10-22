package sabnzbd_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/sabnzbd"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

var allStates = []core.State{
	core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching,
	core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed,
}

// stageCases are every stage the StageSource can answer with: none, each real
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

// TestStatusMappingTable covers the status mapping table of the M4 design
// for every State, every stage and both values of the paused flag.
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

					inQueue, status := sabnzbd.Classify(d, sc.stage, sc.have)

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
	inQueue, status := sabnzbd.Classify(core.Download{State: "teleporting"}, "", false)

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

func TestPostProcessingStatusFollowsTheStageSource(t *testing.T) {
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

func TestFailureTextIsHumanReadableAndRedacted(t *testing.T) {
	for _, reason := range []core.FailureReason{
		core.FailureMissingArticles, core.FailureRepairInsufficient, core.FailureArchiveUnsupported,
		core.FailureArchivePassword, core.FailureArchiveCorrupt, core.FailureDiskFull,
		core.FailurePathViolation, core.FailureSourceInvalid, core.FailureInternal, "something new",
	} {
		msg := sabnzbd.FailureText(reason, "")
		assert.NotEmpty(t, msg, reason)
		assert.NotContains(t, msg, string(reason), "the machine name is not for humans")
	}
	assert.Contains(t, sabnzbd.FailureText(core.FailureArchivePassword, ""), "password")

	msg := sabnzbd.FailureText(core.FailureInternal, "fetch http://idx.example/get?apikey=SECRET123&x=1 failed\n\x00 \x1b[31mred")

	assert.NotContains(t, msg, "SECRET123")
	assert.NotContains(t, msg, "\n")
	assert.NotContains(t, msg, "\x1b")
	assert.Contains(t, msg, "idx.example")
	long := sabnzbd.FailureText(core.FailureInternal, string(make([]byte, 0))+repeat("é", 1000))
	assert.LessOrEqual(t, len(long), 400)
	assert.True(t, utf8Valid(long))
}
