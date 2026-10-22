package unpack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/metrics"
	"testing"
	"time"

	"github.com/bodgit/sevenzip"
	"github.com/stretchr/testify/require"
)

// heapUse is what an operation cost.
type heapUse struct {
	peak  uint64 // highest sampled live heap above the starting point
	total uint64 // bytes allocated during the operation (TotalAlloc delta)
	took  time.Duration
}

func (h heapUse) String() string {
	return fmt.Sprintf("peak +%d MiB, allocated %d MiB, %v", h.peak>>20, h.total>>20, h.took.Round(time.Millisecond))
}

// abortHeap stops the test binary when a measured operation is far past any
// budget, so a regression shows up as a message and not as an OOM kill.
const abortHeap = 3 << 30

// measureHeap runs fn and samples the live heap every 2 ms, which catches the
// peak of an allocation that is freed again before fn returns.
func measureHeap(fn func()) heapUse {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	live := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	base := live()
	var peak uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			if v := live(); v > base && v-base > peak {
				peak = v - base
				if peak > abortHeap {
					fmt.Fprintf(os.Stderr, "measureHeap: heap grew by %d MiB, aborting the test binary\n", peak>>20)
					os.Exit(3)
				}
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	start := time.Now()
	fn()
	took := time.Since(start)
	close(stop)
	<-done
	runtime.ReadMemStats(&after)
	return heapUse{peak: peak, total: after.TotalAlloc - before.TotalAlloc, took: took}
}

// unpackBytes writes an archive to a temp file and unpacks it.
func unpackBytes(t testing.TB, archive []byte, opts ...Option) (Result, error) {
	t.Helper()
	dir := t.TempDir()
	p := writeFile(t, dir, "x.7z", archive)
	set := ArchiveSet{Format: Format7z, Name: "x", Volumes: []string{p}}
	return Unpack(context.Background(), set, filepath.Join(t.TempDir(), "out"), opts...)
}

const (
	hostileHeapBudget = 64 << 20
	hostileTimeBudget = 2 * time.Second
)

// TestHostile7zHeadersFailFastWithoutAllocating opens archives whose headers
// lie about counts and sizes. The library allocates from those numbers before
// anything else looks at the archive, so each case would grow the heap by
// gigabytes (measured on the unguarded code path, sevenzip v1.6.5, darwin/arm64:
// see the table). Every case now fails in a bounded time with a classified
// error and a heap growth below the budget.
func TestHostile7zHeadersFailFastWithoutAllocating(t *testing.T) {
	cases := []struct {
		name    string
		archive []byte
		want    error
	}{
		{"files 2p31", hostileFiles(1 << 31), ErrTooManyEntries},
		{"files 2p32m1", hostileFiles(1<<32 - 1), ErrTooManyEntries},
		{"folders 2p28", hostileFolders(1 << 28), ErrTooManyEntries},
		{"pack streams 2p31", hostilePackStreams(1 << 31), ErrTooManyEntries},
		{"sub streams 2p31", hostileSubStreams(1 << 31), ErrTooManyEntries},
		{"coder properties 4GiB", hostileCoderProps(1<<32 - 1), ErrCorrupt},
		{"coder input streams 2p31", hostileCoderStreams(1 << 31), ErrCorrupt},
		{"coders 2p31", hostileCoders(1 << 31), ErrCorrupt},
		{"header offset beyond EOF", append(signatureHeader(1<<40, 10, 0), make([]byte, 10)...), ErrCorrupt},
		{"header size beyond EOF", append(signatureHeader(0, 1<<62, 0), make([]byte, 10)...), ErrCorrupt},
		{"header size 4 GiB", append(signatureHeader(0, 1<<32, 0), make([]byte, 10)...), ErrCorrupt},
		{"header CRC wrong", func() []byte { a := build7zStored(z7file{name: "a", data: []byte("x")}); a[len(a)-3] ^= 1; return a }(), ErrCorrupt},
		{"plain header, 3M empty names", assemble7z(nil, namesBombHeader(3_000_000)), ErrTooManyEntries},
		{"plain header, 700k empty names", assemble7z(nil, namesBombHeader(700_000)), ErrTooLarge},
		{"encoded header, 3M empty names", hostileEncodedHeader(namesBombHeader(3_000_000), 0), ErrTooManyEntries},
		{"encoded header, 700k empty names", hostileEncodedHeader(namesBombHeader(700_000), 0), ErrTooLarge},
		{"encoded header declares 1 TiB", hostileEncodedHeader(namesBombHeader(10), 1<<40), ErrTooLarge},
		{"encoded header that decodes past what it declares", hostileEncodedHeader(namesBombHeader(1000), 100), ErrCorrupt},
		{"encoded header that decodes to zeros", hostileEncodedHeader(make([]byte, 4<<20), 0), ErrCorrupt},
		{"encoded header that decodes to another encoded header", hostileEncodedHeader(append([]byte{idEncodedHeader}, make([]byte, 40)...), 0), ErrCorrupt},
		{"1M files and no bytes for them", hostileFiles(1_000_000), ErrCorrupt},
		{"name table of 2^60 bytes", hostileNameTable(1000, 1<<60), ErrCorrupt},
		{"attribute table for 2^30 files", hostileAttributes(1 << 30), ErrTooManyEntries},
		{"attribute table for 100k files without values", hostileAttributes(100_000), ErrCorrupt},
		{"pack stream of 2^60 bytes", hostilePackSize(1 << 60), ErrCorrupt},
		{"folder of 2 GiB packed into 4 bytes", declaring7z(2 << 30), ErrBomb},
		{"files that have data and no folders", assemble7z(nil, append([]byte{idHeader}, append(filesInfo([]z7file{{name: "a", data: []byte("x")}}), idEnd)...)), ErrCorrupt},
		{"unknown coder", withUnknownCoder(), ErrUnsupported},
		{"AES with 2^30 key derivation rounds", buildWithCoder("\x06\xf1\x07\x01", []byte{0x40 | 30, 0, 0}, []byte("abcd"), z7file{name: "a", data: []byte("abcd")}), ErrUnsupported},
		{"PPMd with 3 GiB of memory", buildWithCoder("\x03\x04\x01", []byte{6, 0, 0, 0, 0xc0}, []byte("abcd"), z7file{name: "a", data: []byte("abcd")}), ErrUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			use := measureHeap(func() { _, err = unpackBytes(t, c.archive) })
			t.Logf("%s", use)
			require.ErrorIs(t, err, c.want)
			require.Less(t, use.peak, uint64(hostileHeapBudget), "heap peak")
			require.Less(t, use.total, uint64(hostileHeapBudget), "bytes allocated")
			require.Less(t, use.took, hostileTimeBudget)
		})
	}
}

