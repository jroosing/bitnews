package runner

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/usenet/nzb"
)

// DefaultVolumeMargin is the number of recovery blocks fetched beyond what
// the damage needs, in case one of the fetched volumes turns out damaged.
const DefaultVolumeMargin = 1

// exactCellLimit bounds the size of the exact planner's table (volumes times
// target blocks). Beyond it planVolumes falls back to the greedy planner.
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
		if !f.Deferred() || f.RecoveryBlocks <= 0 {
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

// planVolumes picks the recovery volumes to fetch (indexes into files) so
// that their blocks cover needed+margin, at the lowest cost in bytes. Only
// Deferred files with RecoveryBlocks > 0 are candidates. A
// negative margin counts as 0.
//
// The result picks is sorted by index; covered is the sum of the picks'
// blocks. ok is false when all volumes together cannot cover needed (the
// margin is best effort: when the volumes cover needed but not needed+margin,
// all of them are picked). When needed is not positive nothing has to be
// fetched and planVolumes returns no picks, covered 0 and ok true.
//
// Algorithm. Choosing the cheapest set of volumes with at least T blocks is
// a covering knapsack. planVolumes solves it exactly by dynamic programming
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
func planVolumes(files []nzb.File, needed, margin int) (picks []int, covered int, ok bool) {
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

// planExact is the dynamic program behind planVolumes. best[j] is the
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

// planGreedy is the fallback of planVolumes for inputs too large for
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

// deferredFetcher is the decision half of FetchDeferred: list what is still
// deferred, plan, un-defer, requeue. Its steps are functions so it can be
// tested without a runner.
type deferredFetcher struct {
	list    func(ctx context.Context, id core.ID) ([]nzb.File, error)
	undefer func(ctx context.Context, id core.ID, fileIndexes []int) error
	requeue func(ctx context.Context, id core.ID) error
	// margin is the number of blocks fetched beyond the minimum. Zero means
	// DefaultVolumeMargin; a negative value means none.
	margin int
}

// fetch returns (false, nil) and changes nothing when all deferred volumes
// together cannot cover minBlocks. Otherwise it un-defers the cheapest
// sufficient volumes, then requeues the Download, and returns (true, nil).
// If undefer succeeds and requeue fails the error is returned: the volumes
// stay un-deferred, which a retry of the stage finds already done.
func (f deferredFetcher) fetch(ctx context.Context, d core.Download, minBlocks int) (bool, error) {
	files, err := f.list(ctx, d.ID)
	if err != nil {
		return false, fmt.Errorf("list deferred volumes: %w", err)
	}
	margin := f.margin
	if margin == 0 {
		margin = DefaultVolumeMargin
	}
	picks, _, ok := planVolumes(files, minBlocks, margin)
	if !ok || len(picks) == 0 {
		// Not enough, or nothing is needed (minBlocks <= 0): there is no
		// reason to leave PostProcessing.
		return false, nil
	}
	if err := f.undefer(ctx, d.ID, picks); err != nil {
		return false, fmt.Errorf("undefer recovery volumes: %w", err)
	}
	if err := f.requeue(ctx, d.ID); err != nil {
		return false, fmt.Errorf("requeue download for fetching: %w", err)
	}
	return true, nil
}
