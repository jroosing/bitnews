package par2_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func ids(in [][16]byte) []par2.ID {
	var out []par2.ID
	for _, id := range in {
		out = append(out, par2.ID(id))
	}
	return out
}

func TestLoadDir_IndexOnlySet(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40, 5, 16)...)
	writeFile(t, dir, "movie.par2", set.Index())

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Equal(t, par2.ID(set.SetID), got.ID)
	require.Equal(t, uint64(16), got.SliceSize)
	require.Equal(t, ids(set.IDs), got.FileIDs)
	require.Empty(t, got.NonRecoveryIDs)
	require.Len(t, got.FileDescs, 3)
	require.Len(t, got.IFSCs, 3)
	for i, f := range set.Files {
		fd := got.FileDescs[par2.ID(set.IDs[i])]
		require.NotNil(t, fd)
		require.Equal(t, f.Name, fd.Name)
		require.Equal(t, uint64(len(f.Data)), fd.Length)
		require.Len(t, got.IFSCs[par2.ID(set.IDs[i])].Slices, int(sliceCount(uint64(len(f.Data)), 16)))
	}
	require.Empty(t, got.Recovery)
	require.Equal(t, "bitnews test builder", got.Creator)
	require.Empty(t, got.Warnings)
}

func TestLoadDir_MergesIndexAndVolumeFiles(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40, 5)...)
	writeFile(t, dir, "movie.par2", set.Index())
	vol1 := writeFile(t, dir, "movie.vol000+01.par2", set.Volume(0))
	vol2 := writeFile(t, dir, "movie.vol001+04.par2", set.Volume(1, 2, 3, 4))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, got.Recovery, 5)
	require.Empty(t, got.Warnings)
	for e := uint32(0); e <= 4; e++ {
		rs, ok := got.Recovery[e]
		require.True(t, ok, "exponent %d", e)
		require.Equal(t, int64(16), rs.Length)
		data, err := os.ReadFile(rs.Path)
		require.NoError(t, err)
		require.Equal(t, set.sliceData(e), data[rs.Offset:rs.Offset+rs.Length], "exponent %d", e)
	}
	require.Equal(t, vol1, got.Recovery[0].Path)
	require.Equal(t, vol2, got.Recovery[3].Path)
}

func TestLoadDir_IgnoresEverythingThatIsNotATopLevelPar2File(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	other := newSet(16, testFiles(99, 98)...)
	writeFile(t, dir, "MOVIE.PAR2", set.Index()) // extension match is case-insensitive
	writeFile(t, dir, "movie.nzb", other.Index())
	writeFile(t, dir, "movie.par2.txt", other.Index())
	writeFile(t, dir, "par2", other.Index())
	writeFile(t, dir, "empty.par2", nil)
	writeFile(t, dir, "junk.par2", []byte("not a par2 file at all"))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "dir.par2"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	writeFile(t, dir, "sub/nested.par2", other.Index())

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Equal(t, par2.ID(set.SetID), got.ID)
	require.Len(t, got.FileDescs, 1)
}

func TestLoadDir_IgnoresSymlinkedPar2Files(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	set := newSet(16, testFiles(40)...)
	target := writeFile(t, outside, "real.par2", set.Index())
	if err := os.Symlink(target, filepath.Join(dir, "link.par2")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	_, err := par2.LoadDir(dir)

	require.ErrorIs(t, err, par2.ErrNoMainPacket)
}

func TestLoadDir_NoPar2FilesIsNoMainPacket(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("x"))

	_, err := par2.LoadDir(dir)

	require.ErrorIs(t, err, par2.ErrNoMainPacket)
}

func TestLoadDir_MissingDirIsAnError(t *testing.T) {
	_, err := par2.LoadDir(filepath.Join(t.TempDir(), "nope"))

	require.Error(t, err)
	require.NotErrorIs(t, err, par2.ErrNoMainPacket)
}
