package core_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// schedDL builds a normal-priority Download in the given state. Its AddedAt
// is derived from pos so that tests control the tiebreakers explicitly.
func schedDL(id string, state core.State, mods ...func(*core.Download)) core.Download {
	d := core.Download{
		ID:         core.ID(id),
		State:      state,
		Priority:   core.DownloadPriorityNormal,
		SourceKind: core.SourceNZB,
		AddedAt:    now,
	}
	for _, m := range mods {
		m(&d)
	}
	return d
}

func prio(p core.DownloadPriority) func(*core.Download) {
	return func(d *core.Download) { d.Priority = p }
}

func pos(n int) func(*core.Download) {
	return func(d *core.Download) { d.QueuePosition = n }
}

func paused(d *core.Download) { d.Paused = true }

func TestSelectActivatesQueuedDownloadWithinLimit(t *testing.T) {
	downloads := []core.Download{schedDL("a", core.StateQueued)}

	activate, deactivate := core.Select(downloads, 1)

	if want := []core.ID{"a"}; !slices.Equal(activate, want) {
		t.Errorf("activate = %v, want %v", activate, want)
	}
	if len(deactivate) != 0 {
		t.Errorf("deactivate = %v, want none", deactivate)
	}
}

func TestSelectActivatesAtMostLimitDownloads(t *testing.T) {
	downloads := []core.Download{
		schedDL("a", core.StateQueued, pos(0)),
		schedDL("b", core.StateQueued, pos(1)),
		schedDL("c", core.StateQueued, pos(2)),
	}
	tests := []struct {
		name  string
		limit int
		want  []core.ID
	}{
		{"zero activates none", 0, nil},
		{"negative is treated as zero", -3, nil},
		{"one activates the first", 1, []core.ID{"a"}},
		{"above the queue length activates all", 5, []core.ID{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activate, deactivate := core.Select(downloads, tt.limit)

			if !slices.Equal(activate, tt.want) {
				t.Errorf("activate = %v, want %v", activate, tt.want)
			}
			if len(deactivate) != 0 {
				t.Errorf("deactivate = %v, want none", deactivate)
			}
		})
	}
}

func TestSelectPrefersHigherDownloadPriority(t *testing.T) {
	downloads := []core.Download{
		schedDL("low", core.StateQueued, prio(core.DownloadPriorityLow), pos(0)),
		schedDL("normal", core.StateQueued, prio(core.DownloadPriorityNormal), pos(1)),
		schedDL("high", core.StateQueued, prio(core.DownloadPriorityHigh), pos(2)),
	}

	activate, _ := core.Select(downloads, 2)

	if want := []core.ID{"high", "normal"}; !slices.Equal(activate, want) {
		t.Errorf("activate = %v, want %v", activate, want)
	}
}

func TestSelectBreaksPriorityTiesByQueuePositionThenAddedAtThenID(t *testing.T) {
	added := func(offset time.Duration) func(*core.Download) {
		return func(d *core.Download) { d.AddedAt = now.Add(offset) }
	}
	tests := []struct {
		name      string
		downloads []core.Download
		want      []core.ID
	}{
		{
			"lower queue position first",
			[]core.Download{
				schedDL("a", core.StateQueued, pos(2)),
				schedDL("b", core.StateQueued, pos(0)),
				schedDL("c", core.StateQueued, pos(1)),
			},
			[]core.ID{"b", "c", "a"},
		},
		{
			"same position: earlier AddedAt first",
			[]core.Download{
				schedDL("a", core.StateQueued, pos(0), added(2*time.Minute)),
				schedDL("b", core.StateQueued, pos(0), added(0)),
				schedDL("c", core.StateQueued, pos(0), added(time.Minute)),
			},
			[]core.ID{"b", "c", "a"},
		},
		{
			"same position and AddedAt: smaller ID first",
			[]core.Download{
				schedDL("c", core.StateQueued, pos(0)),
				schedDL("a", core.StateQueued, pos(0)),
				schedDL("b", core.StateQueued, pos(0)),
			},
			[]core.ID{"a", "b", "c"},
		},
		{
			"position outranks AddedAt",
			[]core.Download{
				schedDL("old", core.StateQueued, pos(1), added(0)),
				schedDL("new", core.StateQueued, pos(0), added(time.Hour)),
			},
			[]core.ID{"new", "old"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activate, _ := core.Select(tt.downloads, len(tt.downloads))

			if !slices.Equal(activate, tt.want) {
				t.Errorf("activate = %v, want %v", activate, tt.want)
			}
		})
	}
}

