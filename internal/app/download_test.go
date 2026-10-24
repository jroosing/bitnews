package app_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func (s *stack) downloadService(opts ...app.Option) *app.DownloadService {
	opts = append([]app.Option{app.WithClock(s.clock.Now)}, opts...)
	return app.NewDownloadService(s.downloads, s.cats, s.bus, opts...)
}

func TestPauseSetsPausedStoresItAndPublishesDownloadUpdated(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	d := s.addDownload(t, "movie", 0)
	s.clock.Advance(time.Minute)

	got, err := svc.Pause(ctx, d.ID)

	require.NoError(t, err)
	assert.True(t, got.Paused)
	assert.Equal(t, s.clock.Now(), got.UpdatedAt)
	stored, err := s.downloads.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, got, stored)
	assert.Equal(t, []core.Event{core.DownloadUpdated{Download: got}}, s.events())
}

func TestResumeClearsPausedAndPublishesDownloadUpdated(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	d := s.addDownload(t, "movie", 0)
	_, err := svc.Pause(ctx, d.ID)
	require.NoError(t, err)
	s.events()

	got, err := svc.Resume(ctx, d.ID)

	require.NoError(t, err)
	assert.False(t, got.Paused)
	assert.Equal(t, []core.Event{core.DownloadUpdated{Download: got}}, s.events())
}

func TestPauseAndResumeAreIdempotentAndPublishNothingWhenNothingChanges(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	d := s.addDownload(t, "movie", 0)

	_, err := svc.Resume(ctx, d.ID)
	require.NoError(t, err)
	_, err = svc.Pause(ctx, d.ID)
	require.NoError(t, err)
	s.events()
	_, err = svc.Pause(ctx, d.ID)
	require.NoError(t, err)

	assert.Empty(t, s.events())
}

func TestPauseOfDownloadThatWasAlreadyFetchedIsAConflict(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	d := s.addDownload(t, "movie", 0)
	_, err := s.downloads.UpdateState(ctx, d.ID, core.StateChange{To: core.StateFetching, At: epoch})
	require.NoError(t, err)
	_, err = s.downloads.UpdateState(ctx, d.ID, core.StateChange{To: core.StateFetched, At: epoch})
	require.NoError(t, err)

	_, err = svc.Pause(ctx, d.ID)

	require.ErrorIs(t, err, core.ErrConflict)
	var appErr *app.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "not_pausable", appErr.Code)
	assert.Empty(t, s.events())
}

func TestPauseOfUnknownDownloadIsNotFound(t *testing.T) {
	s := newStack(t)

	_, err := s.downloadService().Pause(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestListFiltersByStateCategoryAndKindAndPages(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, s.cats.Create(ctx, cat))
	a := s.addDownload(t, "a", 0)
	b := s.addDownload(t, "b", time.Second)
	c := s.addDownload(t, "c", 2*time.Second)
	b.CategoryID = cat.ID
	require.NoError(t, s.downloads.Update(ctx, b))
	_, err := s.downloads.UpdateState(ctx, c.ID, core.StateChange{To: core.StateFetching, At: epoch})
	require.NoError(t, err)

	byCategory, err := svc.List(ctx, app.ListFilter{CategoryID: cat.ID})
	require.NoError(t, err)
	byState, err := svc.List(ctx, app.ListFilter{State: core.StateFetching})
	require.NoError(t, err)
	byKind, err := svc.List(ctx, app.ListFilter{Kind: core.SourceTorrent})
	require.NoError(t, err)
	first, err := svc.List(ctx, app.ListFilter{Limit: 2})
	require.NoError(t, err)
	second, err := svc.List(ctx, app.ListFilter{Limit: 2, After: first.Next})
	require.NoError(t, err)

	assert.Equal(t, []core.ID{b.ID}, ids(byCategory.Items))
	assert.Equal(t, []core.ID{c.ID}, ids(byState.Items))
	assert.Empty(t, byKind.Items)
	assert.NotNil(t, byKind.Items)
	assert.Equal(t, []core.ID{a.ID, b.ID}, ids(first.Items))
	assert.NotEmpty(t, first.Next)
	assert.Equal(t, []core.ID{c.ID}, ids(second.Items))
	assert.Empty(t, second.Next)
}

func ids(ds []core.Download) []core.ID {
	out := make([]core.ID, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func TestListRejectsUnknownFilterValuesAndBadCursorAsInvalidInput(t *testing.T) {
	s := newStack(t)
	svc := s.downloadService()
	for name, tc := range map[string]struct {
		filter app.ListFilter
		code   string
	}{
		"state":  {app.ListFilter{State: "bogus"}, "invalid_state"},
		"kind":   {app.ListFilter{Kind: "bogus"}, "invalid_kind"},
		"limit":  {app.ListFilter{Limit: -1}, "invalid_limit"},
		"cursor": {app.ListFilter{After: "!!!"}, "invalid_cursor"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.List(context.Background(), tc.filter)

			require.ErrorIs(t, err, app.ErrInvalidInput)
			var appErr *app.Error
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, tc.code, appErr.Code)
		})
	}
}

func TestGetReturnsDownloadOrNotFound(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	d := s.addDownload(t, "movie", 0)

	got, err := svc.Get(ctx, d.ID)
	require.NoError(t, err)
	_, missing := svc.Get(ctx, core.NewID())

	assert.Equal(t, d, got)
	assert.ErrorIs(t, missing, core.ErrNotFound)
}

func TestUpdatePriorityIsStoredAndPublishesDownloadUpdated(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	d := s.addDownload(t, "movie", 0)
	high := core.DownloadPriorityHigh

	got, err := svc.Update(ctx, d.ID, app.Patch{Priority: &high})

	require.NoError(t, err)
	assert.Equal(t, core.DownloadPriorityHigh, got.Priority)
	stored, err := s.downloads.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, got, stored)
	assert.Equal(t, []core.Event{core.DownloadUpdated{Download: got}}, s.events())
}

