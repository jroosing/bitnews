package runner_test

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
	"github.com/jroosing/bitnews/internal/usenet/runner"
)

// fakeVolumes is a runner.VolumeStore that outlives a Runner.
type fakeVolumes struct {
	mu   sync.Mutex
	rows map[core.ID]map[int]bool
}

func newFakeVolumes() *fakeVolumes { return &fakeVolumes{rows: map[core.ID]map[int]bool{}} }

func (f *fakeVolumes) Undeferred(_ context.Context, id core.ID) (map[int]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]bool{}
	maps.Copy(out, f.rows[id])
	return out, nil
}

func (f *fakeVolumes) Want(_ context.Context, id core.ID, idx []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[id] == nil {
		f.rows[id] = map[int]bool{}
	}
	for _, i := range idx {
		if _, ok := f.rows[id][i]; !ok {
			f.rows[id][i] = false
		}
	}
	return nil
}

func (f *fakeVolumes) MarkFetched(_ context.Context, id core.ID, idx []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range idx {
		if _, ok := f.rows[id][i]; ok {
			f.rows[id][i] = true
		}
	}
	return nil
}

var _ runner.VolumeStore = (*fakeVolumes)(nil)

// volumeFixture is a release with an index and two recovery volumes. The NZB
// lists its files by name: 0 movie.mkv, 1 movie.par2, 2 movie.vol000+02.par2,
// 3 movie.vol002+04.par2.
func volumeFixture(t *testing.T) (map[string][]byte, nntptest.Fixture) {
	t.Helper()
	files := map[string][]byte{
		"movie.mkv":            payload(40_000, 1),
		"movie.par2":           payload(3_000, 2),
		"movie.vol000+02.par2": payload(20_000, 3),
		"movie.vol002+04.par2": payload(30_000, 4),
	}
	return files, nntptest.BuildFixture(t, files, 10_000)
}

func (h *harness) idsOf(fx nntptest.Fixture, name string) []string {
	h.t.Helper()
	for _, f := range fx.Files {
		if f.Name == name {
			return f.MessageIDs
		}
	}
	h.t.Fatalf("no file %s", name)
	return nil
}

// requeueForVolumes plays the adapter: PostProcessing to Fetching.
func (h *harness) requeueForVolumes(id core.ID) {
	h.t.Helper()
	d, err := h.repo.UpdateState(context.Background(), id, core.StateChange{To: core.StateFetching, At: time.Now()})
	require.NoError(h.t, err)
	h.bus.Publish(core.DownloadUpdated{Download: d})
}

func (h *harness) toPostProcessing(id core.ID) {
	h.t.Helper()
	_, err := h.repo.UpdateState(context.Background(), id, core.StateChange{To: core.StatePostProcessing, At: time.Now()})
	require.NoError(h.t, err)
}

func TestUndeferFetchesOnlyTheChosenVolumesOnceAndReturnsToFetched(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	files, fx := volumeFixture(t)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	r := h.start()
	h.waitState(d.ID, core.StateFetched)
	for _, id := range append(h.idsOf(fx, "movie.vol000+02.par2"), h.idsOf(fx, "movie.vol002+04.par2")...) {
		require.Zero(t, srv.Requests(id), "volumes are not requested before Undefer")
	}
	ctx := context.Background()
	h.toPostProcessing(d.ID)

	deferred, err := r.ListDeferred(ctx, d.ID)
	require.NoError(t, err)
	require.Len(t, deferred, 2)
	assert.Equal(t, 2, deferred[0].RecoveryBlocks)
	assert.Equal(t, 4, deferred[1].RecoveryBlocks)
	assert.Equal(t, 6, r.DeferredBlocks(d.ID))

	require.NoError(t, r.Undefer(ctx, d.ID, []int{1})) // movie.vol002+04.par2
	assert.Equal(t, 6, r.DeferredBlocks(d.ID), "a volume counts as deferred until it is fetched")
	h.requeueForVolumes(d.ID)
	got := h.waitState(d.ID, core.StateFetched)

	assert.False(t, got.FetchedAt.IsZero())
	for _, id := range h.idsOf(fx, "movie.vol002+04.par2") {
		assert.Equal(t, 1, srv.Requests(id), "the chosen volume is fetched exactly once")
	}
	for _, name := range []string{"movie.mkv", "movie.par2"} {
		for _, id := range h.idsOf(fx, name) {
			assert.Equal(t, 1, srv.Requests(id), "%s is not fetched again", name)
		}
	}
	for _, id := range h.idsOf(fx, "movie.vol000+02.par2") {
		assert.Zero(t, srv.Requests(id), "the other volume stays deferred")
	}
	assert.Equal(t, map[string][]byte{
		"movie.mkv": files["movie.mkv"], "movie.par2": files["movie.par2"], "movie.vol002+04.par2": files["movie.vol002+04.par2"],
	}, readDir(t, h.dirFor(d)))
	deferred, err = r.ListDeferred(ctx, d.ID)
	require.NoError(t, err)
	require.Len(t, deferred, 1)
	assert.Equal(t, 2, deferred[0].RecoveryBlocks)
	assert.Equal(t, 2, r.DeferredBlocks(d.ID))
}