// TestHeaderMemoryEstimateCoversTheLibrary opens a realistic header with the
// library alone and checks that the estimate preflight charges per file is at
// least what the library really takes at its peak. When a new version of the
// library allocates more per file, this test says so and the constant in
// sevenz_header.go has to grow.
func TestHeaderMemoryEstimateCoversTheLibrary(t *testing.T) {
	const n = 100_000
	archive := manyFiles(n)
	var r *sevenzip.Reader
	var err error
	use := measureHeap(func() {
		r, err = sevenzip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	})
	require.NoError(t, err)
	require.Len(t, r.File, n)
	perFile := use.peak / n
	t.Logf("library: peak %d bytes per file (%s), estimate %d, FileHeader %d bytes",
		perFile, use, bytesPerFile, reflect.TypeFor[sevenzip.FileHeader]().Size())
	require.LessOrEqual(t, perFile, uint64(bytesPerFile))
}

func TestPreflightKeepsValidArchivesWorking(t *testing.T) {
	files := []z7file{
		{name: "dir", dir: true},
		{name: "dir/a.txt", data: []byte("alpha")},
		{name: "empty.txt"},
		{name: "dir/b.bin", data: bytes.Repeat([]byte("bravo"), 1000)},
	}
	archives := map[string][]byte{
		"plain header":          build7zStored(files...),
		"LZMA-encoded header":   build7zEncodedHeader(files...),
		"LZMA data":             nil,
		"LZMA2 data":            nil,
		"LZMA-encoded, no data": build7zEncodedHeader(z7file{name: "only-empty"}),
	}
	data := joinData(files[1:2])
	props, packed := lzmaRaw(data)
	archives["LZMA data"] = buildWithCoder(zIDLZMA, props, packed, files[1])
	archives["LZMA2 data"] = buildWithCoder(zIDLZMA2, []byte{16}, lzma2Raw(data), files[1])
	for name, archive := range archives {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "out")
			p := writeFile(t, t.TempDir(), "x.7z", archive)

			res, err := Unpack(context.Background(), ArchiveSet{Format: Format7z, Name: "x", Volumes: []string{p}}, dst)

			require.NoError(t, err)
			for _, f := range res.Files {
				for _, want := range files {
					if want.name == f {
						require.Equal(t, string(want.data), readFile(t, filepath.Join(dst, f)))
					}
				}
			}
			require.NotEmpty(t, res.Files)
		})
	}
}

