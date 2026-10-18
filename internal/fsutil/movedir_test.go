package fsutil_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// makeTree builds a small release-like tree and returns its root.
func makeTree(t *testing.T, parent string) string {
	t.Helper()
	src := filepath.Join(parent, "release")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "Subs", "empty"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("video-bytes"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "Subs", "en.srt"), []byte("subs"), 0o640))
	return src
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	assert.ErrorIs(t, err, os.ErrNotExist, path)
}

func TestMoveDir_SameFilesystemRenames(t *testing.T) {
	base := t.TempDir()
	src := makeTree(t, base)
	dst := filepath.Join(base, "complete", "tv", "release")

	res, err := fsutil.MoveDir(src, dst)
	require.NoError(t, err)

	assert.False(t, res.CrossDevice)
	assertMissing(t, src)
	assert.Equal(t, "video-bytes", readFile(t, filepath.Join(dst, "movie.mkv")))
	assert.Equal(t, "subs", readFile(t, filepath.Join(dst, "Subs", "en.srt")))
	assert.DirExists(t, filepath.Join(dst, "Subs", "empty"))
}

func TestMoveDir_RenameKeepsInodes(t *testing.T) {
	base := t.TempDir()
	src := makeTree(t, base)
	before, err := os.Stat(filepath.Join(src, "movie.mkv"))
	require.NoError(t, err)

	dst := filepath.Join(base, "out")
	_, err = fsutil.MoveDir(src, dst)
	require.NoError(t, err)

	after, err := os.Stat(filepath.Join(dst, "movie.mkv"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "rename must not copy")
}

func TestMoveDir_RefusesExistingDestination(t *testing.T) {
	for _, forceCross := range []bool{false, true} {
		name := "same device"
		if forceCross {
			name = "cross device"
		}
		t.Run(name, func(t *testing.T) {
			if forceCross {
				defer fsutil.ForceCrossDevice()()
			}
			base := t.TempDir()
			src := makeTree(t, base)

			emptyDst := filepath.Join(base, "empty-dst")
			require.NoError(t, os.Mkdir(emptyDst, 0o755))
			_, err := fsutil.MoveDir(src, emptyDst)
			assert.ErrorIs(t, err, fsutil.ErrExists)

			fileDst := filepath.Join(base, "file-dst")
			require.NoError(t, os.WriteFile(fileDst, []byte("keep"), 0o644))
			_, err = fsutil.MoveDir(src, fileDst)
			assert.ErrorIs(t, err, fsutil.ErrExists)
			assert.Equal(t, "keep", readFile(t, fileDst))

			assert.Equal(t, "video-bytes", readFile(t, filepath.Join(src, "movie.mkv")))
		})
	}
}

func TestMoveDir_RejectsMissingOrNonDirectorySource(t *testing.T) {
	base := t.TempDir()
	_, err := fsutil.MoveDir(filepath.Join(base, "nope"), filepath.Join(base, "dst"))
	assert.ErrorIs(t, err, os.ErrNotExist)

	file := filepath.Join(base, "file")
	touch(t, file)
	_, err = fsutil.MoveDir(file, filepath.Join(base, "dst"))
	assert.Error(t, err)
	assertMissing(t, filepath.Join(base, "dst"))
}

func TestMoveDir_RejectsDestinationInsideSource(t *testing.T) {
	for _, forceCross := range []bool{false, true} {
		name := "same device"
		if forceCross {
			name = "cross device"
		}
		t.Run(name, func(t *testing.T) {
			if forceCross {
				defer fsutil.ForceCrossDevice()()
			}
			base := t.TempDir()
			src := makeTree(t, base)

			_, err := fsutil.MoveDir(src, filepath.Join(src, "Subs", "inner"))

			assert.ErrorIs(t, err, fsutil.ErrNested)
			assertMissing(t, filepath.Join(src, "Subs", "inner"))
		})
	}
}

func TestMoveDir_OtherRenameErrorsAreReturnedWithoutCopying(t *testing.T) {
	boom := errors.New("boom")
	defer fsutil.SetRename(func(_, _ string) error { return boom })()
	base := t.TempDir()
	src := makeTree(t, base)
	dst := filepath.Join(base, "dst")

	_, err := fsutil.MoveDir(src, dst)
	assert.ErrorIs(t, err, boom)
	assertMissing(t, dst)
	assert.FileExists(t, filepath.Join(src, "movie.mkv"))
}

func TestMoveDir_CrossDeviceCopiesThenRemovesSource(t *testing.T) {
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	src := makeTree(t, base)
	dst := filepath.Join(base, "complete", "tv", "release")

	res, err := fsutil.MoveDir(src, dst)
	require.NoError(t, err)

	assert.True(t, res.CrossDevice)
	assertMissing(t, src)
	assert.Equal(t, "video-bytes", readFile(t, filepath.Join(dst, "movie.mkv")))
	assert.Equal(t, "subs", readFile(t, filepath.Join(dst, "Subs", "en.srt")))
	assert.DirExists(t, filepath.Join(dst, "Subs", "empty"))
}

func TestMoveDir_CrossDevicePreservesModesAndModTimes(t *testing.T) {
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	src := makeTree(t, base)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, os.Chmod(filepath.Join(src, "movie.mkv"), 0o600))
	require.NoError(t, os.Chmod(filepath.Join(src, "Subs", "en.srt"), 0o640))
	require.NoError(t, os.Chtimes(filepath.Join(src, "movie.mkv"), old, old))
	require.NoError(t, os.Chmod(filepath.Join(src, "Subs"), 0o750))
	exec := filepath.Join(src, "run.sh")
	require.NoError(t, os.WriteFile(exec, []byte("#!/bin/sh"), 0o755))
	require.NoError(t, os.Chmod(exec, 0o755)) // defeat umask
	dst := filepath.Join(base, "dst")

	_, err := fsutil.MoveDir(src, dst)
	require.NoError(t, err)

	if runtime.GOOS != "windows" { // windows only models the write bit
		modeOf := func(rel string) os.FileMode {
			fi, err := os.Stat(filepath.Join(dst, rel))
			require.NoError(t, err)
			return fi.Mode().Perm()
		}
		assert.Equal(t, os.FileMode(0o600), modeOf("movie.mkv"))
		assert.Equal(t, os.FileMode(0o640), modeOf(filepath.Join("Subs", "en.srt")))
		assert.Equal(t, os.FileMode(0o755), modeOf("run.sh"))
		assert.Equal(t, os.FileMode(0o750), modeOf("Subs"))
	}
	fi, err := os.Stat(filepath.Join(dst, "movie.mkv"))
	require.NoError(t, err)
	assert.True(t, fi.ModTime().Equal(old), "mod time %v", fi.ModTime())
}

