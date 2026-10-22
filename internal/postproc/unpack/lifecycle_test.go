package unpack

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixtureSet(t testing.TB, names ...string) ArchiveSet {
	t.Helper()
	return detectOne(t, copyFixtures(t, "7z", names...))
}

func TestARerunClearsALeftoverStagingDir(t *testing.T) {
	o := newOut(t)
	stage := o.dst + ".tmp"
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "old", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "old", "deep", "junk"), []byte("crash leftover"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "foo"), []byte("stale"), 0o600))

	_, err := unpackSet(t, fixtureSet(t, "t0.7z"), o.dst)

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, listing(t, o.dst), "nothing of the old run survives")
	require.Equal(t, "foo\n", readFile(t, filepath.Join(o.dst, "foo")))
	require.Equal(t, []string{"out"}, children(t, o.parent))
}

func TestALeftoverStagingDirWithoutPermissionsIsCleared(t *testing.T) {
	o := newOut(t)
	stage := o.dst + ".tmp"
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "locked", "inner"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(stage, "locked"), 0))

	_, err := unpackSet(t, fixtureSet(t, "t0.7z"), o.dst)

	require.NoError(t, err)
	require.Equal(t, []string{"out"}, children(t, o.parent))
}

func TestARerunAfterASuccessfulOneIsRefusedUnlessReplaceIsSet(t *testing.T) {
	o := newOut(t)
	set := fixtureSet(t, "t0.7z")
	_, err := unpackSet(t, set, o.dst)
	require.NoError(t, err)

	_, err = unpackSet(t, set, o.dst)

	require.ErrorIs(t, err, ErrExists)
	require.Equal(t, []string{"bar", "foo"}, listing(t, o.dst), "the first result is untouched")
	require.Equal(t, []string{"out"}, children(t, o.parent))
}

func TestReplaceSwapsTheExistingDestination(t *testing.T) {
	o := newOut(t)
	require.NoError(t, os.MkdirAll(o.dst, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(o.dst, "stale"), []byte("x"), 0o600))

	_, err := unpackSet(t, fixtureSet(t, "t0.7z"), o.dst, WithReplace())

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, listing(t, o.dst))
	require.Equal(t, []string{"out"}, children(t, o.parent))
}

func TestAFailedReplaceKeepsTheExistingDestination(t *testing.T) {
	o := newOut(t)
	_, err := unpackSet(t, fixtureSet(t, "t0.7z"), o.dst)
	require.NoError(t, err)
	// A password-protected archive without the password fails.
	_, err = unpackSet(t, fixtureSet(t, "t3.7z"), o.dst, WithReplace())
	require.ErrorIs(t, err, ErrPassword)
	require.Equal(t, []string{"bar", "foo"}, listing(t, o.dst))
	require.Equal(t, []string{"out"}, children(t, o.parent))

	// A failure after the extraction (here: the byte cap seen by the walk
	// is not reachable, so use a hostile name) keeps it as well.
	j := newJail(t)
	arch := t.TempDir()
	writeZip(t, filepath.Join(arch, "evil.zip"), zipEnt{name: "g", data: "two"}, zipEnt{name: "../x", data: "evil"})
	_, err = unpackSet(t, oneSet(filepath.Join(arch, "evil.zip")), o.dst, WithReplace())
	require.ErrorIs(t, err, ErrPathViolation)
	require.Equal(t, []string{"bar", "foo"}, listing(t, o.dst))
	j.requireContained(t, false)
}

func TestALeftoverStagingSymlinkIsRemovedWithoutTouchingItsTarget(t *testing.T) {
	o := newOut(t)
	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "precious"), []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(victim, o.dst+".tmp"))

	_, err := unpackSet(t, fixtureSet(t, "t0.7z"), o.dst)

	require.NoError(t, err)
	require.Equal(t, []string{"precious"}, children(t, victim))
	require.Equal(t, []string{"bar", "foo"}, listing(t, o.dst))
}

func TestTheDestinationParentIsCreated(t *testing.T) {
	o := newOut(t)
	dst := filepath.Join(o.parent, "deep", "er", "out")

	_, err := unpackSet(t, fixtureSet(t, "t0.7z"), dst)

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, listing(t, dst))
}

func TestFailureLeavesTheParentDirectoryExactlyAsItWas(t *testing.T) {
	o := newOut(t)
	require.NoError(t, os.WriteFile(filepath.Join(o.parent, "bystander"), []byte("x"), 0o600))

	_, err := unpackSet(t, fixtureSet(t, "t3.7z"), o.dst)

	require.ErrorIs(t, err, ErrPassword)
	require.Equal(t, []string{"bystander"}, children(t, o.parent))
}

func TestANoSpaceLeftErrorIsDiskFull(t *testing.T) {
	require.ErrorIs(t, diskErr(&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}), ErrDiskFull)
	require.NoError(t, diskErr(nil))
	other := &os.PathError{Op: "write", Path: "x", Err: syscall.EACCES}
	require.NotErrorIs(t, diskErr(other), ErrDiskFull)
}

func TestTheArchivesAreNeverModified(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	before, err := os.ReadFile(filepath.Join(dir, "t0.7z"))
	require.NoError(t, err)

	_, err = unpackSet(t, detectOne(t, dir), newOut(t).dst)

	require.NoError(t, err)
	after, err := os.ReadFile(filepath.Join(dir, "t0.7z"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, []string{"t0.7z"}, children(t, dir), "7zz leaves nothing next to the archive")
}