func TestSelectKeepsFetchingDownloadsThatStillFit(t *testing.T) {
	downloads := []core.Download{
		schedDL("fetching", core.StateFetching, pos(0)),
		schedDL("waiting", core.StateQueued, pos(1)),
	}

	activate, deactivate := core.Select(downloads, 1)

	if len(activate) != 0 || len(deactivate) != 0 {
		t.Errorf("activate = %v, deactivate = %v, want both empty", activate, deactivate)
	}
}

func TestSelectSwapsFetchingDownloadForHigherPriorityArrival(t *testing.T) {
	downloads := []core.Download{
		schedDL("fetching", core.StateFetching, pos(0)),
		schedDL("urgent", core.StateQueued, prio(core.DownloadPriorityHigh), pos(1)),
	}

	activate, deactivate := core.Select(downloads, 1)

	if want := []core.ID{"urgent"}; !slices.Equal(activate, want) {
		t.Errorf("activate = %v, want %v", activate, want)
	}
	if want := []core.ID{"fetching"}; !slices.Equal(deactivate, want) {
		t.Errorf("deactivate = %v, want %v", deactivate, want)
	}
}

func TestSelectNeverActivatesPausedDownloads(t *testing.T) {
	downloads := []core.Download{
		schedDL("paused-queued", core.StateQueued, paused, pos(0)),
		schedDL("paused-fetching", core.StateFetching, paused, pos(1)),
		schedDL("paused-force", core.StateQueued, paused, prio(core.DownloadPriorityForce), pos(2)),
		schedDL("next", core.StateQueued, pos(3)),
	}

	activate, deactivate := core.Select(downloads, 1)

	if want := []core.ID{"next"}; !slices.Equal(activate, want) {
		t.Errorf("activate = %v, want %v (paused Downloads must not use a slot)", activate, want)
	}
	if want := []core.ID{"paused-fetching"}; !slices.Equal(deactivate, want) {
		t.Errorf("deactivate = %v, want %v", deactivate, want)
	}
}

func TestSelectForceDownloadsIgnoreTheLimit(t *testing.T) {
	downloads := []core.Download{
		schedDL("normal", core.StateQueued, pos(0)),
		schedDL("force-1", core.StateQueued, prio(core.DownloadPriorityForce), pos(5)),
		schedDL("force-2", core.StateQueued, prio(core.DownloadPriorityForce), pos(6)),
		schedDL("force-3", core.StateFetching, prio(core.DownloadPriorityForce), pos(7)),
	}
	tests := []struct {
		name         string
		limit        int
		wantActivate []core.ID
	}{
		{"limit 0 still runs every force Download", 0, []core.ID{"force-1", "force-2"}},
		{"force Downloads do not use up the limit", 1, []core.ID{"force-1", "force-2", "normal"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activate, deactivate := core.Select(downloads, tt.limit)

			if !slices.Equal(activate, tt.wantActivate) {
				t.Errorf("activate = %v, want %v", activate, tt.wantActivate)
			}
			if len(deactivate) != 0 {
				t.Errorf("deactivate = %v, want none", deactivate)
			}
		})
	}
}

func TestSelectDeactivatesFetchingDownloadsBeyondTheLimit(t *testing.T) {
	downloads := []core.Download{
		schedDL("a", core.StateFetching, pos(0)),
		schedDL("b", core.StateFetching, pos(1)),
		schedDL("c", core.StateFetching, pos(2)),
	}

	activate, deactivate := core.Select(downloads, 1)

	if len(activate) != 0 {
		t.Errorf("activate = %v, want none", activate)
	}
	if want := []core.ID{"b", "c"}; !slices.Equal(deactivate, want) {
		t.Errorf("deactivate = %v, want %v", deactivate, want)
	}
}