func TestUpdateRejectsUnknownPriorityAndEmptyPatchWithoutChangingAnything(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	d := s.addDownload(t, "movie", 0)
	bogus := core.DownloadPriority("urgent")

	_, errPriority := svc.Update(ctx, d.ID, app.Patch{Priority: &bogus})
	_, errEmpty := svc.Update(ctx, d.ID, app.Patch{})

	for err, code := range map[error]string{errPriority: "invalid_priority", errEmpty: "empty_patch"} {
		require.ErrorIs(t, err, app.ErrInvalidInput)
		var appErr *app.Error
		require.ErrorAs(t, err, &appErr)
		assert.Equal(t, code, appErr.Code)
	}
	stored, err := s.downloads.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, d, stored)
	assert.Empty(t, s.events())
}

func TestUpdateSetsAndClearsCategoryAndRejectsUnknownCategory(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, s.cats.Create(ctx, cat))
	d := s.addDownload(t, "movie", 0)

	set, err := svc.Update(ctx, d.ID, app.Patch{Category: app.SetCategory(cat.ID)})
	require.NoError(t, err)
	_, errUnknown := svc.Update(ctx, d.ID, app.Patch{Category: app.SetCategory(core.NewID())})
	cleared, err := svc.Update(ctx, d.ID, app.Patch{Category: app.SetCategory("")})
	require.NoError(t, err)

	assert.Equal(t, cat.ID, set.CategoryID)
	require.ErrorIs(t, errUnknown, app.ErrInvalidInput)
	var appErr *app.Error
	require.ErrorAs(t, errUnknown, &appErr)
	assert.Equal(t, "invalid_category", appErr.Code)
	assert.Empty(t, cleared.CategoryID)
	assert.Len(t, s.events(), 2)
}

func TestUpdateMoveReordersTheQueueAndPublishesEveryChangedDownload(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	a := s.addDownload(t, "a", 0)
	b := s.addDownload(t, "b", time.Second)
	c := s.addDownload(t, "c", 2*time.Second)

	got, err := svc.Update(ctx, c.ID, app.Patch{Move: app.MoveTop})

	require.NoError(t, err)
	assert.Equal(t, 0, got.QueuePosition)
	positions := map[core.ID]int{}
	for _, id := range []core.ID{a.ID, b.ID, c.ID} {
		d, err := s.downloads.Get(ctx, id)
		require.NoError(t, err)
		positions[id] = d.QueuePosition
	}
	assert.Equal(t, map[core.ID]int{c.ID: 0, a.ID: 1, b.ID: 2}, positions)
	var updated []core.ID
	for _, ev := range s.events() {
		updated = append(updated, ev.(core.DownloadUpdated).Download.ID)
	}
	assert.ElementsMatch(t, []core.ID{a.ID, b.ID}, updated)
}

func TestUpdateMoveOnlyRanksDownloadsOfTheSameQueue(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	nzb := s.addDownload(t, "nzb", 0)
	torrent := s.addDownload(t, "torrent", time.Second)
	// The store has no way to change a source kind, so recreate it.
	require.NoError(t, s.downloads.Delete(ctx, torrent.ID))
	torrent.SourceKind = core.SourceTorrent
	require.NoError(t, s.downloads.Create(ctx, torrent))

	got, err := svc.Update(ctx, torrent.ID, app.Patch{Move: app.MoveBottom})

	require.NoError(t, err)
	assert.Equal(t, 0, got.QueuePosition)
	stored, err := s.downloads.Get(ctx, nzb.ID)
	require.NoError(t, err)
	assert.Equal(t, nzb, stored)
	assert.Empty(t, s.events())
}

