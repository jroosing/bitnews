package finalize_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTorrentOriginalsAreNeverTouchedAndUnpackIsKept(t *testing.T) {
	e := newEnv(t)
	d := torrentDownload("Release")
	dir := e.torrentDir("Release")
	writeFiles(t, dir, map[string]string{
		"release.rar":      "rar",
		"release.r00":      "rar",
		"release.par2":     "par",
		"release.nzb":      "nzb",
		"release.nfo":      "nfo",
		"_unpack/film.mkv": "film",
		"_unpack.tmp/x":    "leftover",
	})
	before := hashTree(t, dir)
	want := listing(t, dir)

	out, err := e.finisher().Finish(context.Background(), d, dir, filepath.Join(dir, "_unpack"))
	require.NoError(t, err)

	require.Equal(t, filepath.Join(e.settings.Paths.Torrent.Complete, "Release"), out)
	require.NoDirExists(t, dir)
	got := listing(t, out)
	delete(got, MarkerName)
	require.Equal(t, want, got, "originals, _unpack/ and leftovers all moved intact")
	require.Equal(t, "film", got["_unpack/film.mkv"])

	// Same names, modes, mtimes and bytes as before, apart from the marker.
	require.NoError(t, removeMarker(out))
	require.Equal(t, before, hashTree(t, out))
}

func TestTorrentAlreadyInPlaceIsANoOp(t *testing.T) {
	e := newEnv(t)
	d := torrentDownload("Release")
	e.settings.Paths.Torrent.Incomplete = e.settings.Paths.Torrent.Complete
	dir := filepath.Join(e.settings.Paths.Torrent.Complete, "Release")
	writeFiles(t, dir, map[string]string{"a.mkv": "a", "a.par2": "keep"})
	before := hashTree(t, dir)

	out, err := e.finisher().Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, dir, out)
	require.Equal(t, before, hashTree(t, dir), "nothing added, nothing changed")
	require.Empty(t, e.warns())
}

func TestTorrentAlreadyInPlaceWithCategory(t *testing.T) {
	e := newEnv(t)
	d := torrentDownload("Release")
	d.CategoryID = e.addCategory("tv", "")
	dir := filepath.Join(e.settings.Paths.Torrent.Complete, "tv", "Release")
	writeFiles(t, dir, map[string]string{"a.mkv": "a"})

	out, err := e.finisher().Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, dir, out)
	require.Equal(t, map[string]string{"a.mkv": "a"}, listing(t, dir))
}

func TestTorrentInSameRootButWrongCategoryDirIsMoved(t *testing.T) {
	e := newEnv(t)
	e.settings.Paths.Torrent.Incomplete = e.settings.Paths.Torrent.Complete
	d := torrentDownload("Release")
	d.CategoryID = e.addCategory("tv", "")
	dir := filepath.Join(e.settings.Paths.Torrent.Complete, "Release")
	writeFiles(t, dir, map[string]string{"a.mkv": "a"})

	out, err := e.finisher().Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(e.settings.Paths.Torrent.Complete, "tv", "Release"), out)
	require.NoDirExists(t, dir)
}

func TestDestinationIsWhereFinishWouldMoveATorrent(t *testing.T) {
	e := newEnv(t)
	d := torrentDownload("Release")
	d.CategoryID = e.addCategory("tv", "")

	got, err := e.finisher().Destination(context.Background(), d)

	require.NoError(t, err)
	require.Equal(t, filepath.Join(e.settings.Paths.Torrent.Complete, "tv", "Release"), got)
}
