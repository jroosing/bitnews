package unpack

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/tool/tooltest"
)

// t0.7z holds two 4-byte files, bar and foo.

func TestTheDefaultLimitsAreTheDocumentedOnes(t *testing.T) {
	c := newConfig(nil)

	require.Equal(t, 1_000_000, c.maxEntries)
	require.EqualValues(t, 1<<30, c.bombMin)
	require.EqualValues(t, 100, c.bombRatio)
	require.Zero(t, c.maxBytes, "no byte cap unless asked for")
}

func TestTooManyEntriesFailsBeforeAnythingIsWritten(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	o := newOut(t)

	_, err := unpackSet(t, detectOne(t, dir), o.dst, WithMaxEntries(1))

	require.ErrorIs(t, err, ErrTooManyEntries)
	require.ErrorIs(t, err, ErrLimit)
	require.Empty(t, children(t, o.parent))
}

func TestEntriesAtTheCapAreAccepted(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")

	_, err := unpackSet(t, detectOne(t, dir), newOut(t).dst, WithMaxEntries(2))

	require.NoError(t, err)
}

func TestTheByteCapAppliesToTheDeclaredSizes(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	set := detectOne(t, dir)

	t.Run("over", func(t *testing.T) {
		o := newOut(t)
		_, err := unpackSet(t, set, o.dst, WithMaxBytes(7))
		require.ErrorIs(t, err, ErrTooLarge)
		require.ErrorIs(t, err, ErrLimit)
		require.Empty(t, children(t, o.parent))
	})
	t.Run("exactly at the cap", func(t *testing.T) {
		_, err := unpackSet(t, set, newOut(t).dst, WithMaxBytes(8))
		require.NoError(t, err)
	})
}

func TestDeclaredSizeMustFitIntoFreeDiskSpace(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	set := detectOne(t, dir)
	free := func(n uint64) Option {
		return WithFreeSpace(func(string) (uint64, error) { return n, nil })
	}
	t.Run("does not fit", func(t *testing.T) {
		o := newOut(t)
		_, err := unpackSet(t, set, o.dst, free(7))
		require.ErrorIs(t, err, ErrDiskFull)
		require.ErrorIs(t, err, ErrLimit)
		require.Empty(t, children(t, o.parent))
	})
	t.Run("fits exactly", func(t *testing.T) {
		_, err := unpackSet(t, set, newOut(t).dst, free(8))
		require.NoError(t, err)
	})
	t.Run("min free is kept back", func(t *testing.T) {
		_, err := unpackSet(t, set, newOut(t).dst, free(100), WithMinFree(93))
		require.ErrorIs(t, err, ErrDiskFull)
	})
	t.Run("min free alone leaves room", func(t *testing.T) {
		_, err := unpackSet(t, set, newOut(t).dst, free(100), WithMinFree(92))
		require.NoError(t, err)
	})
	t.Run("less free than min free", func(t *testing.T) {
		_, err := unpackSet(t, set, newOut(t).dst, free(10), WithMinFree(50))
		require.ErrorIs(t, err, ErrDiskFull)
	})
	t.Run("a failing probe disables the check", func(t *testing.T) {
		_, err := unpackSet(t, set, newOut(t).dst, WithFreeSpace(func(string) (uint64, error) { return 0, errors.New("no statfs") }))
		require.NoError(t, err)
	})
	t.Run("the probe is asked about the destination's file system", func(t *testing.T) {
		o := newOut(t)
		var asked string
		_, err := unpackSet(t, set, o.dst, WithFreeSpace(func(p string) (uint64, error) { asked = p; return 1 << 40, nil }))
		require.NoError(t, err)
		require.Equal(t, o.parent, asked)
	})
}

func TestTheDefaultFreeSpaceProbeWorks(t *testing.T) {
	n, err := newConfig(nil).freeSpace(t.TempDir())

	require.NoError(t, err)
	require.NotZero(t, n)
}

func TestARelativeDestinationWorks(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	set := detectOne(t, dir)
	work := t.TempDir()
	z := tooltest.Path(t, "7zz") // found from the module root, so before the Chdir
	t.Chdir(work)

	_, err := Unpack(t.Context(), set, "out", WithSevenZip(z), WithLogger(nil))

	require.NoError(t, err)
	require.Equal(t, "foo\n", readFile(t, filepath.Join(work, "out", "foo")))
}

func TestARelativeVolumePathWorks(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	z := tooltest.Path(t, "7zz")
	t.Chdir(dir)

	res, err := Unpack(t.Context(), ArchiveSet{Format: Format7z, Name: "t0", Volumes: []string{"t0.7z"}}, filepath.Join(t.TempDir(), "out"), WithSevenZip(z), WithLogger(nil))

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Files)
}

func TestAnEmptyArchiveProducesAnEmptyDestination(t *testing.T) {
	src := t.TempDir()
	arch := t.TempDir()
	// 7zz refuses to create an empty archive; an archive of one empty
	// directory has no files.
	require.NoError(t, os.Mkdir(filepath.Join(src, "d"), 0o755))
	run7zz(t, src, "a", "-bd", filepath.Join(arch, "empty.7z"), "d")
	o := newOut(t)

	res, err := unpackSet(t, oneSet(filepath.Join(arch, "empty.7z")), o.dst)

	require.NoError(t, err)
	require.Empty(t, res.Files)
	require.Equal(t, []string{"d"}, listing(t, o.dst))
}
