package filewriter_test

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/usenet/filewriter"
	"gitlab.com/kyura4k/gitnews/internal/usenet/yenc"
)

// splitParts cuts data into parts of at most partSize bytes, as a yEnc
// encoder would, with a matching announced CRC32.
func splitParts(name string, data []byte, partSize int) []yenc.Part {
	var parts []yenc.Part
	total := (len(data) + partSize - 1) / partSize
	for i := 0; i*partSize < len(data); i++ {
		lo := i * partSize
		hi := min(lo+partSize, len(data))
		sum := crc32.ChecksumIEEE(data[lo:hi])
		parts = append(parts, yenc.Part{
			Name:           name,
			Size:           int64(len(data)),
			PartNumber:     i + 1,
			TotalParts:     total,
			Begin:          int64(lo) + 1,
			End:            int64(hi),
			Data:           data[lo:hi],
			CRC32:          sum,
			ExpectedCRC32:  sum,
			HasExpectedCRC: true,
		})
	}
	return parts
}

func newWriter(t *testing.T, opts ...filewriter.Option) (*filewriter.Writer, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "incomplete", "dl")
	w, err := filewriter.New(dir, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	return w, dir
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

func TestWritePartSinglePartFileIsByteExact(t *testing.T) {
	w, dir := newWriter(t)
	data := pattern(1000)

	for _, p := range splitParts("movie.mkv", data, 1000) {
		require.NoError(t, w.WritePart(0, p))
	}

	got, err := os.ReadFile(filepath.Join(dir, "movie.mkv"))
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestWritePartOutOfOrderParallelFilesAreByteExact(t *testing.T) {
	w, dir := newWriter(t)
	rng := rand.New(rand.NewPCG(1, 2))
	const nfiles = 6
	contents := make([][]byte, nfiles)
	var wg sync.WaitGroup
	errs := make(chan error, nfiles)
	for k := range nfiles {
		contents[k] = pattern(5000 + k*777)
		parts := splitParts(fmt.Sprintf("f%d.bin", k), contents[k], 97)
		rng.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
		wg.Go(func() {
			for _, p := range parts {
				if err := w.WritePart(filewriter.FileKey(k), p); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	for k := range nfiles {
		got, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%d.bin", k)))
		require.NoError(t, err)
		require.Equal(t, contents[k], got, "file %d", k)
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestWritePartHostileNamesStayInsideDir(t *testing.T) {
	long := strings.Repeat("a", 600) + ".rar"
	names := []string{
		"../../etc/passwd", "/etc/passwd", `C:\Windows\evil.dll`, "a\x00b.txt",
		"con.txt", long, "", "..", ".", "\u202eexe.txt", "tab\there\n.bin",
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "dl")
	w, err := filewriter.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	for i, name := range names {
		data := pattern(50)
		for _, p := range splitParts(name, data, 50) {
			require.NoError(t, w.WritePart(filewriter.FileKey(i), p), "name %q", name)
		}
		st, ok := w.Ranges(filewriter.FileKey(i))
		require.True(t, ok)
		require.NotContains(t, st.Name, "/")
		require.NotEqual(t, "..", st.Name)
		require.NotEqual(t, ".", st.Name)
		require.LessOrEqual(t, len(st.Name), 255)
		got, err := os.ReadFile(filepath.Join(dir, st.Name))
		require.NoError(t, err, "name %q -> %q", name, st.Name)
		require.Equal(t, data, got)
	}

	require.Len(t, listDir(t, dir), len(names))
	require.Equal(t, []string{"dl"}, listDir(t, parent), "nothing may appear next to the dir")
	_ = filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel, err := filepath.Rel(dir, p)
		require.NoError(t, err)
		if p != parent {
			require.False(t, strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..", p)
		}
		return nil
	})
}

func TestWritePartSameNameForTwoKeysGetsDistinctFiles(t *testing.T) {
	w, dir := newWriter(t)
	a, b := pattern(100), append(pattern(100)[1:], 9)
	require.NoError(t, w.WritePart(0, splitParts("same.bin", a, 100)[0]))
	require.NoError(t, w.WritePart(1, splitParts("same.bin", b, 100)[0]))

	s0, _ := w.Ranges(0)
	s1, _ := w.Ranges(1)
	require.NotEqual(t, s0.Name, s1.Name)
	got0, _ := os.ReadFile(filepath.Join(dir, s0.Name))
	got1, _ := os.ReadFile(filepath.Join(dir, s1.Name))
	require.Equal(t, a, got0)
	require.Equal(t, b, got1)
}

func TestWritePartDoesNotOverwritePreexistingFile(t *testing.T) {
	w, dir := newWriter(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.bin"), []byte("precious"), 0o666))

	require.NoError(t, w.WritePart(0, splitParts("x.bin", pattern(10), 10)[0]))

	got, err := os.ReadFile(filepath.Join(dir, "x.bin"))
	require.NoError(t, err)
	require.Equal(t, "precious", string(got))
	st, _ := w.Ranges(0)
	require.NotEqual(t, "x.bin", st.Name)
}

func TestWritePartDoesNotFollowSymlinkAtTargetName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	w, dir := newWriter(t)
	outside := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(outside, []byte("victim"), 0o666))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "x.bin")))
	dangling := filepath.Join(t.TempDir(), "not-there")
	require.NoError(t, os.Symlink(dangling, filepath.Join(dir, "y.bin")))

	require.NoError(t, w.WritePart(0, splitParts("x.bin", pattern(10), 10)[0]))
	require.NoError(t, w.WritePart(1, splitParts("y.bin", pattern(10), 10)[0]))

	got, _ := os.ReadFile(outside)
	require.Equal(t, "victim", string(got))
	require.NoFileExists(t, dangling)
}

func TestWritePartRejectsSizeMismatchBetweenParts(t *testing.T) {
	w, _ := newWriter(t)
	parts := splitParts("x.bin", pattern(200), 100)
	require.NoError(t, w.WritePart(0, parts[0]))

	bad := parts[1]
	bad.Size = 300
	err := w.WritePart(0, bad)

	require.ErrorIs(t, err, filewriter.ErrSizeMismatch)
	st, _ := w.Ranges(0)
	require.Equal(t, []filewriter.Range{{Start: 0, End: 100}}, st.Done)
}

func TestWritePartRejectsInvalidParts(t *testing.T) {
	base := splitParts("x.bin", pattern(200), 100)[0]
	tests := map[string]func(p *yenc.Part){
		"begin zero":       func(p *yenc.Part) { p.Begin = 0 },
		"begin negative":   func(p *yenc.Part) { p.Begin = -5 },
		"end past size":    func(p *yenc.Part) { p.Begin, p.End = 150, 249 },
		"end before begin": func(p *yenc.Part) { p.Begin, p.End = 50, 10 },
		"short data":       func(p *yenc.Part) { p.Data = p.Data[:99] },
		"long data":        func(p *yenc.Part) { p.Data = append(slices.Clone(p.Data), 0) },
		"size zero":        func(p *yenc.Part) { p.Size = 0 },
		"size negative":    func(p *yenc.Part) { p.Size = -1 },
		"size over 1 TiB":  func(p *yenc.Part) { p.Size = 1<<40 + 1 },
		"empty data":       func(p *yenc.Part) { p.Data, p.Begin, p.End = nil, 5, 4 },
		"huge range":       func(p *yenc.Part) { p.Begin, p.End = 1, math.MaxInt64 },
		"begin max":        func(p *yenc.Part) { p.Begin, p.End = math.MaxInt64, math.MaxInt64 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			w, dir := newWriter(t)
			p := base
			mutate(&p)

			err := w.WritePart(0, p)

			require.Error(t, err)
			require.Empty(t, listDir(t, dir), "an invalid first part must not create a file")
			_, ok := w.Ranges(0)
			require.False(t, ok)
		})
	}
}

func corrupt(p yenc.Part) yenc.Part {
	p.Data = bytes.Repeat([]byte{0xEE}, len(p.Data))
	p.CRC32 = crc32.ChecksumIEEE(p.Data) // announced CRC no longer matches
	return p
}

func readFile(t *testing.T, w *filewriter.Writer, dir string, key filewriter.FileKey) []byte {
	t.Helper()
	st, ok := w.Ranges(key)
	require.True(t, ok)
	b, err := os.ReadFile(filepath.Join(dir, st.Name))
	require.NoError(t, err)
	return b
}

func TestWritePartCRCMismatchIsDamagedThenHealedByGoodDuplicate(t *testing.T) {
	w, dir := newWriter(t)
	data := pattern(300)
	parts := splitParts("x.bin", data, 100)
	require.NoError(t, w.WritePart(0, parts[0]))
	require.NoError(t, w.WritePart(0, corrupt(parts[1])))
	require.NoError(t, w.WritePart(0, parts[2]))

	st, _ := w.Ranges(0)
	require.Equal(t, []filewriter.Range{{Start: 100, End: 200}}, st.Damaged)
	require.Equal(t, []filewriter.Range{{Start: 0, End: 100}, {Start: 200, End: 300}}, st.Done)
	require.Empty(t, st.Missing)
	require.False(t, st.Complete())
	require.Equal(t, bytes.Repeat([]byte{0xEE}, 100), readFile(t, w, dir, 0)[100:200], "best copy is kept")

	require.NoError(t, w.WritePart(0, parts[1]))

	st, _ = w.Ranges(0)
	require.Empty(t, st.Damaged)
	require.True(t, st.Complete())
	require.Equal(t, data, readFile(t, w, dir, 0))
}

func TestWritePartDamagedDuplicateNeverClobbersGoodPart(t *testing.T) {
	w, dir := newWriter(t)
	data := pattern(200)
	parts := splitParts("x.bin", data, 100)
	require.NoError(t, w.WritePart(0, parts[0]))
	require.NoError(t, w.WritePart(0, parts[1]))

	require.NoError(t, w.WritePart(0, corrupt(parts[0])))

	st, _ := w.Ranges(0)
	require.Empty(t, st.Damaged)
	require.True(t, st.Complete())
	require.Equal(t, data, readFile(t, w, dir, 0))
}

func TestWritePartDamagedOverlapOnlyWritesNonGoodBytes(t *testing.T) {
	w, dir := newWriter(t)
	data := pattern(100)
	good := splitParts("x.bin", data, 100)[0]
	good.Begin, good.End, good.Data = 21, 40, data[20:40] // good copy of [20,40)
	require.NoError(t, w.WritePart(0, good))

	bad := splitParts("x.bin", data, 100)[0] // damaged copy of [0,100)
	bad = corrupt(bad)
	require.NoError(t, w.WritePart(0, bad))

	st, _ := w.Ranges(0)
	require.Equal(t, []filewriter.Range{{Start: 20, End: 40}}, st.Done)
	require.Equal(t, []filewriter.Range{{Start: 0, End: 20}, {Start: 40, End: 100}}, st.Damaged)
	got := readFile(t, w, dir, 0)
	require.Equal(t, data[20:40], got[20:40])
	require.Equal(t, bytes.Repeat([]byte{0xEE}, 20), got[:20])
}

func TestWritePartWithoutAnnouncedCRCCountsAsGood(t *testing.T) {
	w, _ := newWriter(t)
	p := splitParts("x.bin", pattern(50), 50)[0]
	p.HasExpectedCRC = false

	require.NoError(t, w.WritePart(0, p))

	st, _ := w.Ranges(0)
	require.True(t, st.Complete())
}

func TestRangesComputesDoneDamagedMissingWithGapsAndOverlaps(t *testing.T) {
	w, _ := newWriter(t)
	data := pattern(1000)
	mk := func(begin, end int64) yenc.Part { // 1-based inclusive
		p := splitParts("x.bin", data, 1000)[0]
		p.Begin, p.End, p.Data = begin, end, data[begin-1:end]
		sum := crc32.ChecksumIEEE(p.Data)
		p.CRC32, p.ExpectedCRC32 = sum, sum
		return p
	}
	for _, p := range []yenc.Part{mk(101, 200), mk(151, 300), mk(201, 250), mk(501, 600)} {
		require.NoError(t, w.WritePart(0, p))
	}
	require.NoError(t, w.WritePart(0, corrupt(mk(701, 800))))

	st, ok := w.Ranges(0)

	require.True(t, ok)
	require.Equal(t, int64(1000), st.Size)
	require.Equal(t, "x.bin", st.Name)
	require.Equal(t, []filewriter.Range{{Start: 100, End: 300}, {Start: 500, End: 600}}, st.Done)
	require.Equal(t, []filewriter.Range{{Start: 700, End: 800}}, st.Damaged)
	require.Equal(t, []filewriter.Range{{Start: 0, End: 100}, {Start: 300, End: 500}, {Start: 600, End: 700}, {Start: 800, End: 1000}}, st.Missing)
	require.Equal(t, st.Missing, st.Pending)
	require.Empty(t, st.Lost)
	require.Equal(t, int64(300), st.DoneBytes())
	require.Equal(t, int64(100), st.DamagedBytes())
	require.Equal(t, int64(600), st.MissingBytes())
	require.False(t, st.Complete())
}

func TestRangesUnknownKey(t *testing.T) {
	w, _ := newWriter(t)
	_, ok := w.Ranges(42)
	require.False(t, ok)
}

func TestMarkMissingSeparatesLostFromPending(t *testing.T) {
	w, _ := newWriter(t)
	data := pattern(400)
	parts := splitParts("x.bin", data, 100)
	require.NoError(t, w.WritePart(0, parts[0]))

	require.NoError(t, w.MarkMissing(0, filewriter.Range{Start: 100, End: 200}))
	require.NoError(t, w.MarkMissing(0, filewriter.Range{Start: 0, End: 150})) // Done bytes unaffected

	st, _ := w.Ranges(0)
	require.Equal(t, []filewriter.Range{{Start: 100, End: 200}}, st.Lost)
	require.Equal(t, []filewriter.Range{{Start: 100, End: 400}}, st.Missing)
	require.Equal(t, []filewriter.Range{{Start: 200, End: 400}}, st.Pending)

	require.NoError(t, w.WritePart(0, parts[1])) // late arrival clears the mark
	st, _ = w.Ranges(0)
	require.Empty(t, st.Lost)
	require.Equal(t, []filewriter.Range{{Start: 200, End: 400}}, st.Missing)
}

func TestMarkMissingRejectsUnknownKeyAndBadRange(t *testing.T) {
	w, _ := newWriter(t)
	require.ErrorIs(t, w.MarkMissing(7, filewriter.Range{Start: 0, End: 1}), filewriter.ErrUnknownFile)
	require.NoError(t, w.WritePart(0, splitParts("x.bin", pattern(100), 100)[0]))
	require.Error(t, w.MarkMissing(0, filewriter.Range{Start: 50, End: 101}))
	require.Error(t, w.MarkMissing(0, filewriter.Range{Start: -1, End: 10}))
	require.Error(t, w.MarkMissing(0, filewriter.Range{Start: 10, End: 5}))
}

func TestRangesReturnsCopies(t *testing.T) {
	w, _ := newWriter(t)
	require.NoError(t, w.WritePart(0, splitParts("x.bin", pattern(100), 50)[0]))
	st, _ := w.Ranges(0)
	st.Done[0].End = 999
	st2, _ := w.Ranges(0)
	require.Equal(t, int64(50), st2.Done[0].End)
}

func TestWritePartSameKeyConcurrentDuplicatesAreSafe(t *testing.T) {
	w, dir := newWriter(t)
	data := pattern(20000)
	parts := splitParts("x.bin", data, 100)
	var wg sync.WaitGroup
	for g := range 8 {
		rng := rand.New(rand.NewPCG(uint64(g), 9))
		ps := slices.Clone(parts)
		rng.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
		wg.Go(func() {
			for _, p := range ps {
				if g%2 == 1 && p.Begin%300 == 1 { // some goroutines also send damaged copies
					assert.NoError(t, w.WritePart(0, corrupt(p)))
				}
				assert.NoError(t, w.WritePart(0, p))
				if p.Begin%1000 == 1 {
					_, _ = w.Ranges(0)
				}
			}
		})
	}
	wg.Wait()

	st, _ := w.Ranges(0)
	require.True(t, st.Complete())
	require.Equal(t, data, readFile(t, w, dir, 0))
	require.Len(t, listDir(t, dir), 1)
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("cannot count open file descriptors on this platform")
	}
	return len(ents)
}

func TestWriterBoundsOpenFilesAndEvictedFilesStayWritable(t *testing.T) {
	const files, maxOpen = 200, 8
	base := openFDs(t)
	w, dir := newWriter(t, filewriter.WithMaxOpenFiles(maxOpen))
	contents := make([][]byte, files)
	parts := make([][]yenc.Part, files)
	for k := range files {
		contents[k] = pattern(200 + k)
		parts[k] = splitParts(fmt.Sprintf("f%03d.bin", k), contents[k], 100)
		require.NoError(t, w.WritePart(filewriter.FileKey(k), parts[k][0]))
		require.LessOrEqual(t, openFDs(t)-base, maxOpen+1, "after file %d", k)
	}

	for k := range files {
		for _, p := range parts[k][1:] {
			require.NoError(t, w.WritePart(filewriter.FileKey(k), p))
		}
		require.LessOrEqual(t, openFDs(t)-base, maxOpen+1)
	}

	for k := range files {
		got := readFile(t, w, dir, filewriter.FileKey(k))
		require.Equal(t, contents[k], got, "file %d", k)
	}
}

func TestWriterBoundsOpenFilesUnderConcurrency(t *testing.T) {
	const files, maxOpen = 40, 4
	w, dir := newWriter(t, filewriter.WithMaxOpenFiles(maxOpen))
	var wg sync.WaitGroup
	contents := make([][]byte, files)
	for k := range files {
		contents[k] = pattern(3000 + k)
		parts := splitParts(fmt.Sprintf("f%03d.bin", k), contents[k], 50)
		rand.New(rand.NewPCG(uint64(k), 1)).Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
		wg.Go(func() {
			for _, p := range parts {
				assert.NoError(t, w.WritePart(filewriter.FileKey(k), p))
			}
		})
	}
	wg.Wait()
	for k := range files {
		require.Equal(t, contents[k], readFile(t, w, dir, filewriter.FileKey(k)))
	}
}

func TestFinishSyncsClosesAndKeepsBookkeeping(t *testing.T) {
	w, dir := newWriter(t)
	data := pattern(300)
	parts := splitParts("x.bin", data, 100)
	require.NoError(t, w.WritePart(0, parts[0]))
	require.NoError(t, w.WritePart(0, parts[1]))

	require.NoError(t, w.Finish(0))

	require.Equal(t, data[:200], readFile(t, w, dir, 0)[:200])
	st, _ := w.Ranges(0)
	require.Equal(t, []filewriter.Range{{Start: 0, End: 200}}, st.Done)
	require.NoError(t, w.WritePart(0, parts[2]), "a finished file is reopened on demand")
	require.Equal(t, data, readFile(t, w, dir, 0))
}

func TestFinishUnknownKey(t *testing.T) {
	w, _ := newWriter(t)
	require.ErrorIs(t, w.Finish(3), filewriter.ErrUnknownFile)
}

func TestSyncAndCloseSucceedAndCloseIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dl")
	w, err := filewriter.New(dir, filewriter.WithMaxOpenFiles(2))
	require.NoError(t, err)
	data := pattern(500)
	for k := range 5 {
		require.NoError(t, w.WritePart(filewriter.FileKey(k), splitParts(fmt.Sprintf("f%d", k), data, 500)[0]))
	}

	require.NoError(t, w.Sync())
	require.NoError(t, w.Close())
	require.NoError(t, w.Close())

	require.ErrorIs(t, w.WritePart(0, splitParts("f0", data, 500)[0]), filewriter.ErrClosed)
	for k := range 5 {
		got, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%d", k)))
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
}

func TestReopenRefusesSymlinkSwappedIn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	w, dir := newWriter(t)
	data := pattern(200)
	parts := splitParts("x.bin", data, 100)
	require.NoError(t, w.WritePart(0, parts[0]))
	require.NoError(t, w.Finish(0))
	outside := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(outside, []byte("victim"), 0o666))
	require.NoError(t, os.Remove(filepath.Join(dir, "x.bin")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "x.bin")))

	err := w.WritePart(0, parts[1])

	require.Error(t, err)
	got, _ := os.ReadFile(outside)
	require.Equal(t, "victim", string(got))
	st, _ := w.Ranges(0)
	require.Equal(t, []filewriter.Range{{Start: 0, End: 100}}, st.Done)
}

func TestFailedWriteLeavesBookkeepingUnchanged(t *testing.T) {
	w, dir := newWriter(t)
	parts := splitParts("x.bin", pattern(200), 100)
	require.NoError(t, w.WritePart(0, parts[0]))
	require.NoError(t, w.Finish(0))
	require.NoError(t, os.Remove(filepath.Join(dir, "x.bin")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "x.bin"), 0o777)) // reopen must fail

	err := w.WritePart(0, parts[1])

	require.Error(t, err)
	st, _ := w.Ranges(0)
	require.Equal(t, []filewriter.Range{{Start: 0, End: 100}}, st.Done)
	require.Equal(t, []filewriter.Range{{Start: 100, End: 200}}, st.Missing)
}

func TestWriteErrorsAreWrapped(t *testing.T) {
	w, dir := newWriter(t)
	parts := splitParts("x.bin", pattern(200), 100)
	require.NoError(t, w.WritePart(0, parts[0]))
	require.NoError(t, w.Finish(0))
	require.NoError(t, os.Remove(filepath.Join(dir, "x.bin")))

	err := w.WritePart(0, parts[1])

	require.ErrorIs(t, err, fs.ErrNotExist)
}
