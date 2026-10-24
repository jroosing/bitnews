package filewriter_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/usenet/filewriter"
)

func TestSnapshotRestoreRoundTripResumesInPlace(t *testing.T) {
	w, dir := newWriter(t)
	dataA, dataB := pattern(1000), pattern(700)
	partsA := splitParts("a.bin", dataA, 100)
	partsB := splitParts("b.bin", dataB, 100)
	for _, i := range []int{0, 1, 5, 9} {
		require.NoError(t, w.WritePart(0, partsA[i]))
	}
	require.NoError(t, w.WritePart(0, corrupt(partsA[3])))
	require.NoError(t, w.MarkMissing(0, filewriter.Range{Start: 600, End: 700}))
	require.NoError(t, w.WritePart(4, partsB[2]))
	want0, _ := w.Ranges(0)
	want4, _ := w.Ranges(4)

	snap, err := w.Snapshot()
	require.NoError(t, err)
	require.NoError(t, w.Close())

	w2, err := filewriter.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w2.Close() })
	reset, err := w2.Restore(snap)

	require.NoError(t, err)
	require.Empty(t, reset)
	got0, ok := w2.Ranges(0)
	require.True(t, ok)
	require.Equal(t, want0, got0)
	got4, _ := w2.Ranges(4)
	require.Equal(t, want4, got4)

	for _, p := range partsA {
		require.NoError(t, w2.WritePart(0, p))
	}
	for _, p := range partsB {
		require.NoError(t, w2.WritePart(4, p))
	}
	require.Equal(t, dataA, readFile(t, w2, dir, 0))
	require.Equal(t, dataB, readFile(t, w2, dir, 4))
	require.Equal(t, []string{"a.bin", "b.bin"}, listDir(t, dir), "names are reused, not re-uniquified")
	st, _ := w2.Ranges(0)
	require.True(t, st.Complete())
}

func TestSnapshotOfEmptyWriterRoundTrips(t *testing.T) {
	w, dir := newWriter(t)
	snap, err := w.Snapshot()
	require.NoError(t, err)

	w2, err := filewriter.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w2.Close() })
	reset, err := w2.Restore(snap)
	require.NoError(t, err)
	require.Empty(t, reset)
}

func TestRestoreResetsMissingAndTruncatedFiles(t *testing.T) {
	w, dir := newWriter(t)
	for k, name := range []string{"keep.bin", "short.bin", "gone.bin", "long.bin"} {
		data := pattern(400)
		for _, p := range splitParts(name, data, 100)[:2] {
			require.NoError(t, w.WritePart(filewriter.FileKey(k), p))
		}
	}
	snap, err := w.Snapshot()
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, os.Truncate(filepath.Join(dir, "short.bin"), 150))
	require.NoError(t, os.Remove(filepath.Join(dir, "gone.bin")))
	require.NoError(t, os.Truncate(filepath.Join(dir, "long.bin"), 900))

	w2, err := filewriter.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w2.Close() })
	reset, err := w2.Restore(snap)

	require.NoError(t, err)
	require.Equal(t, []filewriter.FileKey{1, 2, 3}, reset)
	keep, ok := w2.Ranges(0)
	require.True(t, ok)
	require.Equal(t, []filewriter.Range{{Start: 0, End: 200}}, keep.Done)
	for _, k := range []filewriter.FileKey{1, 3} {
		st, ok := w2.Ranges(k)
		require.True(t, ok, "key %d", k)
		require.Empty(t, st.Done)
		require.Equal(t, []filewriter.Range{{Start: 0, End: 400}}, st.Missing)
		fi, err := os.Stat(filepath.Join(dir, st.Name))
		require.NoError(t, err)
		require.Equal(t, int64(400), fi.Size(), "file is re-allocated to its recorded size")
	}
	_, ok = w2.Ranges(2)
	require.False(t, ok, "a vanished file is forgotten")

	data := pattern(400)
	for _, p := range splitParts("gone.bin", data, 100) {
		require.NoError(t, w2.WritePart(2, p))
	}
	require.Equal(t, data, readFile(t, w2, dir, 2))
}

func TestRestoreRefusesNonRegularFileAtRecordedName(t *testing.T) {
	w, dir := newWriter(t)
	require.NoError(t, w.WritePart(0, splitParts("x.bin", pattern(100), 50)[0]))
	snap, err := w.Snapshot()
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, os.Remove(filepath.Join(dir, "x.bin")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "x.bin"), 0o777))

	w2, err := filewriter.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w2.Close() })
	reset, err := w2.Restore(snap)

	require.NoError(t, err)
	require.Equal(t, []filewriter.FileKey{0}, reset)
	_, ok := w2.Ranges(0)
	require.False(t, ok)
}

func TestRestoreRejectsMalformedSnapshots(t *testing.T) {
	w, dir := newWriter(t)
	require.NoError(t, w.WritePart(0, splitParts("x.bin", pattern(100), 50)[0]))
	snap, err := w.Snapshot()
	require.NoError(t, err)

	flipped := append([]byte(nil), snap...)
	flipped[len(flipped)/2] ^= 0xFF
	badVersion := append([]byte(nil), snap...)
	badVersion[4] = 99
	cases := map[string][]byte{
		"nil":         nil,
		"garbage":     []byte("hello world, not a snapshot"),
		"truncated":   snap[:len(snap)-3],
		"bit flip":    flipped,
		"bad version": badVersion,
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			w2, err := filewriter.New(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = w2.Close() })
			_, err = w2.Restore(b)
			require.ErrorIs(t, err, filewriter.ErrInvalidSnapshot)
			_, ok := w2.Ranges(0)
			require.False(t, ok)
		})
	}
}

func TestRestoreRequiresEmptyWriter(t *testing.T) {
	w, _ := newWriter(t)
	require.NoError(t, w.WritePart(0, splitParts("x.bin", pattern(100), 50)[0]))
	snap, err := w.Snapshot()
	require.NoError(t, err)

	_, err = w.Restore(snap)

	require.ErrorIs(t, err, filewriter.ErrNotEmpty)
}

func TestSnapshotIsCompact(t *testing.T) {
	w, _ := newWriter(t)
	data := pattern(100_000)
	parts := splitParts("big.bin", data, 100)
	for i, p := range parts {
		if i%10 != 3 { // 10% gaps: 100 separate ranges
			require.NoError(t, w.WritePart(0, p))
		}
	}

	snap, err := w.Snapshot()

	require.NoError(t, err)
	require.Less(t, len(snap), 400, "about 3 bytes per range plus header")
}