func TestSelectIgnoresDownloadsThatAreNotQueuedOrFetching(t *testing.T) {
	var downloads []core.Download
	for i, s := range []core.State{
		core.StateAdded, core.StateFetchingMetadata, core.StateFetched,
		core.StatePostProcessing, core.StateReady, core.StateFailed,
	} {
		downloads = append(downloads, schedDL("other-"+string(s), s, prio(core.DownloadPriorityHigh), pos(i)))
	}
	downloads = append(downloads, schedDL("queued", core.StateQueued, pos(10)))

	activate, deactivate := core.Select(downloads, 1)

	if want := []core.ID{"queued"}; !slices.Equal(activate, want) {
		t.Errorf("activate = %v, want %v", activate, want)
	}
	if len(deactivate) != 0 {
		t.Errorf("deactivate = %v, want none", deactivate)
	}
}

func TestSelectDoesNotModifyItsInput(t *testing.T) {
	downloads := []core.Download{
		schedDL("b", core.StateQueued, pos(1)),
		schedDL("a", core.StateQueued, pos(0)),
	}
	before := slices.Clone(downloads)

	core.Select(downloads, 1)

	if !slices.Equal(downloads, before) {
		t.Errorf("input changed: %+v, want %+v", downloads, before)
	}
}

// applySelection plays the caller's part: activate moves Queued Downloads to
// Fetching, deactivate moves Fetching ones back to Queued.
func applySelection(t *testing.T, downloads []core.Download, activate, deactivate []core.ID) []core.Download {
	t.Helper()
	next := slices.Clone(downloads)
	set := func(ids []core.ID, from, to core.State) {
		for _, id := range ids {
			i := slices.IndexFunc(next, func(d core.Download) bool { return d.ID == id })
			if i < 0 || next[i].State != from {
				t.Fatalf("Select returned %q, which is not a %s Download", id, from)
			}
			next[i].State = to
		}
	}
	set(activate, core.StateQueued, core.StateFetching)
	set(deactivate, core.StateFetching, core.StateQueued)
	return next
}

func TestSelectHoldsItsInvariantsForRandomQueues(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	states := []core.State{
		core.StateQueued, core.StateQueued, core.StateFetching, core.StateFetching,
		core.StateAdded, core.StateFetched, core.StateReady, core.StateFailed,
	}
	priorities := []core.DownloadPriority{
		core.DownloadPriorityForce, core.DownloadPriorityHigh,
		core.DownloadPriorityNormal, core.DownloadPriorityLow,
	}
	for range 2000 {
		downloads := make([]core.Download, rng.IntN(15))
		for i := range downloads {
			downloads[i] = schedDL(fmt.Sprintf("d%02d", i), states[rng.IntN(len(states))],
				prio(priorities[rng.IntN(len(priorities))]),
				pos(rng.IntN(5)),
				func(d *core.Download) {
					d.Paused = rng.IntN(4) == 0
					d.AddedAt = now.Add(time.Duration(rng.IntN(3)) * time.Minute)
				})
		}
		limit := rng.IntN(9) - 2
		wantSlots := 0 // non-force Downloads that may be active
		for _, d := range downloads {
			isCandidate := d.State == core.StateQueued || d.State == core.StateFetching
			if isCandidate && !d.Paused && d.Priority != core.DownloadPriorityForce {
				wantSlots++
			}
		}
		wantSlots = min(wantSlots, max(limit, 0))

		activate, deactivate := core.Select(downloads, limit)
		after := applySelection(t, downloads, activate, deactivate)

		gotSlots := 0
		for _, d := range after {
			isCandidate := d.State == core.StateQueued || d.State == core.StateFetching
			active := d.State == core.StateFetching
			switch {
			case !isCandidate:
			case d.Paused && active:
				t.Fatalf("limit %d: paused %s is active\n%+v", limit, d.ID, downloads)
			case d.Priority == core.DownloadPriorityForce && !d.Paused && !active:
				t.Fatalf("limit %d: force %s is not active\n%+v", limit, d.ID, downloads)
			case d.Priority != core.DownloadPriorityForce && active:
				gotSlots++
			}
		}
		if gotSlots != wantSlots {
			t.Fatalf("limit %d: %d non-force Downloads active, want %d\n%+v", limit, gotSlots, wantSlots, downloads)
		}

		again, undo := core.Select(after, limit)
		if len(again) != 0 || len(undo) != 0 {
			t.Fatalf("limit %d: not idempotent: activate %v, deactivate %v\n%+v", limit, again, undo, downloads)
		}
	}
}
