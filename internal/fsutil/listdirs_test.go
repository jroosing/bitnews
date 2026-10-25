package fsutil_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/fsutil"
)

func names(ds []fsutil.Dir) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}

func TestListDirsListsOnlyDirectoriesSortedWithoutHiddenOrLinks(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"tv", "Movies", "anime", ".cache"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644))
	require.NoError(t, os.Symlink(filepath.Join(root, "tv"), filepath.Join(root, "link")))

	got, truncated, err := fsutil.ListDirs(root, 100)

	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, []string{"anime", "Movies", "tv"}, names(got))
	for _, d := range got {
		assert.Equal(t, filepath.Join(root, d.Name), d.Path)
	}
}

func TestListDirsStopsAtTheLimit(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a", "b", "c"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, d), 0o755))
	}

	got, truncated, err := fsutil.ListDirs(root, 2)

	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Equal(t, []string{"a", "b"}, names(got))
}

func TestListDirsMarksFoldersThatCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may write anywhere")
	}
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "open"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "locked"), 0o555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o755) })

	got, _, err := fsutil.ListDirs(root, 10)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "locked", got[0].Name)
	assert.False(t, got[0].Writable)
	assert.True(t, got[1].Writable)
}

func TestNearestDirClimbsToAnExistingFolder(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data", "tv"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "f"), nil, 0o644))

	for in, want := range map[string]string{
		filepath.Join(root, "data", "tv"):                  filepath.Join(root, "data", "tv"),
		filepath.Join(root, "data", "tv") + "/":            filepath.Join(root, "data", "tv"),
		filepath.Join(root, "data", "new", "deeper"):       filepath.Join(root, "data"),
		filepath.Join(root, "data", "f"):                   filepath.Join(root, "data"),
		filepath.Join(root, "data", "tv", "..", "tv", "x"): filepath.Join(root, "data", "tv"),
	} {
		assert.Equal(t, want, fsutil.NearestDir(in), in)
	}
	assert.Equal(t, "/", fsutil.NearestDir("relative/path"), "only absolute paths are taken")
	assert.Equal(t, "/", fsutil.NearestDir("/tmp/\x00bad"), "control characters are refused")
	assert.Equal(t, "/", fsutil.NearestDir(""))
	assert.True(t, strings.HasPrefix(fsutil.NearestDir(root), "/"))
}
