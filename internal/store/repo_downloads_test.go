package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

var _ core.DownloadRepo = (*store.DownloadRepo)(nil)

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newDownloadRepo(t *testing.T) (*store.DownloadRepo, *store.DB) {
	t.Helper()
	db, _ := openTemp(t)
	return store.NewDownloadRepo(db), db
}

// newDownload returns a valid Added NZB Download added at epoch+offset.
func newDownload(name string, offset time.Duration) core.Download {
	at := epoch.Add(offset)
	return core.Download{
		ID:         core.NewID(),
		Name:       name,
		SourceKind: core.SourceNZB,
		State:      core.StateAdded,
		Priority:   core.DownloadPriorityNormal,
		AddedAt:    at,
		UpdatedAt:  at,
	}
}

func TestDownloadRepoCreateThenGetRoundTripsMinimalDownload(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	want := newDownload("minimal", 0)

	require.NoError(t, repo.Create(ctx, want))
	got, err := repo.Get(ctx, want.ID)

	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.True(t, got.FetchedAt.IsZero())
	assert.True(t, got.ReadyAt.IsZero())
}

func TestDownloadRepoRoundTripsEveryField(t *testing.T) {
	ctx := context.Background()
	repo, db := newDownloadRepo(t)
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, store.NewCategoryRepo(db).Create(ctx, cat))
	want := core.Download{
		ID:            core.NewID(),
		Name:          "Some.Show.S01E01",
		SourceKind:    core.SourceTorrent,
		CategoryID:    cat.ID,
		State:         core.StateFailed,
		Priority:      core.DownloadPriorityForce,
		QueuePosition: 7,
		Paused:        true,
		FailureReason: core.FailureDiskFull,
		FailureDetail: "no space left",
		TotalBytes:    1 << 40,
		DoneBytes:     12345,
		AddedAt:       epoch,
		FetchedAt:     epoch.Add(time.Minute),
		ReadyAt:       epoch.Add(2 * time.Minute),
		UpdatedAt:     epoch.Add(3 * time.Minute),
		OutputPath:    "/data/complete/tv/Some.Show",
	}

	require.NoError(t, repo.Create(ctx, want))
	got, err := repo.Get(ctx, want.ID)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestDownloadRepoStoresTimesAsUTCMilliseconds(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	d := newDownload("tz", 0)
	zone := time.FixedZone("UTC+5", 5*3600)
	d.AddedAt = time.Date(2026, 3, 1, 17, 0, 0, 123_456_789, zone)
	d.UpdatedAt = d.AddedAt

	require.NoError(t, repo.Create(ctx, d))
	got, err := repo.Get(ctx, d.ID)

	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 3, 1, 12, 0, 0, 123_000_000, time.UTC), got.AddedAt)
}

func TestDownloadRepoZeroTimesAndEmptyStringsAreNull(t *testing.T) {
	ctx := context.Background()
	repo, db := newDownloadRepo(t)
	d := newDownload("nulls", 0)
	require.NoError(t, repo.Create(ctx, d))

	var cat, reason, fetched, ready any
	err := db.Reader().QueryRowContext(ctx,
		`SELECT category_id, failure_reason, fetched_at, ready_at FROM downloads WHERE id = ?`,
		string(d.ID)).Scan(&cat, &reason, &fetched, &ready)

	require.NoError(t, err)
	assert.Nil(t, cat)
	assert.Nil(t, reason)
	assert.Nil(t, fetched)
	assert.Nil(t, ready)
}

