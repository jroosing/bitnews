package unpack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// zeros yields n zero bytes (or forever when n < 0).
func zeros(n int64) io.Reader {
	return readerFunc(func(p []byte) (int, error) {
		if n == 0 {
			return 0, io.EOF
		}
		k := int64(len(p))
		if n > 0 && k > n {
			k = n
		}
		clear(p[:k])
		if n > 0 {
			n -= k
		}
		return int(k), nil
	})
}

func streamEntry(name string, declared int64, r io.Reader) *entry {
	return &entry{name: name, kind: kindFile, size: declared, open: func() (io.Reader, error) { return r, nil }}
}

func TestALyingSizeHeaderIsCorrupt(t *testing.T) {
	cases := map[string]struct {
		declared int64
		actual   int64
	}{
		"far larger than declared":  {declared: 10, actual: 10 << 20},
		"just past the tolerance":   {declared: 100_000, actual: 100_000 + 1000 + 1024 + 1},
		"smaller than declared":     {declared: 1000, actual: 10},
		"within tolerance but != ":  {declared: 100_000, actual: 100_500},
		"declared zero, has bytes":  {declared: 0, actual: 5000},
		"declared set, nothing got": {declared: 1, actual: 0},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			s := newSeam(t)

			_, err := s.run(t, opener(streamEntry("f", c.declared, zeros(c.actual))))

			require.ErrorIs(t, err, ErrCorrupt)
			require.Empty(t, children(t, s.parent))
		})
	}
}

func TestWritingStopsAtTheDeclaredSizePlusOnePercentAndAKiB(t *testing.T) {
	s := newSeam(t)
	var peak int64
	// The writer's view: watch the staging file grow while the endless stream
	// is read, and make sure it never gets far past the limit.
	endless := readerFunc(func(p []byte) (int, error) {
		if fi, err := os.Stat(filepath.Join(s.dst+".tmp", "f")); err == nil {
			peak = max(peak, fi.Size())
		}
		clear(p)
		return len(p), nil
	})

	_, err := s.run(t, opener(streamEntry("f", 5<<20, endless)))

	require.ErrorIs(t, err, ErrCorrupt)
	limit := int64(5<<20) + (5<<20)/100 + 1024
	require.LessOrEqual(t, peak, limit)
}

func TestAnUnknownSizeIsOnlyBoundByTheTotalGuards(t *testing.T) {
	s := newSeam(t)

	res, err := s.run(t, opener(streamEntry("f", -1, zeros(3<<20))))

	require.NoError(t, err)
	require.EqualValues(t, 3<<20, res.Bytes)
}

func TestACompressionBombIsAborted(t *testing.T) {
	cases := map[string]struct {
		declared int64
	}{
		"size unknown":        {declared: -1},
		"size honestly given": {declared: 8 << 20},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			s := newSeam(t)

			// 8 MiB from a 4 KiB archive is 2048:1; the threshold is lowered
			// from 1 GiB so the test does not write a gigabyte.
			_, err := s.runPacked(t, 4096, opener(streamEntry("f", c.declared, zeros(8<<20))), func(c *config) { c.bombMin = 1 << 20 })

			require.ErrorIs(t, err, ErrBomb)
			require.ErrorIs(t, err, ErrLimit)
			require.Empty(t, children(t, s.parent))
		})
	}
}

func TestBombRatioIsMeasuredAcrossAllFilesOfTheArchive(t *testing.T) {
	s := newSeam(t)

	_, err := s.runPacked(t, 1000, opener(
		streamEntry("a", -1, zeros(40_000)),
		streamEntry("b", -1, zeros(40_000)),
		streamEntry("c", -1, zeros(40_000)),
	), func(c *config) { c.bombMin = 100_000 })

	require.ErrorIs(t, err, ErrBomb, "120 kB from a 1 kB archive is 120:1 in total")
}

func TestOutputBelowTheBombThresholdOrRatioIsFine(t *testing.T) {
	t.Run("high ratio but small output", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.runPacked(t, 10, opener(streamEntry("f", -1, zeros(20<<20))))
		require.NoError(t, err, "default threshold is 1 GiB")
	})
	t.Run("large output at a sane ratio", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.runPacked(t, 1<<20, opener(streamEntry("f", -1, zeros(50<<20))), func(c *config) { c.bombMin = 1 << 20 })
		require.NoError(t, err, "50:1 is below 100:1")
	})
}

func TestTheDefaultLimitsAreTheDocumentedOnes(t *testing.T) {
	c := newConfig(nil)
	require.EqualValues(t, 1<<30, c.bombMin)
	require.EqualValues(t, 100, c.bombRatio)
	require.Equal(t, 1_000_000, c.maxEntries)
	require.Zero(t, c.maxBytes)
	require.Zero(t, c.minFree)
}

func TestTooManyEntriesFailsBeforeAnythingIsWritten(t *testing.T) {
	s := newSeam(t)
	opens := 0
	f := func(name string) *entry {
		e := fileEntry(name, "x")
		inner := e.open
		e.open = func() (io.Reader, error) { opens++; return inner() }
		return e
	}

	_, err := s.run(t, opener(f("a"), f("b"), dirEntry("c"), f("d")), WithMaxEntries(3))

	require.ErrorIs(t, err, ErrTooManyEntries)
	require.ErrorIs(t, err, ErrLimit)
	require.Zero(t, opens)
	require.Empty(t, children(t, s.parent))
}

