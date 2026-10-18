package unpack

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestARerunClearsALeftoverStagingDir(t *testing.T) {
	s := newSeam(t)
	stage := s.dst + ".tmp"
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "old", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "old", "deep", "junk"), []byte("crash leftover"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "f"), []byte("stale"), 0o600))

	_, err := s.run(t, opener(fileEntry("f", "fresh")))

	require.NoError(t, err)
	require.Equal(t, []string{"f"}, listing(t, s.dst), "nothing of the old run survives")
	require.Equal(t, []string{"out"}, children(t, s.parent))
}

func TestARerunAfterASuccessfulOneIsRefusedUnlessReplaceIsSet(t *testing.T) {
	s := newSeam(t)
	_, err := s.run(t, opener(fileEntry("f", "one")))
	require.NoError(t, err)

	_, err = s.run(t, opener(fileEntry("g", "two")))

	require.ErrorIs(t, err, ErrExists)
	require.Equal(t, []string{"f"}, listing(t, s.dst), "the first result is untouched")
	require.Equal(t, []string{"out"}, children(t, s.parent))
}

func TestReplaceSwapsTheExistingDestination(t *testing.T) {
	s := newSeam(t)
	_, err := s.run(t, opener(fileEntry("f", "one")))
	require.NoError(t, err)

	_, err = s.run(t, opener(fileEntry("g", "two")), WithReplace())

	require.NoError(t, err)
	require.Equal(t, []string{"g"}, listing(t, s.dst))
	require.Equal(t, []string{"out"}, children(t, s.parent))
}

func TestAFailedReplaceKeepsTheExistingDestination(t *testing.T) {
	s := newSeam(t)
	_, err := s.run(t, opener(fileEntry("f", "one")))
	require.NoError(t, err)

	_, err = s.run(t, opener(fileEntry("g", "two"), fileEntry("../x", "evil")), WithReplace())

	require.ErrorIs(t, err, ErrPathViolation)
	require.Equal(t, []string{"f"}, listing(t, s.dst))
	require.Equal(t, []string{"out"}, children(t, s.parent))
}

func TestALeftoverStagingSymlinkIsRemovedWithoutTouchingItsTarget(t *testing.T) {
	s := newSeam(t)
	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "precious"), []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(victim, s.dst+".tmp"))

	_, err := s.run(t, opener(fileEntry("f", "x")))

	require.NoError(t, err)
	require.Equal(t, []string{"precious"}, children(t, victim))
	require.Equal(t, []string{"f"}, listing(t, s.dst))
}

func TestTheDestinationParentIsCreated(t *testing.T) {
	s := newSeam(t)
	s.dst = filepath.Join(s.parent, "deep", "er", "out")

	_, err := s.run(t, opener(fileEntry("f", "x")))

	require.NoError(t, err)
	require.Equal(t, []string{"f"}, listing(t, s.dst))
}

func TestAnErrorFromTheSourceMidwayCleansUp(t *testing.T) {
	s := newSeam(t)
	boom := errors.New("boom")

	_, err := s.run(t, func() (source, error) {
		return &failingSource{entries: []*entry{fileEntry("a", "x"), fileEntry("b", "y")}, after: 1, err: boom}, nil
	})

	// Pass 1 already hits the error, before anything is written.
	require.ErrorIs(t, err, boom)
	require.Empty(t, children(t, s.parent))
}

type failingSource struct {
	entries []*entry
	i       int
	after   int
	err     error
}

func (f *failingSource) next() (*entry, error) {
	if f.i >= f.after {
		return nil, f.err
	}
	e := f.entries[f.i]
	f.i++
	return e, nil
}

func (f *failingSource) close() error { return nil }

func TestAFailureToOpenTheArchiveCleansUp(t *testing.T) {
	s := newSeam(t)
	boom := errors.New("cannot open")

	_, err := s.run(t, func() (source, error) { return nil, boom })

	require.ErrorIs(t, err, boom)
	require.Empty(t, children(t, s.parent))
}

func TestAReadErrorMidFileIsCorruptAndCleansUp(t *testing.T) {
	s := newSeam(t)
	calls := 0
	r := readerFunc(func(p []byte) (int, error) {
		calls++
		if calls == 1 {
			return copy(p, "partial"), nil
		}
		return 0, errors.New("decoder exploded")
	})

	_, err := s.run(t, opener(streamEntry("f", -1, r)))

	require.ErrorIs(t, err, ErrCorrupt)
	require.Empty(t, children(t, s.parent))
}

func TestClassifiedReadErrorsKeepTheirClass(t *testing.T) {
	for _, want := range []error{ErrPassword, ErrUnsupported, ErrCorrupt} {
		s := newSeam(t)
		r := readerFunc(func([]byte) (int, error) { return 0, want })

		_, err := s.run(t, opener(streamEntry("f", -1, r)))

		require.ErrorIs(t, err, want)
		if want != ErrCorrupt {
			require.NotErrorIs(t, err, ErrCorrupt)
		}
	}
}

func TestEntryContentOpenFailureCleansUp(t *testing.T) {
	s := newSeam(t)
	e := fileEntry("f", "x")
	e.open = func() (io.Reader, error) { return nil, ErrPassword }

	_, err := s.run(t, opener(e))

	require.ErrorIs(t, err, ErrPassword)
	require.Empty(t, children(t, s.parent))
}

func TestFailureLeavesTheParentDirectoryExactlyAsItWas(t *testing.T) {
	s := newSeam(t)
	require.NoError(t, os.WriteFile(filepath.Join(s.parent, "sibling.txt"), []byte("mine"), 0o600))
	before := listing(t, s.parent)

	_, err := s.run(t, opener(
		dirEntry("a"), fileEntry("a/b", "1"), fileEntry("a/../../escape", "2"),
	))

	require.ErrorIs(t, err, ErrPathViolation)
	require.Equal(t, before, listing(t, s.parent))
}

func TestAnEmptyArchiveProducesAnEmptyDestination(t *testing.T) {
	s := newSeam(t)

	res, err := s.run(t, opener())

	require.NoError(t, err)
	require.Empty(t, res.Files)
	require.DirExists(t, s.dst)
	require.Empty(t, children(t, s.dst))
}

func TestAContextCancelledBeforeTheStartLeavesNothing(t *testing.T) {
	s := newSeam(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := unpackWith(ctx, s.dst, opener(fileEntry("f", "x")), 1, newConfig([]Option{WithLogger(nil)}))

	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, children(t, s.parent))
}

func TestANoSpaceLeftWriteErrorIsDiskFull(t *testing.T) {
	err := diskErr(&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC})

	require.ErrorIs(t, err, ErrDiskFull)
	require.ErrorIs(t, err, ErrLimit)
	require.NoError(t, diskErr(nil))
	other := errors.New("other")
	require.Equal(t, other, diskErr(other))
}

func TestADecoderPanicBecomesACorruptArchive(t *testing.T) {
	err := guarded(func() error { panic("decoder bug") })

	require.ErrorIs(t, err, ErrCorrupt)
	require.NoError(t, guarded(func() error { return nil }))
	boom := errors.New("boom")
	require.Equal(t, boom, guarded(func() error { return boom }))
}
