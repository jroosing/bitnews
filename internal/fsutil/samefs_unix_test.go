//go:build unix

package fsutil_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

func TestSameFilesystem_PathsInTheSameDirectoryAreOnTheSameFilesystem(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	require.NoError(t, os.Mkdir(a, 0o755))
	require.NoError(t, os.WriteFile(b, []byte("x"), 0o644))

	same, err := fsutil.SameFilesystem(a, b)
	require.NoError(t, err)
	assert.True(t, same)
}

func TestSameFilesystem_MissingPathUsesDeepestExistingAncestor(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no", "such", "dir")

	same, err := fsutil.SameFilesystem(dir, missing)
	require.NoError(t, err)
	assert.True(t, same)

	same, err = fsutil.SameFilesystem(missing, filepath.Join(dir, "other", "missing"))
	require.NoError(t, err)
	assert.True(t, same)
}

func TestSameFilesystem_MissingPathBelowAFileUsesTheFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, nil, 0o644))

	same, err := fsutil.SameFilesystem(dir, filepath.Join(file, "child"))
	require.NoError(t, err)
	assert.True(t, same)
}

func TestSameFilesystem_RelativePaths(t *testing.T) {
	same, err := fsutil.SameFilesystem(".", "does-not-exist-here")
	require.NoError(t, err)
	assert.True(t, same)
}

func TestSameFilesystem_ReportsDifferentDevices(t *testing.T) {
	// /dev is a separate devfs/devtmpfs/tmpfs mount on macOS and Linux hosts.
	// Compare against what the kernel says so the test cannot be wrong on
	// exotic layouts; it still proves the Dev comparison is wired up.
	dir := t.TempDir()
	var a, b syscall.Stat_t
	require.NoError(t, syscall.Stat(dir, &a))
	require.NoError(t, syscall.Stat("/dev", &b))

	same, err := fsutil.SameFilesystem(dir, "/dev")
	require.NoError(t, err)
	assert.Equal(t, a.Dev == b.Dev, same)
	if a.Dev == b.Dev {
		t.Log("temp dir and /dev share a device on this host; difference case not exercised")
	}
}

func TestSameFilesystem_FollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink("/dev", link))

	var want, got syscall.Stat_t
	require.NoError(t, syscall.Stat(dir, &want))
	require.NoError(t, syscall.Stat("/dev", &got))

	same, err := fsutil.SameFilesystem(dir, link)
	require.NoError(t, err)
	assert.Equal(t, want.Dev == got.Dev, same)
}