// TestLZMA2DictionaryIsClampedToTheStream: an LZMA2 stream announces its
// dictionary in one byte and the library allocates all of it. Property byte 38
// is 2 GiB (measured on the unguarded path: 2048 MiB, 140 ms) for a stream of
// 17 bytes.
func TestLZMA2DictionaryIsClampedToTheStream(t *testing.T) {
	data := []byte("hello hello hello")
	archive := buildWithCoder(zIDLZMA2, []byte{38}, lzma2Raw(data), z7file{name: "a", data: data})
	var err error
	var res Result
	dst := ""
	use := measureHeap(func() {
		dir := t.TempDir()
		p := writeFile(t, dir, "x.7z", archive)
		dst = filepath.Join(t.TempDir(), "out")
		res, err = Unpack(context.Background(), ArchiveSet{Format: Format7z, Name: "x", Volumes: []string{p}}, dst)
	})
	t.Logf("%s", use)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, res.Files)
	require.Equal(t, "hello hello hello", readFile(t, filepath.Join(dst, "a")))
	require.Less(t, use.peak, uint64(hostileHeapBudget))
	require.Less(t, use.total, uint64(hostileHeapBudget))
}

func TestDecoderMemoryThatTheStreamNeedsStaysUnderTheDictionaryCap(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 2<<20) // 2 MiB: the stream needs a 2 MiB dictionary
	archive := buildWithCoder(zIDLZMA2, []byte{36}, lzma2Raw(data), z7file{name: "a", data: data})

	_, err := unpackBytes(t, archive, WithMaxDictionary(1<<20))
	require.ErrorIs(t, err, ErrUnsupported)

	res, err := unpackBytes(t, archive, WithMaxDictionary(4<<20))
	require.NoError(t, err)
	require.EqualValues(t, 2<<20, res.Bytes)
}

func TestMultiVolume7zIsValidatedAcrossTheVolumes(t *testing.T) {
	files := []z7file{{name: "a.txt", data: bytes.Repeat([]byte("x"), 3000)}, {name: "b.txt", data: []byte("tail")}}
	t.Run("a valid archive cut into volumes", func(t *testing.T) {
		dir := t.TempDir()
		for i, v := range splitInto(build7zEncodedHeader(files...), 5) {
			writeFile(t, dir, fmt.Sprintf("x.7z.%03d", i+1), v)
		}
		set := detectOne(t, dir)
		require.Greater(t, len(set.Volumes), 1)
		dst := filepath.Join(t.TempDir(), "out")

		res, err := Unpack(context.Background(), set, dst)

		require.NoError(t, err)
		require.Equal(t, []string{"a.txt", "b.txt"}, res.Files)
	})
	t.Run("a header that points past the last volume", func(t *testing.T) {
		archive := append(signatureHeader(100, 50, 0), make([]byte, 120)...) // 32+100+50 > 152
		dir := t.TempDir()
		for i, v := range splitInto(archive, 3) {
			writeFile(t, dir, fmt.Sprintf("x.7z.%03d", i+1), v)
		}

		var err error
		use := measureHeap(func() { _, err = Unpack(context.Background(), detectOne(t, dir), filepath.Join(t.TempDir(), "out")) })

		require.ErrorIs(t, err, ErrCorrupt)
		require.Less(t, use.total, uint64(hostileHeapBudget))
	})
	t.Run("a header spread over the volumes", func(t *testing.T) {
		// Cut so that the header itself is split between two volumes.
		archive := build7zStored(files...)
		dir := t.TempDir()
		writeFile(t, dir, "x.7z.001", archive[:len(archive)-20])
		writeFile(t, dir, "x.7z.002", archive[len(archive)-20:])

		res, err := Unpack(context.Background(), detectOne(t, dir), filepath.Join(t.TempDir(), "out"))

		require.NoError(t, err)
		require.Len(t, res.Files, 2)
	})
}

