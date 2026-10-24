package core_test

import (
	"errors"
	"testing"

	"github.com/jroosing/bitnews/internal/core"
)

var allStates = []core.State{
	core.StateAdded,
	core.StateFetchingMetadata,
	core.StateQueued,
	core.StateFetching,
	core.StateFetched,
	core.StatePostProcessing,
	core.StateReady,
	core.StateFailed,
}

// legalMoves mirrors the lifecycle diagram in docs/architecture.md, plus the
// moves the M3 and M5 designs add. Every pair not listed must be rejected.
var legalMoves = map[[2]core.State]string{
	{core.StateAdded, core.StateQueued}:            "added NZB or .torrent enters its Queue",
	{core.StateAdded, core.StateFetchingMetadata}:  "added magnet link fetches its metadata",
	{core.StateFetchingMetadata, core.StateQueued}: "magnet with received metadata enters its Queue",
	{core.StateFetchingMetadata, core.StateFailed}: "magnet metadata timeout fails the Download",
	{core.StateQueued, core.StateFetching}:         "Queue activates a Download",
	{core.StateQueued, core.StateFailed}:           "queued Download fails before it is activated",
	{core.StateFetching, core.StateQueued}:         "Queue deactivates a Download, or a restart requeues it",
	{core.StateFetching, core.StateFetched}:        "all data is received",
	{core.StateFetching, core.StateFailed}:         "fetching fails",
	{core.StateFetched, core.StatePostProcessing}:  "Fetched Download starts Post-processing",
	{core.StateFetched, core.StateReady}:           "torrent without archives is Ready right away",
	{core.StateFetched, core.StateFailed}:          "torrent move to complete fails",
	{core.StatePostProcessing, core.StateFetching}: "Repair needs deferred recovery volumes fetched",
	{core.StatePostProcessing, core.StateReady}:    "Post-processing succeeds",
	{core.StatePostProcessing, core.StateFailed}:   "Post-processing fails",
}

func TestTransitionAllowsExactlyTheLifecycleMoves(t *testing.T) {
	for _, from := range allStates {
		for _, to := range allStates {
			err := core.Transition(from, to)
			if behavior, legal := legalMoves[[2]core.State{from, to}]; legal {
				if err != nil {
					t.Errorf("%s -> %s (%s): got %v, want nil", from, to, behavior, err)
				}
				continue
			}
			if !errors.Is(err, core.ErrIllegalTransition) {
				t.Errorf("%s -> %s: got %v, want ErrIllegalTransition", from, to, err)
			}
		}
	}
}

func TestTransitionRejectsUnknownStates(t *testing.T) {
	for _, s := range allStates {
		if err := core.Transition("bogus", s); !errors.Is(err, core.ErrIllegalTransition) {
			t.Errorf("bogus -> %s: got %v, want ErrIllegalTransition", s, err)
		}
		if err := core.Transition(s, "bogus"); !errors.Is(err, core.ErrIllegalTransition) {
			t.Errorf("%s -> bogus: got %v, want ErrIllegalTransition", s, err)
		}
	}
}

func TestInQueueIsTrueForTheStatesBeforeFetched(t *testing.T) {
	for state, want := range map[core.State]bool{
		core.StateAdded: true, core.StateFetchingMetadata: true, core.StateQueued: true, core.StateFetching: true,
		core.StateFetched: false, core.StatePostProcessing: false, core.StateReady: false, core.StateFailed: false,
		"unknown": false,
	} {
		if got := state.InQueue(); got != want {
			t.Errorf("%s.InQueue() = %v, want %v", state, got, want)
		}
	}
}
