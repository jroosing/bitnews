package fsutil_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/fsutil"
)

func TestMkdirAll_CreatesMissingDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")

	require.NoError(t, fsutil.MkdirAll(dir, 0o777))

	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, fi.IsDir())
	require.NoError(t, fsutil.MkdirAll(dir, 0o777), "an existing directory is fine")
}

func TestMkdirAll_UnwritableParentNamesTheFolderAndTheUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	parent := filepath.Join(t.TempDir(), "locked")
	require.NoError(t, os.Mkdir(parent, 0o555))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	err := fsutil.MkdirAll(filepath.Join(parent, "sub", "dir"), 0o777)

	require.Error(t, err)
	assert.ErrorIs(t, err, fs.ErrPermission)
	assert.Contains(t, err.Error(), parent)
	assert.Contains(t, err.Error(), fmt.Sprintf("uid %d", os.Geteuid()))
}

func TestMkdirAll_OtherErrorsAreNotRelabelled(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	err := fsutil.MkdirAll(filepath.Join(file, "sub"), 0o777)

	require.Error(t, err)
	assert.False(t, errors.Is(err, fs.ErrPermission))
	assert.NotContains(t, err.Error(), "uid")
}

func TestWriteError_NamesTheFolderOfAFailedWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.Mkdir(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, err := os.Create(filepath.Join(dir, "file"))
	require.Error(t, err)

	got := fsutil.WriteError(err)

	assert.ErrorIs(t, got, fs.ErrPermission)
	assert.Contains(t, got.Error(), dir)
	assert.Contains(t, got.Error(), fmt.Sprintf("uid %d", os.Geteuid()))
	var d interface{ FailureDetail() string }
	require.ErrorAs(t, got, &d)
	assert.Contains(t, d.FailureDetail(), dir)
}

func TestWriteError_LeavesOtherErrorsAlone(t *testing.T) {
	err := errors.New("boom")
	assert.Same(t, err, fsutil.WriteError(err))
	assert.NoError(t, fsutil.WriteError(nil))
}

func TestWriteError_NamesTheDestinationFolderOfARefusedRename(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	require.NoError(t, os.Mkdir(src, 0o755))
	dstParent := filepath.Join(base, "ro")
	require.NoError(t, os.Mkdir(dstParent, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dstParent, 0o755) })

	got := fsutil.WriteError(os.Rename(src, filepath.Join(dstParent, "dst")))

	assert.ErrorIs(t, got, fs.ErrPermission)
	assert.Contains(t, got.Error(), dstParent)
	assert.Contains(t, got.Error(), fmt.Sprintf("uid %d", os.Geteuid()))
}
