package fsutil_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

func touch(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, nil, 0o644))
}

func TestUnique_ReturnsPathUnchangedWhenFree(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file.mkv")
	assert.Equal(t, p, fsutil.Unique(p))
}

func TestUnique_InsertsCounterBeforeExtension(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file.mkv")
	touch(t, p)
	assert.Equal(t, filepath.Join(dir, "file.1.mkv"), fsutil.Unique(p))

	touch(t, filepath.Join(dir, "file.1.mkv"))
	assert.Equal(t, filepath.Join(dir, "file.2.mkv"), fsutil.Unique(p))
}

func TestUnique_DirectoriesGetCounterAppended(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "Show.S01.1080p")
	require.NoError(t, os.Mkdir(d, 0o755))
	assert.Equal(t, d+".1", fsutil.Unique(d))

	require.NoError(t, os.Mkdir(d+".1", 0o755))
	assert.Equal(t, d+".2", fsutil.Unique(d))
}

func TestUnique_DotfilesAndNoExtension(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".hidden", "README"} {
		p := filepath.Join(dir, name)
		touch(t, p)
		assert.Equal(t, p+".1", fsutil.Unique(p))
	}
}

func TestUnique_TreatsDanglingSymlinkAsTaken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "file.txt")
	require.NoError(t, os.Symlink(filepath.Join(dir, "nowhere"), p))
	assert.Equal(t, filepath.Join(dir, "file.1.txt"), fsutil.Unique(p))
}

func TestUnique_KeepsNameWithin255Bytes(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("a", 251) + ".mkv"
	p := filepath.Join(dir, name)
	touch(t, p)

	got := fsutil.Unique(p)
	assert.Equal(t, dir, filepath.Dir(got))
	assert.LessOrEqual(t, len(filepath.Base(got)), 255)
	assert.True(t, strings.HasSuffix(got, ".1.mkv"))
	_, err := os.Lstat(got)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
