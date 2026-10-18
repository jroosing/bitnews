package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func (s *stack) categoryService() *app.CategoryService {
	return app.NewCategoryService(s.cats, s.downloads, s.bus, app.WithClock(s.clock.Now))
}

func errCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *app.Error
	require.ErrorAs(t, err, &appErr)
	return appErr.Code
}

func TestCreateCategoryStoresItWithAFreshID(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.categoryService()

	got, err := svc.Create(ctx, "tv-4k", "/data/tv")

	require.NoError(t, err)
	assert.NotEmpty(t, got.ID)
	assert.Equal(t, "tv-4k", got.Name)
	assert.Equal(t, "/data/tv", got.DirOverride)
	stored, err := s.cats.Get(ctx, got.ID)
	require.NoError(t, err)
	assert.Equal(t, got, stored)
	listed, err := svc.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []core.Category{got}, listed)
}

func TestCreateCategoryRejectsBadNamesRelativeDirsAndDuplicates(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.categoryService()
	_, err := svc.Create(ctx, "tv", "")
	require.NoError(t, err)

	_, badName := svc.Create(ctx, "TV Shows", "")
	_, dotDot := svc.Create(ctx, "..", "")
	_, relative := svc.Create(ctx, "movies", "movies/here")
	_, duplicate := svc.Create(ctx, "tv", "")

	require.ErrorIs(t, badName, app.ErrInvalidInput)
	assert.Equal(t, "invalid_name", errCode(t, badName))
	assert.Equal(t, "invalid_name", errCode(t, dotDot))
	require.ErrorIs(t, relative, app.ErrInvalidInput)
	assert.Equal(t, "invalid_dir_override", errCode(t, relative))
	require.ErrorIs(t, duplicate, core.ErrConflict)
	assert.Equal(t, "name_taken", errCode(t, duplicate))
}

func TestListCategoriesIsEmptySliceNotNil(t *testing.T) {
	got, err := newStack(t).categoryService().List(context.Background())

	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestUpdateCategoryRenamesSetsAndClearsDirOverride(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.categoryService()
	c, err := svc.Create(ctx, "tv", "")
	require.NoError(t, err)
	name, dir, none := "series", "/data/series", ""

	renamed, err := svc.Update(ctx, c.ID, app.CategoryPatch{Name: &name})
	require.NoError(t, err)
	withDir, err := svc.Update(ctx, c.ID, app.CategoryPatch{DirOverride: &dir})
	require.NoError(t, err)
	cleared, err := svc.Update(ctx, c.ID, app.CategoryPatch{DirOverride: &none})
	require.NoError(t, err)

	assert.Equal(t, core.Category{ID: c.ID, Name: "series"}, renamed)
	assert.Equal(t, core.Category{ID: c.ID, Name: "series", DirOverride: "/data/series"}, withDir)
	assert.Equal(t, core.Category{ID: c.ID, Name: "series"}, cleared)
}

func TestUpdateCategoryValidatesAndReportsConflictsAndMissingRows(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.categoryService()
	tv, err := svc.Create(ctx, "tv", "")
	require.NoError(t, err)
	_, err = svc.Create(ctx, "movies", "")
	require.NoError(t, err)
	bad, rel, taken := "Bad Name", "relative", "movies"

	_, errName := svc.Update(ctx, tv.ID, app.CategoryPatch{Name: &bad})
	_, errDir := svc.Update(ctx, tv.ID, app.CategoryPatch{DirOverride: &rel})
	_, errTaken := svc.Update(ctx, tv.ID, app.CategoryPatch{Name: &taken})
	_, errEmpty := svc.Update(ctx, tv.ID, app.CategoryPatch{})
	_, errMissing := svc.Update(ctx, core.NewID(), app.CategoryPatch{Name: &taken})

	assert.Equal(t, "invalid_name", errCode(t, errName))
	assert.Equal(t, "invalid_dir_override", errCode(t, errDir))
	require.ErrorIs(t, errTaken, core.ErrConflict)
	assert.Equal(t, "name_taken", errCode(t, errTaken))
	require.ErrorIs(t, errEmpty, app.ErrInvalidInput)
	assert.Equal(t, "empty_patch", errCode(t, errEmpty))
	assert.ErrorIs(t, errMissing, core.ErrNotFound)
	stored, err := s.cats.Get(ctx, tv.ID)
	require.NoError(t, err)
	assert.Equal(t, tv, stored)
}

func TestDeleteCategoryClearsItFromDownloadsAndPublishesTheirUpdates(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.categoryService()
	c, err := svc.Create(ctx, "tv", "")
	require.NoError(t, err)
	d := s.addDownload(t, "show", 0)
	s.addDownload(t, "other", 1)
	d.CategoryID = c.ID
	require.NoError(t, s.downloads.Update(ctx, d))

	err = svc.Delete(ctx, c.ID)

	require.NoError(t, err)
	_, getErr := s.cats.Get(ctx, c.ID)
	assert.ErrorIs(t, getErr, core.ErrNotFound)
	events := s.events()
	require.Len(t, events, 1)
	updated, ok := events[0].(core.DownloadUpdated)
	require.True(t, ok)
	assert.Equal(t, d.ID, updated.Download.ID)
	assert.Empty(t, updated.Download.CategoryID)
}

func TestDeleteUnknownCategoryIsNotFound(t *testing.T) {
	err := newStack(t).categoryService().Delete(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}
