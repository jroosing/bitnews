package store_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
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
	want := core.Category{ID: core.NewID(), Name: "tv", UsenetDir: "/media/tv", TorrentDir: "/media/torrents/tv"}

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
	movies := core.Category{ID: core.NewID(), Name: "movies", UsenetDir: "/m"}
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

func TestCategoryRepoUpdateChangesNameAndDirOverrides(t *testing.T) {
	ctx := context.Background()
	repo, _ := newCategoryRepo(t)
	c := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, repo.Create(ctx, c))
	c.Name, c.UsenetDir, c.TorrentDir = "series", "/media/series", "/media/torrents/series"

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

	err := repo.Update(ctx, core.Category{ID: c.ID, Name: "tv", TorrentDir: "relative/dir"})

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

// Before migration 9 one folder override served both protocols; it becomes
// the override of each, so nothing changes until the user splits them.
func TestMigration0009SplitsTheDirOverrideIntoOnePerProtocol(t *testing.T) {
	ctx := context.Background()
	upTo8 := fstest.MapFS{}
	files, err := fs.Glob(os.DirFS("migrations"), "*.sql")
	require.NoError(t, err)
	for _, f := range files {
		if f < "0009" {
			data, err := os.ReadFile(filepath.Join("migrations", f))
			require.NoError(t, err)
			upTo8[f] = &fstest.MapFile{Data: data}
		}
	}
	path := filepath.Join(t.TempDir(), "bitnews.db")
	old, err := store.Open(ctx, path, store.WithMigrations(upTo8))
	require.NoError(t, err)
	_, err = old.Writer().ExecContext(ctx, `INSERT INTO categories (id, name, dir_override) VALUES ('c1', 'movies', '/data/usenet/complete/movies'), ('c2', 'tv', '')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	db, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cats, err := store.NewCategoryRepo(db).List(ctx)

	require.NoError(t, err)
	assert.Equal(t, []core.Category{
		{ID: "c1", Name: "movies", UsenetDir: "/data/usenet/complete/movies", TorrentDir: "/data/usenet/complete/movies"},
		{ID: "c2", Name: "tv"},
	}, cats)
	var name string
	require.NoError(t, db.Reader().QueryRow(`SELECT name FROM schema_migrations WHERE version = 9`).Scan(&name))
	assert.True(t, strings.Contains(name, "category"), name)
}