func TestUpdateMoveOfADownloadOutsideTheQueueIsAConflict(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.downloadService()
	d := s.addDownload(t, "movie", 0)
	for _, to := range []core.State{core.StateFetching, core.StateFetched} {
		_, err := s.downloads.UpdateState(ctx, d.ID, core.StateChange{To: to, At: epoch})
		require.NoError(t, err)
	}

	_, err := svc.Update(ctx, d.ID, app.Patch{Move: app.MoveUp})

	require.ErrorIs(t, err, core.ErrConflict)
	var appErr *app.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "not_queued", appErr.Code)
}

func TestUpdateRejectsUnknownMove(t *testing.T) {
	s := newStack(t)
	d := s.addDownload(t, "movie", 0)

	_, err := s.downloadService().Update(context.Background(), d.ID, app.Patch{Move: "sideways"})

	require.ErrorIs(t, err, app.ErrInvalidInput)
	var appErr *app.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "invalid_move", appErr.Code)
}

func TestUpdateOfUnknownDownloadIsNotFound(t *testing.T) {
	s := newStack(t)
	high := core.DownloadPriorityHigh

	_, err := s.downloadService().Update(context.Background(), core.NewID(), app.Patch{Priority: &high})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

// recordingRemover records what it was asked to hold and remove, and
// whether the row still existed at that moment.
type recordingRemover struct {
	repo     core.DownloadRepo
	held     []core.ID
	released []core.ID
	removed  []core.Download
	rowSeen  []bool
	err      error
	holdErr  error
	// onHold runs inside Hold; onRemove inside RemoveData.
	onHold, onRemove func()
}

func (r *recordingRemover) Hold(_ context.Context, d core.Download) (func(), error) {
	r.held = append(r.held, d.ID)
	if r.onHold != nil {
		r.onHold()
	}
	if r.holdErr != nil {
		return nil, r.holdErr
	}
	return func() { r.released = append(r.released, d.ID) }, nil
}

func (r *recordingRemover) RemoveData(ctx context.Context, d core.Download) error {
	_, getErr := r.repo.Get(ctx, d.ID)
	r.removed = append(r.removed, d)
	r.rowSeen = append(r.rowSeen, getErr == nil)
	if r.onRemove != nil {
		r.onRemove()
	}
	return r.err
}

// moveTo walks the stored Download through the given States.
func (s *stack) moveTo(t *testing.T, id core.ID, states ...core.State) {
	t.Helper()
	for _, st := range states {
		c := core.StateChange{To: st, At: epoch}
		_, err := s.downloads.UpdateState(context.Background(), id, c)
		require.NoError(t, err, "to %s", st)
	}
}

func TestDeleteRemovesRowThenPublishesRemovedWithoutTouchingData(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	remover := &recordingRemover{repo: s.downloads}
	svc := s.downloadService(app.WithDataRemover(remover))
	d := s.addDownload(t, "movie", 0)

	err := svc.Delete(ctx, d.ID, false)

	require.NoError(t, err)
	_, getErr := s.downloads.Get(ctx, d.ID)
	assert.ErrorIs(t, getErr, core.ErrNotFound)
	assert.Equal(t, []core.Event{core.Removed{ID: d.ID}}, s.events())
	assert.Empty(t, remover.removed)
}

func TestDeleteWithDataAsksTheRemoverBeforeDeletingTheRow(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	remover := &recordingRemover{repo: s.downloads}
	svc := s.downloadService(app.WithDataRemover(remover))
	d := s.addDownload(t, "movie", 0)

	err := svc.Delete(ctx, d.ID, true)

	require.NoError(t, err)
	assert.Equal(t, []core.Download{d}, remover.removed)
	assert.Equal(t, []bool{true}, remover.rowSeen)
	assert.Equal(t, []core.Event{core.Removed{ID: d.ID}}, s.events())
}

func TestDeleteKeepsRowAndPublishesNothingWhenDataRemovalFails(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	remover := &recordingRemover{repo: s.downloads, err: assert.AnError}
	svc := s.downloadService(app.WithDataRemover(remover))
	d := s.addDownload(t, "movie", 0)

	err := svc.Delete(ctx, d.ID, true)

	require.ErrorIs(t, err, assert.AnError)
	_, getErr := s.downloads.Get(ctx, d.ID)
	assert.NoError(t, getErr)
	assert.Empty(t, s.events())
	assert.Equal(t, []core.ID{d.ID}, remover.released, "the fetch goes on")
}

func TestDeleteRefusesADownloadThatIsBeingPostProcessed(t *testing.T) {
	for name, states := range map[string][]core.State{
		"fetched":         {core.StateFetching, core.StateFetched},
		"post-processing": {core.StateFetching, core.StateFetched, core.StatePostProcessing},
	} {
		for _, deleteData := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delete_data=%v", name, deleteData), func(t *testing.T) {
				ctx := context.Background()
				s := newStack(t)
				remover := &recordingRemover{repo: s.downloads}
				svc := s.downloadService(app.WithDataRemover(remover))
				d := s.addDownload(t, "movie", 0)
				s.moveTo(t, d.ID, states...)

				err := svc.Delete(ctx, d.ID, deleteData)

				require.ErrorIs(t, err, core.ErrConflict)
				assert.True(t, app.IsBusy(err))
				assert.Equal(t, "download_busy", errCode(t, err))
				_, getErr := s.downloads.Get(ctx, d.ID)
				assert.NoError(t, getErr, "the row stays")
				assert.Empty(t, remover.removed, "no file is touched")
				assert.Empty(t, s.events())
			})
		}
	}
}