func TestDownloadRepoGetUnknownIDIsNotFound(t *testing.T) {
	repo, _ := newDownloadRepo(t)

	_, err := repo.Get(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestDownloadRepoCreateDuplicateIDIsConflict(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	d := newDownload("dup", 0)
	require.NoError(t, repo.Create(ctx, d))

	err := repo.Create(ctx, d)

	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestDownloadRepoUpdateStoresEditableFields(t *testing.T) {
	ctx := context.Background()
	repo, db := newDownloadRepo(t)
	cat := core.Category{ID: core.NewID(), Name: "movies"}
	require.NoError(t, store.NewCategoryRepo(db).Create(ctx, cat))
	d := newDownload("old", 0)
	require.NoError(t, repo.Create(ctx, d))
	d.Name = "new"
	d.CategoryID = cat.ID
	d.Priority = core.DownloadPriorityHigh
	d.QueuePosition = 3
	d.Paused = true
	d.TotalBytes = 1000
	d.DoneBytes = 400
	d.UpdatedAt = epoch.Add(time.Hour)

	require.NoError(t, repo.Update(ctx, d))

	got, err := repo.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, d, got)
}

func TestDownloadRepoUpdateClearsCategory(t *testing.T) {
	ctx := context.Background()
	repo, db := newDownloadRepo(t)
	cat := core.Category{ID: core.NewID(), Name: "movies"}
	require.NoError(t, store.NewCategoryRepo(db).Create(ctx, cat))
	d := newDownload("x", 0)
	d.CategoryID = cat.ID
	require.NoError(t, repo.Create(ctx, d))
	d.CategoryID = ""

	require.NoError(t, repo.Update(ctx, d))

	got, err := repo.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Empty(t, got.CategoryID)
}

func TestDownloadRepoUpdateIgnoresStateAndStateChangeFields(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	d := newDownload("x", 0)
	require.NoError(t, repo.Create(ctx, d))
	tampered := d
	tampered.State = core.StateReady
	tampered.FetchedAt = epoch.Add(time.Minute)
	tampered.ReadyAt = epoch.Add(time.Minute)
	tampered.OutputPath = "/elsewhere"
	tampered.FailureReason = core.FailureInternal
	tampered.FailureDetail = "boom"

	require.NoError(t, repo.Update(ctx, tampered))

	got, err := repo.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, d, got)
}

func TestDownloadRepoUpdateUnknownIDIsNotFound(t *testing.T) {
	repo, _ := newDownloadRepo(t)

	err := repo.Update(context.Background(), newDownload("ghost", 0))

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestDownloadRepoUpdateStateAppliesCoreChangeAndPersists(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	d := newDownload("x", 0)
	d.State = core.StatePostProcessing
	require.NoError(t, repo.Create(ctx, d))
	at := epoch.Add(time.Hour)

	got, err := repo.UpdateState(ctx, d.ID, core.StateChange{
		To: core.StateReady, At: at, OutputPath: "/data/complete/x",
	})

	require.NoError(t, err)
	want := d
	require.NoError(t, want.ChangeState(core.StateChange{
		To: core.StateReady, At: at, OutputPath: "/data/complete/x",
	}))
	assert.Equal(t, want, got)
	stored, err := repo.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, want, stored)
}

func TestDownloadRepoUpdateStatePersistsFailure(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	d := newDownload("x", 0)
	d.State = core.StateFetching
	require.NoError(t, repo.Create(ctx, d))

	_, err := repo.UpdateState(ctx, d.ID, core.StateChange{
		To: core.StateFailed, At: epoch.Add(time.Minute),
		FailureReason: core.FailureMissingArticles, FailureDetail: "12 missing",
	})

	require.NoError(t, err)
	stored, err := repo.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, core.StateFailed, stored.State)
	assert.Equal(t, core.FailureMissingArticles, stored.FailureReason)
	assert.Equal(t, "12 missing", stored.FailureDetail)
}

func TestDownloadRepoUpdateStateIllegalMoveReturnsCoreErrorAndLeavesRowUntouched(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	d := newDownload("x", 0) // Added -> Ready is not a legal move
	require.NoError(t, repo.Create(ctx, d))

	_, err := repo.UpdateState(ctx, d.ID, core.StateChange{
		To: core.StateReady, At: epoch.Add(time.Hour), OutputPath: "/out",
	})

	require.ErrorIs(t, err, core.ErrIllegalTransition)
	stored, err := repo.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, d, stored)
}

func TestDownloadRepoUpdateStateInvalidChangeReturnsCoreErrorAndLeavesRowUntouched(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	d := newDownload("x", 0)
	d.State = core.StatePostProcessing
	require.NoError(t, repo.Create(ctx, d))

	_, err := repo.UpdateState(ctx, d.ID, core.StateChange{To: core.StateReady, At: epoch})

	require.ErrorIs(t, err, core.ErrInvalidStateChange)
	stored, err := repo.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, d, stored)
}

