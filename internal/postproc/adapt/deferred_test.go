package adapt

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nzb"
)

// vol builds a par2 volume file of the given block count and byte size.
func vol(blocks int, bytes int64) nzb.File {
	return nzb.File{
		Kind:           nzb.KindPar2Volume,
		RecoveryBlocks: blocks,
		Segments:       []nzb.Segment{{Number: 1, Bytes: bytes}},
	}
}

func TestPlanVolumes_ExactCover(t *testing.T) {
	files := []nzb.File{vol(1, 100), vol(2, 200), vol(4, 400)}
	picks, covered, ok := PlanVolumes(files, 4, 0)
	assert.True(t, ok)
	assert.Equal(t, []int{2}, picks)
	assert.Equal(t, 4, covered)
}

func TestPlanVolumes_Table(t *testing.T) {
	tests := []struct {
		name        string
		files       []nzb.File
		needed      int
		margin      int
		wantPicks   []int
		wantCovered int
		wantOK      bool
	}{
		{
			name:   "prefers fewer bytes over fewer volumes",
			files:  []nzb.File{vol(4, 1000), vol(2, 100), vol(2, 100)},
			needed: 4, wantPicks: []int{1, 2}, wantCovered: 4, wantOK: true,
		},
		{
			name:   "prefers fewer bytes over fewer blocks",
			files:  []nzb.File{vol(8, 900), vol(3, 100), vol(3, 100)},
			needed: 5, wantPicks: []int{1, 2}, wantCovered: 6, wantOK: true,
		},
		{
			name:   "margin adds blocks",
			files:  []nzb.File{vol(1, 10), vol(1, 10), vol(1, 10)},
			needed: 1, margin: 1, wantPicks: []int{0, 1}, wantCovered: 2, wantOK: true,
		},
		{
			name:   "margin is best effort when volumes cover needed only",
			files:  []nzb.File{vol(2, 10), vol(1, 10)},
			needed: 3, margin: 1, wantPicks: []int{0, 1}, wantCovered: 3, wantOK: true,
		},
		{
			name:   "negative margin counts as zero",
			files:  []nzb.File{vol(1, 10), vol(1, 10)},
			needed: 1, margin: -5, wantPicks: []int{0}, wantCovered: 1, wantOK: true,
		},
		{
			name:   "impossible",
			files:  []nzb.File{vol(1, 10), vol(2, 10)},
			needed: 4, margin: 1, wantOK: false,
		},
		{
			name:   "single volume",
			files:  []nzb.File{vol(5, 10)},
			needed: 3, margin: 1, wantPicks: []int{0}, wantCovered: 5, wantOK: true,
		},
		{
			name:   "zero needed fetches nothing",
			files:  []nzb.File{vol(5, 10)},
			needed: 0, margin: 1, wantOK: true,
		},
		{
			name:   "no volumes",
			files:  nil,
			needed: 1, wantOK: false,
		},
		{
			name:   "no volumes and nothing needed",
			files:  nil,
			needed: 0, wantOK: true,
		},
		{
			name: "other kinds and empty volumes are ignored",
			files: []nzb.File{
				{Kind: nzb.KindData, RecoveryBlocks: 9, Segments: []nzb.Segment{{Bytes: 1}}},
				{Kind: nzb.KindPar2Index, Segments: []nzb.Segment{{Bytes: 1}}},
				vol(0, 1),
				vol(2, 50),
			},
			needed: 2, wantPicks: []int{3}, wantCovered: 2, wantOK: true,
		},
		{
			name:   "ties prefer fewer volumes",
			files:  []nzb.File{vol(1, 50), vol(1, 50), vol(2, 100)},
			needed: 2, wantPicks: []int{2}, wantCovered: 2, wantOK: true,
		},
		{
			name:   "segment bytes are summed",
			files:  []nzb.File{multiSeg(2, 100, 100), vol(2, 150)},
			needed: 2, wantPicks: []int{1}, wantCovered: 2, wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			picks, covered, ok := PlanVolumes(tt.files, tt.needed, tt.margin)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantPicks, picks)
			assert.Equal(t, tt.wantCovered, covered)
		})
	}
}

// multiSeg builds a volume whose segments have the given sizes.
func multiSeg(blocks int, sizes ...int64) nzb.File {
	f := nzb.File{Kind: nzb.KindPar2Volume, RecoveryBlocks: blocks}
	for i, s := range sizes {
		f.Segments = append(f.Segments, nzb.Segment{Number: i + 1, Bytes: s})
	}
	return f
}