func TestSevenZipLimitsApplyBeforeTheLibraryOpensTheArchive(t *testing.T) {
	three := build7zStored(z7file{name: "a", data: []byte("1")}, z7file{name: "b", data: []byte("2")}, z7file{name: "c", data: []byte("3")})

	t.Run("max entries", func(t *testing.T) {
		_, err := unpackBytes(t, three, WithMaxEntries(2))
		require.ErrorIs(t, err, ErrTooManyEntries)
	})
	t.Run("max bytes against the declared folder size", func(t *testing.T) {
		_, err := unpackBytes(t, three, WithMaxBytes(2))
		require.ErrorIs(t, err, ErrTooLarge)
	})
	t.Run("max header memory", func(t *testing.T) {
		_, err := unpackBytes(t, manyFiles(1000), WithMaxHeaderMemory(100_000))
		require.ErrorIs(t, err, ErrTooLarge)
		_, err = unpackBytes(t, manyFiles(1000), WithMaxHeaderMemory(2_000_000))
		require.NotErrorIs(t, err, ErrTooLarge)
	})
	t.Run("header size cap", func(t *testing.T) {
		_, err := unpackBytes(t, manyFiles(1000), func(c *config) { c.maxHeader = 1000 })
		require.ErrorIs(t, err, ErrTooLarge)
	})
	t.Run("encoded header size cap", func(t *testing.T) {
		archive := build7zEncodedHeader(z7file{name: "a", data: []byte("1")})
		_, err := unpackBytes(t, archive, func(c *config) { c.maxHeader = 10 })
		require.ErrorIs(t, err, ErrTooLarge)
		_, err = unpackBytes(t, archive)
		require.NoError(t, err)
	})
}

func TestHeaderOverlayServesTheCheckedHeaderNotTheFile(t *testing.T) {
	archive := build7zEncodedHeader(z7file{name: "a", data: []byte("abc")}, z7file{name: "b", data: []byte("defgh")})
	dir := t.TempDir()
	p := writeFile(t, dir, "x.7z", archive)
	m, err := openMulti([]string{p})
	require.NoError(t, err)
	defer func() { _ = m.Close() }()

	ov, err := preflight7z(m, m.size, newConfig(nil), m.size)
	require.NoError(t, err)
	before := make([]byte, ov.size())
	n, err := ov.ReadAt(before, 0)
	require.NoError(t, err)
	require.EqualValues(t, ov.size(), n)
	require.Equal(t, byte(idHeader), before[ov.size()-int64(len(ov.hdr))], "the header is plain even though the file's is encoded")

	// The file changes under us after the check.
	require.NoError(t, os.WriteFile(p, bytes.Repeat([]byte{0xff}, len(archive)), 0o600))
	after := make([]byte, ov.size())
	_, err = ov.ReadAt(after, 0)
	require.NoError(t, err)
	hdrStart := ov.size() - int64(len(ov.hdr))
	require.Equal(t, before[:sigHeaderSize], after[:sigHeaderSize])
	require.Equal(t, before[hdrStart:], after[hdrStart:])

	// Reads at every alignment give the same bytes as one read.
	for _, chunk := range []int{1, 7, 32, 33, 100} {
		got := make([]byte, 0, len(before))
		buf := make([]byte, chunk)
		for off := int64(0); off < ov.size(); {
			k, err := ov.ReadAt(buf, off)
			got = append(got, buf[:k]...)
			off += int64(k)
			if err != nil {
				require.ErrorIs(t, err, io.EOF)
				break
			}
		}
		require.Equal(t, after, got, "chunk %d", chunk)
	}
}

// hostile7zSeeds are the crafted archives of the table above, for the fuzzers.
func hostile7zSeeds() [][]byte {
	data := []byte("hello hello hello")
	props, packed := lzmaRaw(data)
	files := []z7file{{name: "dir", dir: true}, {name: "dir/a.txt", data: data}, {name: "e"}}
	return [][]byte{
		hostileFiles(1 << 31), hostileFolders(1 << 28), hostilePackStreams(1 << 31),
		hostileSubStreams(1 << 31), hostileCoderProps(1<<32 - 1), hostileCoderStreams(1 << 31),
		hostileCoders(1 << 31), hostileNameTable(1000, 1<<60), hostileAttributes(1 << 30),
		hostilePackSize(1 << 60), declaring7z(2 << 30), withUnknownCoder(),
		assemble7z(nil, namesBombHeader(2000)),
		hostileEncodedHeader(namesBombHeader(2000), 0),
		hostileEncodedHeader(namesBombHeader(10), 1<<40),
		build7zStored(files...), build7zEncodedHeader(files...), manyFiles(50),
		buildWithCoder(zIDLZMA, props, packed, files[1]),
		buildWithCoder(zIDLZMA2, []byte{38}, lzma2Raw(data), files[1]),
		buildWithCoder("\x03\x04\x01", []byte{6, 0, 0, 0, 0xc0}, packed, files[1]),
		signatureHeader(1<<40, 10, 0),
	}
}