func TestDeleteWithDataRefusesWhenTheFetchFinishedBeforeTheHold(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	remover := &recordingRemover{repo: s.downloads}
	svc := s.downloadService(app.WithDataRemover(remover))
	d := s.addDownload(t, "movie", 0)
	s.moveTo(t, d.ID, core.StateFetching)
	// The fetch completes while the remover is stopping it, and the
	// Post-processor may pick the Download up at any moment.
	remover.onHold = func() { s.moveTo(t, d.ID, core.StateFetched) }

	err := svc.Delete(ctx, d.ID, true)

	assert.True(t, app.IsBusy(err), "%v", err)
	assert.Empty(t, remover.removed, "no file is touched")
	assert.Equal(t, []core.ID{d.ID}, remover.released)
	_, getErr := s.downloads.Get(ctx, d.ID)
	assert.NoError(t, getErr)
}

func TestDeleteWithDataHoldsBeforeRemovingFiles(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	remover := &recordingRemover{repo: s.downloads}
	svc := s.downloadService(app.WithDataRemover(remover))
	d := s.addDownload(t, "movie", 0)

	require.NoError(t, svc.Delete(ctx, d.ID, true))

	assert.Equal(t, []core.ID{d.ID}, remover.held)
	assert.Empty(t, remover.released, "the Queue forgets the hold when it sees Removed")
}

func TestDeleteWithDataFailsWhenTheHoldFails(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	remover := &recordingRemover{repo: s.downloads, holdErr: assert.AnError}
	svc := s.downloadService(app.WithDataRemover(remover))
	d := s.addDownload(t, "movie", 0)

	err := svc.Delete(ctx, d.ID, true)

	require.ErrorIs(t, err, assert.AnError)
	assert.Empty(t, remover.removed)
	_, getErr := s.downloads.Get(ctx, d.ID)
	assert.NoError(t, getErr)
}

func TestRemovingFilesDoesNotBlockOtherDownloads(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	removing, finish := make(chan struct{}), make(chan struct{})
	remover := &recordingRemover{repo: s.downloads, onRemove: func() { close(removing); <-finish }}
	svc := s.downloadService(app.WithDataRemover(remover))
	big := s.addDownload(t, "big", 0)
	other := s.addDownload(t, "other", time.Second)

	done := make(chan error, 1)
	go func() { done <- svc.Delete(ctx, big.ID, true) }()
	<-removing

	paused := make(chan error, 1)
	go func() { _, err := svc.Pause(ctx, other.ID); paused <- err }()
	select {
	case err := <-paused:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("pausing another download waited for the file removal")
	}
	close(finish)
	require.NoError(t, <-done)
}

func TestASecondDeleteOfTheSameDownloadWhileTheFirstRunsIsAConflict(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	removing, finish := make(chan struct{}), make(chan struct{})
	remover := &recordingRemover{repo: s.downloads, onRemove: func() { close(removing); <-finish }}
	svc := s.downloadService(app.WithDataRemover(remover))
	d := s.addDownload(t, "big", 0)

	done := make(chan error, 1)
	go func() { done <- svc.Delete(ctx, d.ID, true) }()
	<-removing

	err := svc.Delete(ctx, d.ID, true)

	assert.ErrorIs(t, err, core.ErrConflict)
	assert.Equal(t, "download_deleting", errCode(t, err))
	close(finish)
	require.NoError(t, <-done)
}

func TestDeleteWithoutARemoverStillSucceedsWithDeleteData(t *testing.T) {
	s := newStack(t)
	d := s.addDownload(t, "movie", 0)

	err := s.downloadService().Delete(context.Background(), d.ID, true)

	assert.NoError(t, err)
}

func TestDeleteOfUnknownDownloadIsNotFoundAndPublishesNothing(t *testing.T) {
	s := newStack(t)

	err := s.downloadService().Delete(context.Background(), core.NewID(), false)

	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.Empty(t, s.events())
}
