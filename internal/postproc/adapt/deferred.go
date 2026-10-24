package adapt

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/usenet/nzb"
)

// DefaultMargin is the number of recovery blocks fetched beyond what the
// damage needs, in case one of the fetched volumes turns out damaged.
const DefaultMargin = 1

// exactCellLimit bounds the size of the exact planner's table (volumes times
// target blocks). Beyond it PlanVolumes falls back to the greedy planner.
const exactCellLimit = 1 << 22

// volume is a candidate PAR2 recovery volume: an index into the caller's
// files, the blocks it holds and what it costs to fetch.
type volume struct {
	index  int
	blocks int
	bytes  int64
}

// volumesOf returns the usable volumes of files, in file order. Files that
// are not PAR2 volumes, or that claim no recovery blocks, are skipped.
func volumesOf(files []nzb.File) []volume {
	var vols []volume
	for i, f := range files {
		if f.Kind != nzb.KindPar2Volume || f.RecoveryBlocks <= 0 {
			continue
		}
		vols = append(vols, volume{index: i, blocks: f.RecoveryBlocks, bytes: fileBytes(f)})
	}
	return vols
}

// fileBytes is the sum of the segment sizes of f, saturating.
func fileBytes(f nzb.File) int64 {
	var n int64
	for _, s := range f.Segments {
		n = satAdd64(n, max(s.Bytes, 0))
	}
	return n
}