func TestUndeferSecondRoundFetchesOnlyTheNewVolume(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	files, fx := volumeFixture(t)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	r := h.start()
	h.waitState(d.ID, core.StateFetched)
	ctx := context.Background()
	h.toPostProcessing(d.ID)
	require.NoError(t, r.Undefer(ctx, d.ID, []int{1}))
	h.requeueForVolumes(d.ID)
	h.waitState(d.ID, core.StateFetched)
	h.toPostProcessing(d.ID)

	deferred, err := r.ListDeferred(ctx, d.ID)
	require.NoError(t, err)
	require.Len(t, deferred, 1)
	require.NoError(t, r.Undefer(ctx, d.ID, []int{0}))
	h.requeueForVolumes(d.ID)
	h.waitState(d.ID, core.StateFetched)

	for _, f := range fx.Files {
		for _, id := range f.MessageIDs {
			assert.Equal(t, 1, srv.Requests(id), "%s: every Article exactly once", f.Name)
		}
	}
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
	assert.Zero(t, r.DeferredBlocks(d.ID))
}

func TestUndeferIsRefusedOutsidePostProcessing(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	_, fx := volumeFixture(t)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	r := h.start()
	h.waitState(d.ID, core.StateFetched)

	err := r.Undefer(context.Background(), d.ID, []int{0})

	assert.ErrorIs(t, err, core.ErrIllegalTransition)
}

func TestUndeferRejectsAnIndexOutsideTheDeferredList(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	_, fx := volumeFixture(t)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	r := h.start()
	h.waitState(d.ID, core.StateFetched)
	h.toPostProcessing(d.ID)

	assert.Error(t, r.Undefer(context.Background(), d.ID, []int{2}))
	assert.Error(t, r.Undefer(context.Background(), d.ID, []int{-1}))
	assert.Equal(t, 6, r.DeferredBlocks(d.ID), "nothing was recorded")
}

