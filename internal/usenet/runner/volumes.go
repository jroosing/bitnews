package runner

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/usenet/nzb"
)

// VolumeStore persists which PAR2 recovery volumes of a Download were
// un-deferred (M3-07), so the choice survives a restart. Volumes are
// identified by their index in NZB.Files.
type VolumeStore interface {
	// Undeferred returns the un-deferred volumes: NZB file index to
	// whether the volume was fetched. A Download without any is not an
	// error.
	Undeferred(ctx context.Context, id core.ID) (map[int]bool, error)
	// Want records the volumes as un-deferred and not fetched yet. A
	// volume that is recorded already keeps its fetched flag.
	Want(ctx context.Context, id core.ID, fileIndexes []int) error
	// MarkFetched sets the fetched flag.
	MarkFetched(ctx context.Context, id core.ID, fileIndexes []int) error
}

// memVolumes is the VolumeStore used when Config.Volumes is nil.
type memVolumes struct {
	mu   sync.Mutex
	rows map[core.ID]map[int]bool
}

func newMemVolumes() *memVolumes { return &memVolumes{rows: map[core.ID]map[int]bool{}} }

func (m *memVolumes) Undeferred(_ context.Context, id core.ID) (map[int]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int]bool, len(m.rows[id]))
	maps.Copy(out, m.rows[id])
	return out, nil
}

func (m *memVolumes) Want(_ context.Context, id core.ID, idx []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows[id] == nil {
		m.rows[id] = map[int]bool{}
	}
	for _, i := range idx {
		if _, ok := m.rows[id][i]; !ok {
			m.rows[id][i] = false
		}
	}
	return nil
}

func (m *memVolumes) MarkFetched(_ context.Context, id core.ID, idx []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, i := range idx {
		if _, ok := m.rows[id][i]; ok {
			m.rows[id][i] = true
		}
	}
	return nil
}

// deferredFiles returns the NZB indexes of the Download's recovery volumes
// that are not fetched yet (deferred or un-deferred and pending), in file
// order. Volumes without Articles are not offered.
func (r *Runner) deferredFiles(ctx context.Context, id core.ID) (*nzb.NZB, []int, error) {
	n, err := r.loadNZB(ctx, core.Download{ID: id})
	if err != nil {
		return nil, nil, err
	}
	und, err := r.volumes.Undeferred(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("runner: read un-deferred volumes of %s: %w", id, err)
	}
	var idx []int
	for i, f := range n.Files {
		if f.Deferred() && len(f.Segments) > 0 && !und[i] {
			idx = append(idx, i)
		}
	}
	return n, idx, nil
}

// ListDeferred returns the recovery volumes (PAR2 files named
// .volNN+MM.par2) of the Download that have not been fetched, in NZB order.
// The indexes Undefer takes are indexes into this list. It parses the stored
// NZB. An unknown Download is an error wrapping core.ErrNotFound.
func (r *Runner) ListDeferred(ctx context.Context, id core.ID) ([]nzb.File, error) {
	if _, err := r.cfg.Downloads.Get(ctx, id); err != nil {
		return nil, fmt.Errorf("runner: list deferred volumes of %s: %w", id, err)
	}
	n, idx, err := r.deferredFiles(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("runner: list deferred volumes of %s: %w", id, err)
	}
	out := make([]nzb.File, len(idx))
	for i, fi := range idx {
		out[i] = n.Files[fi]
	}
	return out, nil
}

// Undefer records that the volumes at fileIndexes (indexes into the list
// ListDeferred returns) are wanted. The Download must be in
// StatePostProcessing; moving it back to StateFetching is the caller's job
// (see FetchDeferred). The next fetch of the Download then fetches
// only these volumes: its other Articles were resolved already. The choice
// is persisted through Config.Volumes. Undefer of a volume that is wanted
// already changes nothing.
func (r *Runner) Undefer(ctx context.Context, id core.ID, fileIndexes []int) error {
	d, err := r.cfg.Downloads.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("runner: undefer volumes of %s: %w", id, err)
	}
	if d.State != core.StatePostProcessing {
		return fmt.Errorf("runner: undefer volumes of a %s download: %w", d.State, core.ErrIllegalTransition)
	}
	_, idx, err := r.deferredFiles(ctx, id)
	if err != nil {
		return fmt.Errorf("runner: undefer volumes of %s: %w", id, err)
	}
	chosen := make([]int, 0, len(fileIndexes))
	for _, i := range fileIndexes {
		if i < 0 || i >= len(idx) {
			return fmt.Errorf("runner: undefer volumes of %s: index %d is outside the %d deferred volumes", id, i, len(idx))
		}
		chosen = append(chosen, idx[i])
	}
	slices.Sort(chosen)
	if err := r.volumes.Want(ctx, id, slices.Compact(chosen)); err != nil {
		return fmt.Errorf("runner: undefer volumes of %s: %w", id, err)
	}
	return nil
}

// DeferredBlocks returns how many recovery blocks the Download's volumes
// that are not fetched yet hold (the MM of their names). It is the supplier
// par2.Verifier.DeferredBlocks wants. Any error counts as none, and is
// logged.
func (r *Runner) DeferredBlocks(id core.ID) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, idx, err := r.deferredFiles(ctx, id)
	if err != nil {
		r.log.Debug("usenet runner: cannot count deferred blocks", "id", id, "err", err)
		return 0
	}
	total := 0
	for _, fi := range idx {
		total = min(total+max(n.Files[fi].RecoveryBlocks, 0), math.MaxInt32)
	}
	return total
}

var _ core.DeferredVolumes = (*Runner)(nil)

// FetchDeferred implements core.DeferredVolumes: when Repair needs more
// recovery blocks than are on disk, it un-defers the cheapest volumes that
// cover minBlocks plus Config.VolumeMargin and moves the Download back to
// StateFetching, so the next pass fetches just those volumes. It returns
// more=false, and changes nothing, when all deferred volumes together cannot
// cover minBlocks.
func (r *Runner) FetchDeferred(ctx context.Context, d core.Download, minBlocks int) (bool, error) {
	return deferredFetcher{
		list: r.ListDeferred, undefer: r.Undefer, requeue: r.refetch, margin: r.cfg.VolumeMargin,
	}.fetch(ctx, d, minBlocks)
}

// refetch moves a Download from PostProcessing back to Fetching and
// publishes the change.
func (r *Runner) refetch(ctx context.Context, id core.ID) error {
	_, err := r.q.SetState(ctx, id, core.StateChange{To: core.StateFetching, At: r.clock.Now()})
	return err
}
