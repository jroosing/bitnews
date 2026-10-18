package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

var _ core.CategoryRepo = (*store.CategoryRepo)(nil)

func newCategoryRepo(t *testing.T) (*store.CategoryRepo, *store.DB) {
	t.Helper()
	db, _ := openTemp(t)
	return store.NewCategoryRepo(db), db
}

func TestCategoryRepoCreateThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	want := core.Category{ID: core.NewID(), Name: "tv", DirOverride: "/media/tv"}

	require.NoError(t, repo.Create(ctx, want))
	got, err := repo.Get(ctx, want.ID)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestCategoryRepoGetUnknownIDIsNotFound(t *testing.T) {
	repo, _ := newCategoryRepo(t)

	_, err := repo.Get(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestCategoryRepoCreateRejectsInvalidCategoryWithoutWriting(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	bad := core.Category{ID: core.NewID(), Name: "Not Valid"}

	err := repo.Create(ctx, bad)

	require.ErrorIs(t, err, core.ErrInvalidCategoryName)
	_, err = repo.Get(ctx, bad.ID)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestCategoryRepoCreateDuplicateNameIsConflict(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	require.NoError(t, repo.Create(ctx, core.Category{ID: core.NewID(), Name: "tv"}))

	err := repo.Create(ctx, core.Category{ID: core.NewID(), Name: "tv"})

	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestCategoryRepoCreateDuplicateIDIsConflict(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	id := core.NewID()
	require.NoError(t, repo.Create(ctx, core.Category{ID: id, Name: "tv"}))

	err := repo.Create(ctx, core.Category{ID: id, Name: "movies"})

	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestCategoryRepoListReturnsAllOrderedByName(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	tv := core.Category{ID: core.NewID(), Name: "tv"}
	movies := core.Category{ID: core.NewID(), Name: "movies", DirOverride: "/m"}
	require.NoError(t, repo.Create(ctx, tv))
	require.NoError(t, repo.Create(ctx, movies))

	got, err := repo.List(ctx)

	require.NoError(t, err)
	assert.Equal(t, []core.Category{movies, tv}, got)
}

func TestCategoryRepoListOnEmptyDBIsEmpty(t *testing.T) {
	repo, _ := newCategoryRepo(t)

	got, err := repo.List(context.Background())

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCategoryRepoUpdateChangesNameAndDirOverride(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	c := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, repo.Create(ctx, c))
	c.Name, c.DirOverride = "series", "/media/series"

	require.NoError(t, repo.Update(ctx, c))

	got, err := repo.Get(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, c, got)
}

func TestCategoryRepoUpdateUnknownIDIsNotFound(t *testing.T) {
	repo, _ := newCategoryRepo(t)

	err := repo.Update(context.Background(), core.Category{ID: core.NewID(), Name: "tv"})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestCategoryRepoUpdateToTakenNameIsConflict(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	require.NoError(t, repo.Create(ctx, core.Category{ID: core.NewID(), Name: "tv"}))
	movies := core.Category{ID: core.NewID(), Name: "movies"}
	require.NoError(t, repo.Create(ctx, movies))
	movies.Name = "tv"

	err := repo.Update(ctx, movies)

	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestCategoryRepoUpdateRejectsInvalidCategoryAndKeepsRow(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	c := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, repo.Create(ctx, c))

	err := repo.Update(ctx, core.Category{ID: c.ID, Name: "tv", DirOverride: "relative/dir"})

	require.Error(t, err)
	got, err := repo.Get(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, c, got)
}

func TestCategoryRepoDeleteRemovesCategory(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	c := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, repo.Create(ctx, c))

	require.NoError(t, repo.Delete(ctx, c.ID))

	_, err := repo.Get(ctx, c.ID)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestCategoryRepoDeleteUnknownIDIsNotFound(t *testing.T) {
	repo, _ := newCategoryRepo(t)

	err := repo.Delete(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}