func TestListDeferredOfAnUnknownDownloadIsAnError(t *testing.T) {
	h := newHarness(t)
	r := h.start()

	_, err := r.ListDeferred(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.Zero(t, r.DeferredBlocks(core.NewID()))
}

func TestUndeferredVolumesSurviveARestartOfTheRunner(t *testing.T) {
	h := newHarness(t)
	vols := newFakeVolumes()
	h.cfg.Volumes = vols
	srv := h.addServer("main", 0, 2)
	files, fx := volumeFixture(t)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	r := h.start()
	h.waitState(d.ID, core.StateFetched)
	h.toPostProcessing(d.ID)
	require.NoError(t, r.Undefer(context.Background(), d.ID, []int{1}))
	h.stop()
	// The daemon died after the requeue and before the volume was fetched.
	_, err := h.repo.UpdateState(context.Background(), d.ID, core.StateChange{To: core.StateFetching, At: time.Now()})
	require.NoError(t, err)

	r = h.start()
	require.NoError(t, r.Queue().Recover(context.Background()))
	h.waitState(d.ID, core.StateFetched)

	for _, id := range h.idsOf(fx, "movie.mkv") {
		assert.Equal(t, 1, srv.Requests(id), "the data file is not fetched again")
	}
	for _, id := range h.idsOf(fx, "movie.vol002+04.par2") {
		assert.Equal(t, 1, srv.Requests(id))
	}
	assert.Equal(t, files["movie.mkv"], readDir(t, h.dirFor(d))["movie.mkv"], "the data file is untouched")
	assert.Equal(t, 2, r.DeferredBlocks(d.ID))
}

func TestSupplementRunKeepsTheStoredFetchReport(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	_, fx := volumeFixture(t)
	fx.Install(srv)
	mkv := h.idsOf(fx, "movie.mkv")
	srv.Missing(mkv[0])
	d := h.addDownload("movie", fx)
	r := h.start()
	h.waitState(d.ID, core.StateFetched)
	before, err := r.FetchReport(context.Background(), d)
	require.NoError(t, err)
	require.Equal(t, 1, before.MissingArticles)
	h.toPostProcessing(d.ID)
	require.NoError(t, r.Undefer(context.Background(), d.ID, []int{0}))
	h.requeueForVolumes(d.ID)
	h.waitState(d.ID, core.StateFetched)

	after, err := r.FetchReport(context.Background(), d)

	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// freezable wraps a ProgressStore so a test can play a crash between two
// periodic saves: while frozen, saves and deletes vanish.
type freezable struct {
	inner  runner.ProgressStore
	mu     sync.Mutex
	frozen bool
	saves  int
}

func (f *freezable) Load(ctx context.Context, id core.ID) ([]byte, error) {
	return f.inner.Load(ctx, id)
}

func (f *freezable) Save(ctx context.Context, id core.ID, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen {
		return nil
	}
	f.saves++
	return f.inner.Save(ctx, id, b)
}

func (f *freezable) Delete(ctx context.Context, id core.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen {
		return nil
	}
	return f.inner.Delete(ctx, id)
}

func (f *freezable) setFrozen(v bool) {
	f.mu.Lock()
	f.frozen = v
	f.mu.Unlock()
}

func (f *freezable) saveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saves
}

func TestResumeWithTheSQLiteStoreRefetchesOnlyWhatWasNotSavedAfterACrash(t *testing.T) {
	h := newHarness(t)
	progress := &freezable{inner: store.NewUsenetProgressRepo(h.db)}
	h.cfg.Progress = progress
	h.cfg.SaveInterval = 30 * time.Millisecond
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(3 * time.Millisecond)
	files := map[string][]byte{"movie.mkv": payload(1_000_000, 1), "movie.nfo": payload(20_000, 2)}
	fx := nntptest.BuildFixture(t, files, 10_000) // 102 articles
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	h.start()
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 40 && progress.saveCount() >= 2 }, 15*time.Second, time.Millisecond)
	progress.setFrozen(true) // the crash: nothing after this point reaches the database
	h.stop()
	progress.setFrozen(false)
	firstRun := srv.TotalRequests()
	require.Less(t, firstRun, 102)
	require.Equal(t, core.StateFetching, h.get(d.ID).State, "the crash leaves it Fetching")
	_, err := store.NewUsenetProgressRepo(h.db).Load(context.Background(), d.ID)
	require.NoError(t, err, "a periodic save reached the database")

	r := h.start()
	require.NoError(t, r.Queue().Recover(context.Background()))
	h.waitState(d.ID, core.StateFetched)

	assert.Equal(t, files, readDir(t, h.dirFor(d)))
	refetched := 0
	for _, id := range fx.MessageIDs() {
		if srv.Requests(id) > 1 {
			refetched++
		}
	}
	assert.Less(t, refetched, firstRun-10, "most Articles of the first run were skipped")
	_, err = store.NewUsenetProgressRepo(h.db).Load(context.Background(), d.ID)
	assert.ErrorIs(t, err, core.ErrNotFound, "the blob is deleted once Fetched")
}