func TestMoveDir_CrossDeviceCopiesReadOnlyDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions are not modelled on windows")
	}
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	src := makeTree(t, base)
	require.NoError(t, os.Chmod(filepath.Join(src, "Subs"), 0o555))
	dst := filepath.Join(base, "dst")
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dst, "Subs"), 0o755) })

	_, err := fsutil.MoveDir(src, dst)
	require.NoError(t, err)

	fi, err := os.Stat(filepath.Join(dst, "Subs"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o555), fi.Mode().Perm())
	assert.Equal(t, "subs", readFile(t, filepath.Join(dst, "Subs", "en.srt")))
	assertMissing(t, src)
}

func TestMoveDir_CrossDeviceRefusesSymlinksAndLeavesSourceIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	src := makeTree(t, base)
	secret := filepath.Join(base, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(src, "zz-link")))
	require.NoError(t, os.Symlink(base, filepath.Join(src, "Subs", "dirlink")))
	dst := filepath.Join(base, "out", "dst")

	_, err := fsutil.MoveDir(src, dst)

	assert.ErrorIs(t, err, fsutil.ErrSymlink)
	assertMissing(t, dst)
	assert.Equal(t, "video-bytes", readFile(t, filepath.Join(src, "movie.mkv")))
	assert.Equal(t, "subs", readFile(t, filepath.Join(src, "Subs", "en.srt")))
	assert.Equal(t, "secret", readFile(t, secret))
	_, err = os.Lstat(filepath.Join(src, "zz-link"))
	assert.NoError(t, err)
}

func TestMoveDir_CrossDeviceRefusesSymlinkedSourceRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	real := makeTree(t, base)
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))

	_, err := fsutil.MoveDir(link, filepath.Join(base, "dst"))

	assert.ErrorIs(t, err, fsutil.ErrSymlink)
	assertMissing(t, filepath.Join(base, "dst"))
	assert.FileExists(t, filepath.Join(real, "movie.mkv"))
}

func TestMoveDir_CrossDeviceFailureRemovesPartialCopyAndKeepsSource(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unreadable files: not available on windows or as root")
	}
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	src := makeTree(t, base)
	locked := filepath.Join(src, "zz-locked.bin")
	require.NoError(t, os.WriteFile(locked, []byte("locked"), 0o600))
	require.NoError(t, os.Chmod(locked, 0))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	dst := filepath.Join(base, "dst")

	_, err := fsutil.MoveDir(src, dst)

	require.Error(t, err)
	assertMissing(t, dst)
	assert.Equal(t, "video-bytes", readFile(t, filepath.Join(src, "movie.mkv")))
	assert.Equal(t, "subs", readFile(t, filepath.Join(src, "Subs", "en.srt")))
	assert.FileExists(t, locked)
}

func TestMoveDir_CrossDeviceCopiesHardlinkedFilesAsSeparateFiles(t *testing.T) {
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	src := makeTree(t, base)
	require.NoError(t, os.Link(filepath.Join(src, "movie.mkv"), filepath.Join(src, "Subs", "movie-link.mkv")))
	dst := filepath.Join(base, "dst")

	_, err := fsutil.MoveDir(src, dst)
	require.NoError(t, err)

	a, err := os.Stat(filepath.Join(dst, "movie.mkv"))
	require.NoError(t, err)
	b, err := os.Stat(filepath.Join(dst, "Subs", "movie-link.mkv"))
	require.NoError(t, err)
	assert.False(t, os.SameFile(a, b), "hardlinks are not preserved across devices")
	assert.Equal(t, "video-bytes", readFile(t, filepath.Join(dst, "Subs", "movie-link.mkv")))
}

func TestMoveDir_CrossDeviceKeepsCompleteCopyWhenSourceCannotBeRemoved(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a read-only parent: not available on windows or as root")
	}
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	parent := filepath.Join(base, "incomplete")
	require.NoError(t, os.Mkdir(parent, 0o755))
	src := makeTree(t, parent)
	require.NoError(t, os.Chmod(parent, 0o555)) // src itself cannot be unlinked
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	dst := filepath.Join(base, "dst")

	res, err := fsutil.MoveDir(src, dst)

	require.Error(t, err)
	assert.True(t, res.CrossDevice)
	assert.Equal(t, "video-bytes", readFile(t, filepath.Join(dst, "movie.mkv")), "complete copy must be kept")
}

func TestMoveDir_RejectsDestinationInsideSourceViaSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	src := makeTree(t, base)
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Symlink(src, alias))

	_, err := fsutil.MoveDir(src, filepath.Join(alias, "Subs", "inner"))

	assert.ErrorIs(t, err, fsutil.ErrNested)
	assertMissing(t, filepath.Join(src, "Subs", "inner"))
}