func TestDeferredBytes_SumsVolumesOnly(t *testing.T) {
	files := []nzb.File{
		vol(1, 10),
		multiSeg(2, 5, 7),
		{Kind: nzb.KindData, Segments: []nzb.Segment{{Bytes: 1000}}},
		{Kind: nzb.KindPar2Index, Segments: []nzb.Segment{{Bytes: 1000}}},
	}
	assert.Equal(t, int64(22), DeferredBytes(files))
	assert.Equal(t, int64(0), DeferredBytes(nil))
}

func TestDeferredBytes_Saturates(t *testing.T) {
	files := []nzb.File{vol(1, math.MaxInt64), vol(1, 5)}
	assert.Equal(t, int64(math.MaxInt64), DeferredBytes(files))
}

// randomVolumes builds n volumes with small random sizes.
func randomVolumes(r *rand.Rand, n int) []nzb.File {
	files := make([]nzb.File, n)
	for i := range files {
		files[i] = vol(1+r.IntN(6), int64(1+r.IntN(30)))
	}
	return files
}

// bruteForce returns the cost (bytes, count) of the cheapest subset with at
// least target blocks, or false when none exists.
func bruteForce(files []nzb.File, target int) (best cost, found bool) {
	for mask := 0; mask < 1<<len(files); mask++ {
		var c cost
		blocks := 0
		for i, f := range files {
			if mask&(1<<i) != 0 {
				blocks += f.RecoveryBlocks
				c.bytes += fileBytes(f)
				c.count++
			}
		}
		if blocks >= target && (!found || c.less(best)) {
			best, found = c, true
		}
	}
	return best, found
}

func costOf(files []nzb.File, picks []int) cost {
	var c cost
	for _, i := range picks {
		c.bytes += fileBytes(files[i])
		c.count++
	}
	return c
}

func TestPlanVolumes_MatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		files := randomVolumes(r, r.IntN(11))
		needed := r.IntN(30)
		margin := r.IntN(3)
		total := 0
		for _, f := range files {
			total += f.RecoveryBlocks
		}

		picks, covered, ok := PlanVolumes(files, needed, margin)
		if needed > 0 && total < needed {
			require.False(t, ok)
			continue
		}
		require.True(t, ok)
		if needed == 0 {
			require.Empty(t, picks)
			continue
		}
		target := min(needed+margin, total)
		want, found := bruteForce(files, target)
		require.True(t, found)
		require.Equal(t, want, costOf(files, picks), "files=%v needed=%d margin=%d", files, needed, margin)
		sum := 0
		for _, i := range picks {
			sum += files[i].RecoveryBlocks
		}
		require.Equal(t, sum, covered)
		require.GreaterOrEqual(t, covered, target)
		require.True(t, slices.IsSorted(picks))
	}
}

func TestPlanGreedy_IsValidAndNotCheaperThanOptimal(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 500 {
		files := randomVolumes(r, 1+r.IntN(10))
		total := 0
		for _, f := range files {
			total += f.RecoveryBlocks
		}
		target := 1 + r.IntN(total)

		chosen := planGreedy(volumesOf(files), target)
		var c cost
		sum := 0
		seen := map[int]bool{}
		for _, v := range chosen {
			require.False(t, seen[v.index], "volume picked twice")
			seen[v.index] = true
			sum += v.blocks
			c.bytes += v.bytes
			c.count++
		}
		require.GreaterOrEqual(t, sum, target, "greedy must cover the target")
		// No redundant volume remains.
		for _, v := range chosen {
			require.Less(t, sum-v.blocks, target, "volume %d is redundant", v.index)
		}
		opt, found := bruteForce(files, target)
		require.True(t, found)
		require.False(t, c.less(opt), "greedy cannot beat the optimum")
	}
}

func TestPlanVolumes_HugeInputFallsBackToGreedy(t *testing.T) {
	// 2 volumes x a million blocks exceeds the exact planner's table.
	files := []nzb.File{vol(2_000_000, 100), vol(2_000_000, 50), vol(10, 5)}
	picks, covered, ok := PlanVolumes(files, 1_500_000, 1)
	require.True(t, ok)
	assert.Equal(t, []int{1}, picks)
	assert.Equal(t, 2_000_000, covered)

	_, _, ok = PlanVolumes(files, 5_000_000, 1)
	assert.False(t, ok)
}

func TestPlanVolumes_NeverOverflows(t *testing.T) {
	files := []nzb.File{vol(math.MaxInt, 1), vol(math.MaxInt, 1)}
	picks, _, ok := PlanVolumes(files, math.MaxInt, math.MaxInt)
	require.True(t, ok)
	assert.Len(t, picks, 1)
}

// fakeController records the order of controller calls.
type fakeController struct {
	log        *[]string
	files      []nzb.File
	listErr    error
	undeferErr error
	undeferred []int
}

