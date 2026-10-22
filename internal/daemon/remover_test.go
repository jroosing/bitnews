package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

type fakeHolder struct {
	holdErr  error
	held     []core.ID
	released []core.ID
}

func (f *fakeHolder) Hold(_ context.Context, id core.ID) error {
	f.held = append(f.held, id)
	return f.holdErr
}
func (f *fakeHolder) Release(id core.ID) { f.released = append(f.released, id) }

type fakeCats struct {
	core.CategoryRepo
	cats []core.Category
}

func (f fakeCats) List(context.Context) ([]core.Category, error) { return f.cats, nil }

type removerEnv struct {
	incomplete, complete string
	holder               *fakeHolder
	remover              *dataRemover
}

func newRemoverEnv(t *testing.T, cats ...core.Category) *removerEnv {
	t.Helper()
	base := t.TempDir()
	e := &removerEnv{
		incomplete: filepath.Join(base, "incomplete"), complete: filepath.Join(base, "complete"),
		holder: &fakeHolder{},
	}
	require.NoError(t, os.MkdirAll(e.incomplete, 0o777))
	require.NoError(t, os.MkdirAll(e.complete, 0o777))
	s := core.DefaultSettings()
	s.Paths.Usenet = core.Roots{Incomplete: e.incomplete, Complete: e.complete}
	e.remover = &dataRemover{
		settings:   func() core.Settings { return s },
		categories: fakeCats{cats: cats},
		holder:     e.holder,
		dirFor:     func(d core.Download) string { return filepath.Join(e.incomplete, string(d.ID)) },
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return e
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o777))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o666))
}

func nzbDownload(output string) core.Download {
	return core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, OutputPath: output}
}

func TestRemoveDataDeletesTheWorkingDirAndTheOutput(t *testing.T) {
	e := newRemoverEnv(t)
	d := nzbDownload(filepath.Join(e.complete, "tv", "show"))
	writeFile(t, filepath.Join(e.incomplete, string(d.ID), "part.bin"))
	writeFile(t, filepath.Join(d.OutputPath, "episode.mkv"))

	require.NoError(t, e.remover.RemoveData(context.Background(), d))

	assert.NoDirExists(t, filepath.Join(e.incomplete, string(d.ID)))
	assert.NoDirExists(t, d.OutputPath)
	assert.DirExists(t, filepath.Join(e.complete, "tv"), "only the Download's own dir goes")
	assert.DirExists(t, e.incomplete)
	assert.Equal(t, []core.ID{d.ID}, e.holder.held, "the fetch is held off first")
	assert.Empty(t, e.holder.released, "the hold stays until the Download is gone")
}

func TestRemoveDataIsIdempotent(t *testing.T) {
	e := newRemoverEnv(t)
	d := nzbDownload(filepath.Join(e.complete, "gone"))

	require.NoError(t, e.remover.RemoveData(context.Background(), d))
	require.NoError(t, e.remover.RemoveData(context.Background(), d))
}

func TestRemoveDataRefusesTheRootsThemselves(t *testing.T) {
	e := newRemoverEnv(t)
	writeFile(t, filepath.Join(e.complete, "keep.txt"))
	writeFile(t, filepath.Join(e.incomplete, "keep.txt"))

	for _, out := range []string{e.complete, e.complete + "/", filepath.Join(e.complete, "tv", ".."), e.incomplete, filepath.Dir(e.complete), "/", ""} {
		require.NoError(t, e.remover.RemoveData(context.Background(), nzbDownload(out)), out)
	}

	assert.FileExists(t, filepath.Join(e.complete, "keep.txt"))
	assert.FileExists(t, filepath.Join(e.incomplete, "keep.txt"))
}

func TestRemoveDataLeavesPathsOutsideEveryRoot(t *testing.T) {
	e := newRemoverEnv(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "precious"))

	require.NoError(t, e.remover.RemoveData(context.Background(), nzbDownload(outside)))
	require.NoError(t, e.remover.RemoveData(context.Background(), nzbDownload("relative/path")))

	assert.FileExists(t, filepath.Join(outside, "precious"))
}

func TestRemoveDataUnlinksASymlinkedOutputWithoutFollowingIt(t *testing.T) {
	e := newRemoverEnv(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "precious"))
	link := filepath.Join(e.complete, "link")
	require.NoError(t, os.Symlink(outside, link))

	require.NoError(t, e.remover.RemoveData(context.Background(), nzbDownload(link)))

	_, err := os.Lstat(link)
	assert.True(t, os.IsNotExist(err), "the link is gone")
	assert.FileExists(t, filepath.Join(outside, "precious"), "its target is untouched")
}

func TestRemoveDataDoesNotEnterSymlinksInsideTheOutput(t *testing.T) {
	e := newRemoverEnv(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "precious"))
	out := filepath.Join(e.complete, "release")
	writeFile(t, filepath.Join(out, "a.mkv"))
	require.NoError(t, os.Symlink(outside, filepath.Join(out, "escape")))

	require.NoError(t, e.remover.RemoveData(context.Background(), nzbDownload(out)))

	assert.NoDirExists(t, out)
	assert.FileExists(t, filepath.Join(outside, "precious"))
}

func TestRemoveDataRefusesAPathThatClimbsOutThroughASymlinkedParent(t *testing.T) {
	e := newRemoverEnv(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "release", "precious"))
	require.NoError(t, os.Symlink(outside, filepath.Join(e.complete, "tv")))
	d := nzbDownload(filepath.Join(e.complete, "tv", "release"))

	err := e.remover.RemoveData(context.Background(), d)

	require.Error(t, err)
	assert.FileExists(t, filepath.Join(outside, "release", "precious"))
	assert.Equal(t, []core.ID{d.ID}, e.holder.released, "the Download stays, so the hold is released")
}

func TestRemoveDataAllowsTheDirectoryOverrideOfACategory(t *testing.T) {
	override := t.TempDir()
	e := newRemoverEnv(t, core.Category{ID: core.NewID(), Name: "tv", DirOverride: override})
	out := filepath.Join(override, "show")
	writeFile(t, filepath.Join(out, "a.mkv"))

	require.NoError(t, e.remover.RemoveData(context.Background(), nzbDownload(out)))
	require.NoError(t, e.remover.RemoveData(context.Background(), nzbDownload(override)))

	assert.NoDirExists(t, out)
	assert.DirExists(t, override, "the override dir itself stays")
}

func TestRemoveDataStopsWhenTheFetchCannotBeHeld(t *testing.T) {
	e := newRemoverEnv(t)
	e.holder.holdErr = errors.New("still running")
	d := nzbDownload("")
	writeFile(t, filepath.Join(e.incomplete, string(d.ID), "part.bin"))

	err := e.remover.RemoveData(context.Background(), d)

	require.Error(t, err)
	assert.FileExists(t, filepath.Join(e.incomplete, string(d.ID), "part.bin"))
}

func TestRemoveDataIgnoresTorrentsForNow(t *testing.T) {
	e := newRemoverEnv(t)
	out := filepath.Join(e.complete, "t")
	writeFile(t, filepath.Join(out, "a"))
	d := core.Download{ID: core.NewID(), SourceKind: core.SourceTorrent, OutputPath: out}

	require.NoError(t, e.remover.RemoveData(context.Background(), d))

	assert.FileExists(t, filepath.Join(out, "a"))
	assert.Empty(t, e.holder.held)
}