func TestDownloadRepoUpdateStateUnknownIDIsNotFound(t *testing.T) {
	repo, _ := newDownloadRepo(t)

	_, err := repo.UpdateState(context.Background(), core.NewID(), core.StateChange{To: core.StateQueued})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestDownloadRepoDeleteRemovesDownload(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	d := newDownload("x", 0)
	require.NoError(t, repo.Create(ctx, d))

	require.NoError(t, repo.Delete(ctx, d.ID))

	_, err := repo.Get(ctx, d.ID)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestDownloadRepoDeleteUnknownIDIsNotFound(t *testing.T) {
	repo, _ := newDownloadRepo(t)

	err := repo.Delete(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestCategoryRepoDeleteClearsCategoryOfItsDownloads(t *testing.T) {
	ctx := context.Background()
	downloads, db := newDownloadRepo(t)
	categories := store.NewCategoryRepo(db)
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, categories.Create(ctx, cat))
	d := newDownload("x", 0)
	d.CategoryID = cat.ID
	require.NoError(t, downloads.Create(ctx, d))

	require.NoError(t, categories.Delete(ctx, cat.ID))

	got, err := downloads.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Empty(t, got.CategoryID)
}

func ids(ds []core.Download) []core.ID {
	out := make([]core.ID, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func createAll(t *testing.T, repo *store.DownloadRepo, ds ...core.Download) {
	t.Helper()
	for _, d := range ds {
		require.NoError(t, repo.Create(context.Background(), d))
	}
}

func TestDownloadRepoListOrdersByAddedAtThenID(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	late := newDownload("late", time.Hour)
	earlyB := newDownload("early-b", 0)
	earlyA := newDownload("early-a", 0)
	earlyA.ID, earlyB.ID = "aaa", "bbb"
	createAll(t, repo, late, earlyB, earlyA)

	page, next, err := repo.List(ctx, core.DownloadQuery{})

	require.NoError(t, err)
	assert.Equal(t, []core.ID{"aaa", "bbb", late.ID}, ids(page))
	assert.Empty(t, next)
}

func TestDownloadRepoListOnEmptyDBIsEmptyWithNoCursor(t *testing.T) {
	repo, _ := newDownloadRepo(t)

	page, next, err := repo.List(context.Background(), core.DownloadQuery{})

	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Empty(t, next)
}

func TestDownloadRepoListFiltersByStateCategoryAndSourceKind(t *testing.T) {
	ctx := context.Background()
	repo, db := newDownloadRepo(t)
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, store.NewCategoryRepo(db).Create(ctx, cat))
	plain := newDownload("plain", 0)
	queued := newDownload("queued", time.Second)
	queued.State = core.StateQueued
	inCat := newDownload("in-cat", 2*time.Second)
	inCat.CategoryID = cat.ID
	torrent := newDownload("torrent", 3*time.Second)
	torrent.SourceKind = core.SourceTorrent
	createAll(t, repo, plain, queued, inCat, torrent)

	tests := []struct {
		name string
		q    core.DownloadQuery
		want []core.ID
	}{
		{"state", core.DownloadQuery{State: core.StateQueued}, []core.ID{queued.ID}},
		{"category", core.DownloadQuery{CategoryID: cat.ID}, []core.ID{inCat.ID}},
		{"kind", core.DownloadQuery{SourceKind: core.SourceTorrent}, []core.ID{torrent.ID}},
		{"combined", core.DownloadQuery{State: core.StateAdded, SourceKind: core.SourceNZB},
			[]core.ID{plain.ID, inCat.ID}},
		{"no match", core.DownloadQuery{State: core.StateReady}, []core.ID{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, _, err := repo.List(ctx, tt.q)

			require.NoError(t, err)
			assert.Equal(t, tt.want, ids(page))
		})
	}
}

func TestDownloadRepoListPagesThroughAllRowsWithCursor(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	var want []core.ID
	for i := range 5 {
		d := newDownload("d", time.Duration(i)*time.Second)
		createAll(t, repo, d)
		want = append(want, d.ID)
	}

	var got []core.ID
	var pages int
	after := ""
	for {
		page, next, err := repo.List(ctx, core.DownloadQuery{After: after, Limit: 2})
		require.NoError(t, err)
		got = append(got, ids(page)...)
		pages++
		if next == "" {
			break
		}
		after = next
	}

	assert.Equal(t, want, got)
	assert.Equal(t, 3, pages)
}

func TestDownloadRepoListExactMultipleOfLimitEndsWithoutEmptyPage(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	for i := range 4 {
		createAll(t, repo, newDownload("d", time.Duration(i)*time.Second))
	}

	first, next, err := repo.List(ctx, core.DownloadQuery{Limit: 2})
	require.NoError(t, err)
	require.NotEmpty(t, next)
	second, next, err := repo.List(ctx, core.DownloadQuery{Limit: 2, After: next})

	require.NoError(t, err)
	assert.Len(t, first, 2)
	assert.Len(t, second, 2)
	assert.Empty(t, next)
}

func TestDownloadRepoListPagingKeepsFilters(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	for i := range 6 {
		d := newDownload("d", time.Duration(i)*time.Second)
		if i%2 == 0 {
			d.SourceKind = core.SourceTorrent
		}
		createAll(t, repo, d)
	}
	q := core.DownloadQuery{SourceKind: core.SourceTorrent, Limit: 2}

	first, next, err := repo.List(ctx, q)
	require.NoError(t, err)
	q.After = next
	second, next, err := repo.List(ctx, q)

	require.NoError(t, err)
	assert.Len(t, first, 2)
	assert.Len(t, second, 1)
	assert.Empty(t, next)
	for _, d := range append(first, second...) {
		assert.Equal(t, core.SourceTorrent, d.SourceKind)
	}
}

func TestDownloadRepoListCursorIsStableUnderConcurrentInserts(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	var existing []core.ID
	for i := range 6 {
		d := newDownload("d", time.Duration(i+1)*time.Second)
		createAll(t, repo, d)
		existing = append(existing, d.ID)
	}
	first, next, err := repo.List(ctx, core.DownloadQuery{Limit: 3})
	require.NoError(t, err)

	// Inserts between the pages: one before the cursor, one inside the
	// remaining range and one after it.
	before := newDownload("before", 0)
	inside := newDownload("inside", 4*time.Second+time.Millisecond)
	after := newDownload("after", time.Hour)
	createAll(t, repo, before, inside, after)
	got := ids(first)
	for next != "" {
		var page []core.Download
		page, next, err = repo.List(ctx, core.DownloadQuery{Limit: 3, After: next})
		require.NoError(t, err)
		got = append(got, ids(page)...)
	}

	// Every row that existed at the first page shows up exactly once, in
	// order; rows inserted behind the cursor are not revisited.
	want := append(slices.Clone(existing[:4]), inside.ID)
	want = append(want, existing[4:]...)
	want = append(want, after.ID)
	assert.Equal(t, want, got)
	assert.Len(t, toSet(got), len(got))
	assert.NotContains(t, got, before.ID)
}

func toSet(ids []core.ID) map[core.ID]bool {
	s := make(map[core.ID]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

func TestDownloadRepoListLimitDefaultsAndIsCapped(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	for i := range store.MaxListLimit + 5 {
		createAll(t, repo, newDownload("d", time.Duration(i)*time.Second))
	}

	byDefault, next, err := repo.List(ctx, core.DownloadQuery{})
	require.NoError(t, err)
	assert.Len(t, byDefault, store.DefaultListLimit)
	assert.NotEmpty(t, next)

	capped, next, err := repo.List(ctx, core.DownloadQuery{Limit: 10 * store.MaxListLimit})
	require.NoError(t, err)
	assert.Len(t, capped, store.MaxListLimit)
	assert.NotEmpty(t, next)
}

func TestDownloadRepoListRejectsMalformedCursor(t *testing.T) {
	repo, _ := newDownloadRepo(t)

	for _, cursor := range []string{"not base64!", "bm9jb2xvbg", "eDp5"} { // "nocolon", "x:y"
		_, _, err := repo.List(context.Background(), core.DownloadQuery{After: cursor})

		assert.ErrorIs(t, err, store.ErrInvalidCursor, cursor)
	}
}

func TestDownloadRepoListHonorsCancelledContext(t *testing.T) {
	repo, _ := newDownloadRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := repo.List(ctx, core.DownloadQuery{})

	assert.ErrorIs(t, err, context.Canceled)
}

func TestDownloadRepoWritesHonorCancelledContext(t *testing.T) {
	repo, _ := newDownloadRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newDownload("x", 0)

	assert.ErrorIs(t, repo.Create(ctx, d), context.Canceled)
	_, err := repo.UpdateState(ctx, d.ID, core.StateChange{To: core.StateQueued})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestDownloadRepoUpdateKeepsAddedAtSoPagingStaysStable(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	var all []core.Download
	for i := range 4 {
		d := newDownload("d", time.Duration(i)*time.Second)
		createAll(t, repo, d)
		all = append(all, d)
	}
	first, next, err := repo.List(ctx, core.DownloadQuery{Limit: 2})
	require.NoError(t, err)
	require.NotEmpty(t, next)

	moved := all[2]
	moved.AddedAt = epoch.Add(-time.Hour)
	require.NoError(t, repo.Update(ctx, moved))

	stored, err := repo.Get(ctx, moved.ID)
	require.NoError(t, err)
	assert.Equal(t, all[2].AddedAt, stored.AddedAt)
	second, next, err := repo.List(ctx, core.DownloadQuery{Limit: 2, After: next})
	require.NoError(t, err)
	assert.Equal(t, ids(all), append(ids(first), ids(second)...))
	assert.Empty(t, next)
}