func (f *fakeController) ListDeferred(_ context.Context, id core.ID) ([]nzb.File, error) {
	*f.log = append(*f.log, "list:"+string(id))
	return f.files, f.listErr
}

func (f *fakeController) Undefer(_ context.Context, id core.ID, idx []int) error {
	*f.log = append(*f.log, "undefer:"+string(id))
	f.undeferred = idx
	return f.undeferErr
}

func newDeferred(files []nzb.File) (*DeferredVolumes, *fakeController, *[]string) {
	log := &[]string{}
	ctl := &fakeController{log: log, files: files}
	dv := &DeferredVolumes{
		Controller: ctl,
		Requeue: func(_ context.Context, id core.ID) error {
			*log = append(*log, "requeue:"+string(id))
			return nil
		},
	}
	return dv, ctl, log
}

func TestDeferredVolumes_UndefersThenRequeues(t *testing.T) {
	dv, ctl, log := newDeferred([]nzb.File{vol(4, 400), vol(2, 100), vol(2, 100)})
	more, err := dv.FetchDeferred(context.Background(), core.Download{ID: "d1"}, 3)
	require.NoError(t, err)
	assert.True(t, more)
	// 3 blocks + the default margin of 1 = 4, cheapest as the two small ones.
	assert.Equal(t, []int{1, 2}, ctl.undeferred)
	assert.Equal(t, []string{"list:d1", "undefer:d1", "requeue:d1"}, *log)
}

func TestDeferredVolumes_ExplicitAndNoMargin(t *testing.T) {
	dv, ctl, _ := newDeferred([]nzb.File{vol(1, 10), vol(1, 10), vol(1, 10)})
	dv.Margin = -1
	more, err := dv.FetchDeferred(context.Background(), core.Download{ID: "d"}, 1)
	require.NoError(t, err)
	assert.True(t, more)
	assert.Equal(t, []int{0}, ctl.undeferred)

	dv, ctl, _ = newDeferred([]nzb.File{vol(1, 10), vol(1, 10), vol(1, 10)})
	dv.Margin = 2
	_, err = dv.FetchDeferred(context.Background(), core.Download{ID: "d"}, 1)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2}, ctl.undeferred)
}

func TestDeferredVolumes_CannotCoverChangesNothing(t *testing.T) {
	dv, ctl, log := newDeferred([]nzb.File{vol(1, 10), vol(2, 10)})
	more, err := dv.FetchDeferred(context.Background(), core.Download{ID: "d1"}, 4)
	require.NoError(t, err)
	assert.False(t, more)
	assert.Nil(t, ctl.undeferred)
	assert.Equal(t, []string{"list:d1"}, *log, "no undefer, no requeue")
}

func TestDeferredVolumes_ErrorsPropagate(t *testing.T) {
	boom := errors.New("boom")
	t.Run("list", func(t *testing.T) {
		dv, ctl, log := newDeferred([]nzb.File{vol(2, 10)})
		ctl.listErr = boom
		more, err := dv.FetchDeferred(context.Background(), core.Download{ID: "d"}, 1)
		assert.ErrorIs(t, err, boom)
		assert.False(t, more)
		assert.Equal(t, []string{"list:d"}, *log)
	})
	t.Run("undefer stops before requeue", func(t *testing.T) {
		dv, ctl, log := newDeferred([]nzb.File{vol(2, 10)})
		ctl.undeferErr = boom
		more, err := dv.FetchDeferred(context.Background(), core.Download{ID: "d"}, 1)
		assert.ErrorIs(t, err, boom)
		assert.False(t, more)
		assert.Equal(t, []string{"list:d", "undefer:d"}, *log)
	})
	t.Run("requeue failure is returned", func(t *testing.T) {
		dv, _, log := newDeferred([]nzb.File{vol(2, 10)})
		dv.Requeue = func(context.Context, core.ID) error {
			*log = append(*log, "requeue")
			return boom
		}
		more, err := dv.FetchDeferred(context.Background(), core.Download{ID: "d"}, 1)
		assert.ErrorIs(t, err, boom)
		assert.False(t, more)
		assert.Equal(t, []string{"list:d", "undefer:d", "requeue"}, *log)
	})
	t.Run("misconfigured", func(t *testing.T) {
		_, err := (&DeferredVolumes{}).FetchDeferred(context.Background(), core.Download{ID: "d"}, 1)
		assert.Error(t, err)
	})
}

func TestDeferredVolumes_AssertsPort(t *testing.T) {
	var _ core.DeferredVolumes = (*DeferredVolumes)(nil)
}