// fuzzPreflightAllocBudget bounds what preflight and the library together may
// allocate for one input, whatever the input declares. The fuzz config caps
// the header at 1 MiB and its memory estimate at 16 MiB.
const fuzzPreflightAllocBudget = 64 << 20

// splitArchive cuts an archive into the bytes before its header and the
// header, as the signature header says.
func splitArchive(a []byte) (streams, header []byte, ok bool) {
	if len(a) < sigHeaderSize {
		return nil, nil, false
	}
	off := binary.LittleEndian.Uint64(a[12:])
	size := binary.LittleEndian.Uint64(a[20:])
	avail := uint64(len(a) - sigHeaderSize)
	if off > avail || size > avail-off {
		return nil, nil, false
	}
	return a[sigHeaderSize : sigHeaderSize+off], a[sigHeaderSize+off : sigHeaderSize+off+size], true
}

// FuzzSevenZipPreflight feeds arbitrary bytes to the 7z pre-flight. It must
// never panic, must classify every error, and whatever it accepts must be an
// archive the library opens without allocating more than the budget.
//
// The checksums of a 7z archive reject nearly every mutation before the parser
// runs, so the input is also taken apart: mode 1 puts the fuzzed header behind
// a signature header whose offsets and CRCs are right, mode 2 does that after
// compressing the header, so the fuzzer reaches the parser and the decoder.
func FuzzSevenZipPreflight(f *testing.F) {
	for _, seed := range hostile7zSeeds() {
		f.Add(uint8(0), []byte(nil), seed)
		if streams, header, ok := splitArchive(seed); ok {
			f.Add(uint8(1), streams, header)
		}
	}
	for _, n := range []string{"t0.7z", "t1.7z", "t2.7z", "t3.7z", "t4.7z", "t5.7z", "lzma2.7z", "empty.7z", "multi.7z.001"} {
		if b, err := os.ReadFile(filepath.Join("testdata", "7z", n)); err == nil {
			f.Add(uint8(0), []byte(nil), b)
			if streams, header, ok := splitArchive(b); ok {
				f.Add(uint8(1), streams, header)
			}
		}
	}
	f.Add(uint8(2), []byte("stream"), namesBombHeader(5))
	f.Fuzz(func(t *testing.T, mode uint8, streams, header []byte) {
		if len(streams) > 1<<17 || len(header) > 1<<17 {
			return
		}
		var data []byte
		switch mode % 3 {
		case 0:
			data = header
		case 1:
			data = assemble7z(streams, header)
		case 2:
			data = encodeHeader(streams, header)
		}
		cfg := newConfig([]Option{WithLogger(nil), WithPassword("password"), func(c *config) {
			c.maxHeader = 1 << 20
			c.maxHeadMem = 16 << 20
		}})
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()

		ov, err := preflight7z(bytes.NewReader(data), int64(len(data)), cfg, int64(len(data)))
		if err != nil {
			classified := false
			for _, known := range []error{ErrCorrupt, ErrPassword, ErrUnsupported, ErrLimit} {
				classified = classified || errors.Is(err, known)
			}
			if !classified {
				t.Fatalf("unclassified error: %v", err)
			}
		} else {
			// Accepted: the library must cope with it within the budget. It may
			// still reject it; it may even panic on a header the checks let by,
			// which the production code turns into ErrCorrupt.
			_ = guarded(func() error {
				r, err := sevenzip.NewReaderWithPassword(ov, ov.size(), "password")
				if err != nil {
					return err
				}
				for _, file := range r.File {
					_ = file.Name
				}
				return nil
			})
		}

		runtime.ReadMemStats(&after)
		if grew := after.TotalAlloc - before.TotalAlloc; grew > fuzzPreflightAllocBudget {
			t.Fatalf("an input of %d bytes made the pre-flight and the library allocate %d MiB (err %v)", len(data), grew>>20, err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("an input of %d bytes took %v (err %v)", len(data), took, err)
		}
	})
}