func TestEntriesAtTheCapAreAccepted(t *testing.T) {
	s := newSeam(t)

	_, err := s.run(t, opener(fileEntry("a", "x"), fileEntry("b", "x"), fileEntry("c", "x")), WithMaxEntries(3))

	require.NoError(t, err)
}

func TestSkippedEntriesCountTowardTheEntryCap(t *testing.T) {
	s := newSeam(t)

	_, err := s.run(t, opener(otherEntry("a", kindSymlink), otherEntry("b", kindSymlink), otherEntry("c", kindSymlink)), WithMaxEntries(2))

	require.ErrorIs(t, err, ErrTooManyEntries)
}

func TestDeclaredSizeMustFitIntoFreeDiskSpace(t *testing.T) {
	free := func(n uint64) Option {
		return WithFreeSpace(func(string) (uint64, error) { return n, nil })
	}
	entries := func() openFunc {
		return opener(fileEntry("a", "0123456789"), fileEntry("b", "0123456789"))
	}
	t.Run("does not fit", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, entries(), free(19))
		require.ErrorIs(t, err, ErrDiskFull)
		require.ErrorIs(t, err, ErrLimit)
		require.Empty(t, children(t, s.parent))
	})
	t.Run("fits exactly", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, entries(), free(20))
		require.NoError(t, err)
	})
	t.Run("min free is kept back", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, entries(), free(100), WithMinFree(81))
		require.ErrorIs(t, err, ErrDiskFull)
	})
	t.Run("min free alone leaves room", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, entries(), free(100), WithMinFree(80))
		require.NoError(t, err)
	})
	t.Run("less free than min free", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, opener(), free(10), WithMinFree(50))
		require.ErrorIs(t, err, ErrDiskFull)
	})
	t.Run("a failing probe disables the check", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, entries(), WithFreeSpace(func(string) (uint64, error) { return 0, errors.New("no statfs") }))
		require.NoError(t, err)
	})
	t.Run("the probe is asked about the destination's file system", func(t *testing.T) {
		s := newSeam(t)
		var asked string
		_, err := s.run(t, entries(), WithFreeSpace(func(p string) (uint64, error) { asked = p; return 1 << 40, nil }))
		require.NoError(t, err)
		require.Equal(t, s.parent, asked)
	})
}

func TestTheDefaultFreeSpaceProbeWorks(t *testing.T) {
	n, err := freeSpace(t.TempDir())

	require.NoError(t, err)
	require.NotZero(t, n)
}

func TestTheByteCapAppliesToDeclaredAndActualSizes(t *testing.T) {
	t.Run("declared total", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, opener(fileEntry("a", "123456"), fileEntry("b", "123456")), WithMaxBytes(11))
		require.ErrorIs(t, err, ErrTooLarge)
		require.Empty(t, children(t, s.parent))
	})
	t.Run("actual bytes behind an unknown size", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, opener(streamEntry("a", -1, zeros(1<<20))), WithMaxBytes(1<<19))
		require.ErrorIs(t, err, ErrTooLarge)
		require.Empty(t, children(t, s.parent))
	})
	t.Run("exactly at the cap", func(t *testing.T) {
		s := newSeam(t)
		_, err := s.run(t, opener(fileEntry("a", "12345"), fileEntry("b", "123456")), WithMaxBytes(11))
		require.NoError(t, err)
	})
}

func TestACancelledContextStopsBetweenEntriesAndCleansUp(t *testing.T) {
	s := newSeam(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancelling := fileEntry("a", "x")
	inner := cancelling.open
	cancelling.open = func() (io.Reader, error) { cancel(); return inner() }

	_, err := unpackWith(ctx, s.dst, opener(cancelling, fileEntry("b", "y")), 1, newConfig([]Option{WithLogger(nil)}))

	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, children(t, s.parent))
}

func TestACancelledContextStopsInTheMiddleOfAFileAndCleansUp(t *testing.T) {
	s := newSeam(t)
	ctx, cancel := context.WithCancel(context.Background())
	chunks := 0
	endless := readerFunc(func(p []byte) (int, error) {
		chunks++
		if chunks == 3 {
			cancel()
		}
		return len(p), nil
	})

	_, err := unpackWith(ctx, s.dst, opener(streamEntry("big", -1, endless)), 1<<40, newConfig([]Option{WithLogger(nil)}))

	require.ErrorIs(t, err, context.Canceled)
	require.LessOrEqual(t, chunks, 4, "stops within a chunk of the cancel")
	require.Empty(t, children(t, s.parent))
}

func TestCopyReadsInOneMiBChunks(t *testing.T) {
	s := newSeam(t)
	largest := 0
	r := readerFunc(func(p []byte) (int, error) {
		largest = max(largest, len(p))
		return 0, io.EOF
	})

	_, err := s.run(t, opener(streamEntry("f", -1, r)))

	require.NoError(t, err)
	require.Equal(t, 1<<20, largest)
}

func TestARelativeDestinationWorks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, err := unpackWith(context.Background(), "out", opener(fileEntry("f", "x")), 1, newConfig([]Option{WithLogger(nil)}))

	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(dir, "out", "f"))
	require.NoError(t, err)
	require.True(t, bytes.Equal([]byte("x"), got))
}
