package core_test

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// orderQueue returns Downloads a, b, c, d at positions 0..3, deliberately
// listed out of order.
func orderQueue() []core.Download {
	return []core.Download{
		schedDL("c", core.StateQueued, pos(2)),
		schedDL("a", core.StateQueued, pos(0)),
		schedDL("d", core.StateFetching, pos(3)),
		schedDL("b", core.StateQueued, pos(1)),
	}
}

func TestMoveToTopPutsTheDownloadFirstAndKeepsTheOthersInOrder(t *testing.T) {
	got, err := core.MoveToTop(orderQueue(), "c")
	if err != nil {
		t.Fatalf("MoveToTop: %v", err)
	}

	want := map[core.ID]int{"c": 0, "a": 1, "b": 2, "d": 3}
	if !maps.Equal(got, want) {
		t.Errorf("positions = %v, want %v", got, want)
	}
}

func TestMoveToTopRejectsAnUnknownDownload(t *testing.T) {
	_, err := core.MoveToTop(orderQueue(), "nope")

	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestMoveToBottomPutsTheDownloadLastAndKeepsTheOthersInOrder(t *testing.T) {
	got, err := core.MoveToBottom(orderQueue(), "b")
	if err != nil {
		t.Fatalf("MoveToBottom: %v", err)
	}

	want := map[core.ID]int{"a": 0, "c": 1, "d": 2, "b": 3}
	if !maps.Equal(got, want) {
		t.Errorf("positions = %v, want %v", got, want)
	}
}

func TestMoveUpAndDownSwapWithTheNeighbour(t *testing.T) {
	tests := []struct {
		name string
		move func([]core.Download, core.ID) (map[core.ID]int, error)
		id   core.ID
		want map[core.ID]int
	}{
		{"up from the middle", core.MoveUp, "c", map[core.ID]int{"a": 0, "c": 1, "b": 2, "d": 3}},
		{"down from the middle", core.MoveDown, "b", map[core.ID]int{"a": 0, "c": 1, "b": 2, "d": 3}},
		{"up at the top changes nothing", core.MoveUp, "a", map[core.ID]int{"a": 0, "b": 1, "c": 2, "d": 3}},
		{"down at the bottom changes nothing", core.MoveDown, "d", map[core.ID]int{"a": 0, "b": 1, "c": 2, "d": 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.move(orderQueue(), tt.id)
			if err != nil {
				t.Fatalf("move: %v", err)
			}

			if !maps.Equal(got, tt.want) {
				t.Errorf("positions = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMovesRenumberGapsAndDuplicatesDensely(t *testing.T) {
	// Positions 5, 5, 9, 9 are ties; AddedAt then ID decide their order.
	downloads := []core.Download{
		schedDL("d", core.StateQueued, pos(9)),
		schedDL("b", core.StateQueued, pos(5)),
		schedDL("c", core.StateQueued, pos(9)),
		schedDL("a", core.StateQueued, pos(5)),
	}

	got, err := core.MoveUp(downloads, "a") // already first: renumber only
	if err != nil {
		t.Fatalf("MoveUp: %v", err)
	}

	want := map[core.ID]int{"a": 0, "b": 1, "c": 2, "d": 3}
	if !maps.Equal(got, want) {
		t.Errorf("positions = %v, want %v", got, want)
	}
}

func TestMovesRejectAnUnknownDownload(t *testing.T) {
	moves := map[string]func([]core.Download, core.ID) (map[core.ID]int, error){
		"top": core.MoveToTop, "bottom": core.MoveToBottom, "up": core.MoveUp, "down": core.MoveDown,
	}
	for name, move := range moves {
		t.Run(name, func(t *testing.T) {
			got, err := move(orderQueue(), "nope")

			if !errors.Is(err, core.ErrNotFound) {
				t.Errorf("got %v, want ErrNotFound", err)
			}
			if got != nil {
				t.Errorf("positions = %v, want nil", got)
			}
		})
	}
}

func TestMovesDoNotModifyTheirInput(t *testing.T) {
	downloads := orderQueue()
	before := slices.Clone(downloads)

	if _, err := core.MoveToTop(downloads, "d"); err != nil {
		t.Fatalf("MoveToTop: %v", err)
	}

	if !slices.Equal(downloads, before) {
		t.Errorf("input changed: %+v, want %+v", downloads, before)
	}
}

func TestMovesAssignEveryDownloadAUniquePositionFromZero(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	moves := []func([]core.Download, core.ID) (map[core.ID]int, error){
		core.MoveToTop, core.MoveToBottom, core.MoveUp, core.MoveDown,
	}
	for range 500 {
		downloads := make([]core.Download, 1+rng.IntN(10))
		for i := range downloads {
			downloads[i] = schedDL(fmt.Sprintf("d%02d", i), core.StateQueued, pos(rng.IntN(20)-5))
		}
		id := downloads[rng.IntN(len(downloads))].ID

		got, err := moves[rng.IntN(len(moves))](downloads, id)
		if err != nil {
			t.Fatalf("move: %v", err)
		}

		seen := make([]bool, len(downloads))
		for _, d := range downloads {
			p, ok := got[d.ID]
			if !ok || p < 0 || p >= len(downloads) || seen[p] {
				t.Fatalf("positions %v are not dense 0..%d for %+v", got, len(downloads)-1, downloads)
			}
			seen[p] = true
		}
	}
}
