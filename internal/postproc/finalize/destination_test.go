package finalize_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/fsutil"
)

func finishUsenet(t *testing.T, e *env, d core.Download) (string, error) {
	t.Helper()
	dir := e.usenetDir("work-" + string(d.ID))
	writeFiles(t, dir, map[string]string{"a.mkv": "a"})
	return e.finisher().Finish(context.Background(), d, dir, "")
}

func TestDestinationUsenetCategoryDir(t *testing.T) {
	e := newEnv(t)
	d := usenetDownload("Show")
	d.CategoryID = e.addCategory("tv", "")

	out, err := finishUsenet(t, e, d)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(e.settings.Paths.Usenet.Complete, "tv", "Show"), out)
	require.FileExists(t, filepath.Join(out, "a.mkv"))
}

func TestDestinationNoCategoryUsesCompleteRoot(t *testing.T) {
	e := newEnv(t)
	out, err := finishUsenet(t, e, usenetDownload("Show"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(e.settings.Paths.Usenet.Complete, "Show"), out)
}

func TestDestinationDirOverrideWinsOverCategoryDir(t *testing.T) {
	e := newEnv(t)
	override := filepath.Join(e.base, "elsewhere", "movies")
	d := usenetDownload("Film")
	d.CategoryID = e.addCategory("movies", override)

	out, err := finishUsenet(t, e, d)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(override, "Film"), out)
	require.NoDirExists(t, filepath.Join(e.settings.Paths.Usenet.Complete, "movies"))
}

func TestDestinationIsResolvedAtMoveTime(t *testing.T) {
	e := newEnv(t)
	d := usenetDownload("Show")
	d.CategoryID = e.addCategory("tv", "")
	f := e.finisher()
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"a": "a"})

	// Category renamed and settings changed after the Download was queued.
	e.cats[d.CategoryID] = core.Category{ID: d.CategoryID, Name: "series"}
	e.settings.Paths.Usenet.Complete = filepath.Join(e.base, "moved-root")

	out, err := f.Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(e.base, "moved-root", "series", "Show"), out)
}

func TestDestinationTorrentUsesTorrentCompleteRoot(t *testing.T) {
	e := newEnv(t)
	d := torrentDownload("Linux.ISO")
	d.CategoryID = e.addCategory("iso", "")
	dir := e.torrentDir("Linux.ISO")
	writeFiles(t, dir, map[string]string{"a.iso": "iso"})

	out, err := e.finisher().Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(e.settings.Paths.Torrent.Complete, "iso", "Linux.ISO"), out)
}

func TestDestinationDeletedCategoryFallsBackToRootWithWarning(t *testing.T) {
	e := newEnv(t)
	d := usenetDownload("Show")
	d.CategoryID = core.NewID() // not in the repo

	out, err := finishUsenet(t, e, d)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(e.settings.Paths.Usenet.Complete, "Show"), out)
	require.Len(t, e.warns(), 1)
	require.Equal(t, "category_missing", e.warns()[0].Kind)
}

func TestDestinationCollisionUsesUnique(t *testing.T) {
	e := newEnv(t)
	taken := filepath.Join(e.settings.Paths.Usenet.Complete, "Show")
	writeFiles(t, taken, map[string]string{"old.mkv": "old"})

	out, err := finishUsenet(t, e, usenetDownload("Show"))
	require.NoError(t, err)
	require.Equal(t, taken+".1", out)
	require.Equal(t, "old", listing(t, taken)["old.mkv"], "existing dir untouched")
	require.FileExists(t, filepath.Join(out, "a.mkv"))
}

func TestDestinationHostileCategoryNameIsRejected(t *testing.T) {
	for _, name := range []string{"..", "../escape", "a/../../x", "/abs", `..\x`} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			d := usenetDownload("Show")
			d.CategoryID = e.addCategory(name, "")
			dir := e.usenetDir("w")
			writeFiles(t, dir, map[string]string{"junk.par2": "p", "a.mkv": "a"})

			_, err := e.finisher().Finish(context.Background(), d, dir, "")
			require.Error(t, err)
			// Nothing happened: not moved, nothing deleted, nothing escaped.
			require.FileExists(t, filepath.Join(dir, "junk.par2"))
			require.NoDirExists(t, filepath.Join(e.base, "escape"))
			require.NoDirExists(t, filepath.Join(e.base, "usenet", "escape"))
		})
	}
}

func TestDestinationHostileOverrideIsRejected(t *testing.T) {
	for _, override := range []string{"relative/dir", "/data/../../etc", "/data/x/../../y", ""} {
		t.Run(override, func(t *testing.T) {
			e := newEnv(t)
			d := usenetDownload("Show")
			d.CategoryID = e.addCategory("tv", override)
			dir := e.usenetDir("w")
			writeFiles(t, dir, map[string]string{"a.mkv": "a"})

			out, err := e.finisher().Finish(context.Background(), d, dir, "")
			if override == "" { // no override is the normal case
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(out, e.settings.Paths.Usenet.Complete))
				return
			}
			require.Error(t, err)
			require.DirExists(t, dir)
		})
	}
}

func TestDestinationCategoryDirSymlinkedOutsideRootIsRejected(t *testing.T) {
	e := newEnv(t)
	outside := filepath.Join(e.base, "outside")
	require.NoError(t, osMkdirAll(outside))
	require.NoError(t, osMkdirAll(e.settings.Paths.Usenet.Complete))
	require.NoError(t, osSymlink(outside, filepath.Join(e.settings.Paths.Usenet.Complete, "tv")))
	d := usenetDownload("Show")
	d.CategoryID = e.addCategory("tv", "")

	_, err := finishUsenet(t, e, d)
	require.ErrorIs(t, err, fsutil.ErrUnsafePath)
	require.Empty(t, listing(t, outside))
}

func TestDestinationNamesAreSanitized(t *testing.T) {
	long := strings.Repeat("x", 400)
	tests := []struct {
		name string
		in   string
		want func(id core.ID) string
	}{
		{"traversal", "../x", func(core.ID) string { return ".._x" }},
		{"slashes", "a/b\\c", func(core.ID) string { return "a_b_c" }},
		{"empty", "", func(id core.ID) string { return "download-" + string(id) }},
		{"dots", "...", func(id core.ID) string { return "download-" + string(id) }},
		{"long", long, func(core.ID) string { return long[:255] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			d := usenetDownload(tt.in)
			out, err := finishUsenet(t, e, d)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(e.settings.Paths.Usenet.Complete, tt.want(d.ID)), out)
		})
	}
}

func TestDestinationParentIsCreated(t *testing.T) {
	e := newEnv(t)
	require.NoDirExists(t, e.settings.Paths.Usenet.Complete)
	out, err := finishUsenet(t, e, usenetDownload("Show"))
	require.NoError(t, err)
	require.DirExists(t, filepath.Dir(out))
}

func TestDestinationUnknownSourceKindFails(t *testing.T) {
	e := newEnv(t)
	d := usenetDownload("Show")
	d.SourceKind = "ftp"
	_, err := finishUsenet(t, e, d)
	require.Error(t, err)
}