func satAdd64(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func satAddInt(a, b int) int {
	if a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// DeferredBytes is the number of bytes needed to fetch every PAR2 recovery
// volume in files (other files are ignored).
func DeferredBytes(files []nzb.File) int64 {
	var n int64
	for _, v := range volumesOf(files) {
		n = satAdd64(n, v.bytes)
	}
	return n
}

// PlanVolumes picks the recovery volumes to fetch (indexes into files) so
// that their blocks cover needed+margin, at the lowest cost in bytes. Only
// files of Kind KindPar2Volume with RecoveryBlocks > 0 are candidates. A
// negative margin counts as 0.
//
// The result picks is sorted by index; covered is the sum of the picks'
// blocks. ok is false when all volumes together cannot cover needed (the
// margin is best effort: when the volumes cover needed but not needed+margin,
// all of them are picked). When needed is not positive nothing has to be
// fetched and PlanVolumes returns no picks, covered 0 and ok true.
//
// Algorithm. Choosing the cheapest set of volumes with at least T blocks is
// a covering knapsack. PlanVolumes solves it exactly by dynamic programming
// over the blocks covered (capped at T = min(needed+margin, total blocks)),
// which takes O(volumes x T) time and memory, and as the tables of that
// size are only small for realistic sets, it uses it while volumes x T is
// at most about four million cells. Larger inputs (hostile names claiming
// huge block counts) use a greedy planner: volumes by ascending bytes per
// block until T is covered, then redundant volumes (most expensive first)
// are dropped. The greedy plan is always valid but not always cheapest.
//
// Among plans of equal bytes the one with fewer volumes wins, and then the
// one that prefers earlier files.
func PlanVolumes(files []nzb.File, needed, margin int) (picks []int, covered int, ok bool) {
	if needed <= 0 {
		return nil, 0, true
	}
	vols := volumesOf(files)
	total := 0
	for _, v := range vols {
		total = satAddInt(total, v.blocks)
	}
	if total < needed {
		return nil, 0, false
	}
	target := min(satAddInt(needed, max(margin, 0)), total)

	var chosen []volume
	if uint64(len(vols))*uint64(target) <= exactCellLimit {
		chosen = planExact(vols, target)
	} else {
		chosen = planGreedy(vols, target)
	}
	for _, v := range chosen {
		picks = append(picks, v.index)
		covered = satAddInt(covered, v.blocks)
	}
	sort.Ints(picks)
	return picks, covered, true
}

// cost orders plans: fewer bytes first, then fewer volumes.
type cost struct {
	bytes int64
	count int
}

func (c cost) less(o cost) bool {
	if c.bytes != o.bytes {
		return c.bytes < o.bytes
	}
	return c.count < o.count
}

// planExact is the dynamic program behind PlanVolumes. best[j] is the
// cheapest cost to cover at least j blocks (j capped at target); take[i][j]
// records whether volume i improved best[j] so the plan can be rebuilt.
// target is reachable: the volumes' blocks sum to at least target.
func planExact(vols []volume, target int) []volume {
	const unreachable = math.MaxInt64
	best := make([]cost, target+1)
	for j := 1; j <= target; j++ {
		best[j] = cost{bytes: unreachable}
	}
	take := make([][]bool, len(vols))
	for i, v := range vols {
		take[i] = make([]bool, target+1)
		// Descending j keeps this a 0/1 knapsack: best[from] is still the
		// state before volume i.
		for j := target; j >= 1; j-- {
			from := max(j-v.blocks, 0)
			if best[from].bytes == unreachable {
				continue
			}
			cand := cost{bytes: satAdd64(best[from].bytes, v.bytes), count: best[from].count + 1}
			if cand.less(best[j]) {
				best[j] = cand
				take[i][j] = true
			}
		}
	}
	var chosen []volume
	j := target
	for i := len(vols) - 1; i >= 0 && j > 0; i-- {
		if take[i][j] {
			chosen = append(chosen, vols[i])
			j = max(j-vols[i].blocks, 0)
		}
	}
	return chosen
}

// planGreedy is the fallback of PlanVolumes for inputs too large for
// planExact.
func planGreedy(vols []volume, target int) []volume {
	order := slicesClone(vols)
	// Ascending bytes per block, compared by cross multiplication in float64
	// (exactness does not matter for a heuristic).
	sort.SliceStable(order, func(a, b int) bool {
		return float64(order[a].bytes)/float64(order[a].blocks) < float64(order[b].bytes)/float64(order[b].blocks)
	})
	var (
		chosen []volume
		sum    int
	)
	for _, v := range order {
		if sum >= target {
			break
		}
		chosen = append(chosen, v)
		sum = satAddInt(sum, v.blocks)
	}
	// Drop volumes that are redundant, most expensive first.
	sort.SliceStable(chosen, func(a, b int) bool { return chosen[a].bytes > chosen[b].bytes })
	kept := chosen[:0:0]
	for i, v := range chosen {
		if sum-v.blocks >= target {
			sum -= v.blocks
			continue
		}
		kept = append(kept, chosen[i])
	}
	return kept
}

func slicesClone(v []volume) []volume { return append([]volume(nil), v...) }

// DeferredController is the fetch side's half of the deferred volumes
// contract (M3-07). The real implementation lives with the fetch
// scheduler.
type DeferredController interface {
	// ListDeferred returns the files of the Download that are still
	// deferred (not fetched). The indexes Undefer takes are indexes into
	// this list.
	ListDeferred(ctx context.Context, id core.ID) ([]nzb.File, error)
	// Undefer makes the scheduler fetch the files at fileIndexes (indexes
	// into the list ListDeferred returned).
	Undefer(ctx context.Context, id core.ID, fileIndexes []int) error
}

// DeferredVolumes implements core.DeferredVolumes with a controller and
// PlanVolumes.
type DeferredVolumes struct {
	// Controller lists and un-defers the volumes. Required.
	Controller DeferredController
	// Requeue moves the Download from PostProcessing back to Fetching (the
	// port makes this the adapter's job). It runs after Undefer so the
	// scheduler already knows the new work when the state changes. Required.
	Requeue func(ctx context.Context, id core.ID) error
	// Margin is the number of blocks fetched beyond the minimum. Zero means
	// DefaultMargin; a negative value means none.
	Margin int
}

var _ core.DeferredVolumes = (*DeferredVolumes)(nil)

// FetchDeferred implements core.DeferredVolumes. It returns (false, nil) and
// changes nothing when all deferred volumes together cannot cover minBlocks.
// Otherwise it un-defers the cheapest sufficient volumes, then requeues the
// Download, and returns (true, nil). If Undefer succeeds and Requeue fails
// the error is returned: the volumes stay un-deferred, which a retry of the
// stage finds already done.
func (v *DeferredVolumes) FetchDeferred(ctx context.Context, d core.Download, minBlocks int) (bool, error) {
	if v.Controller == nil || v.Requeue == nil {
		return false, errors.New("adapt: DeferredVolumes needs a Controller and a Requeue")
	}
	files, err := v.Controller.ListDeferred(ctx, d.ID)
	if err != nil {
		return false, fmt.Errorf("list deferred volumes: %w", err)
	}
	margin := v.Margin
	if margin == 0 {
		margin = DefaultMargin
	}
	picks, _, ok := PlanVolumes(files, minBlocks, margin)
	if !ok {
		return false, nil
	}
	if len(picks) == 0 {
		// Nothing is needed (minBlocks <= 0) or nothing could be chosen:
		// there is no reason to leave PostProcessing.
		return false, nil
	}
	if err := v.Controller.Undefer(ctx, d.ID, picks); err != nil {
		return false, fmt.Errorf("undefer recovery volumes: %w", err)
	}
	if err := v.Requeue(ctx, d.ID); err != nil {
		return false, fmt.Errorf("requeue download for fetching: %w", err)
	}
	return true, nil
}
