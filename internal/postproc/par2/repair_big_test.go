package par2_test

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"hash/crc32"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

// sparseEnv is a single huge file that is zero except for a few slices, with a
// set whose recovery slices are true Reed-Solomon data. Zero slices add
// nothing to a recovery slice, so the encoder only has to visit the non-zero
// ones and the test never holds the file in memory.
type sparseEnv struct {
	Dir      string
	Set      *par2.Set
	Name     string
	Length   int64
	SliceLen int64
	NonZero  map[int][]byte // slice index -> its true content (last slice: trimmed)
}

func newSparseEnv(t testing.TB, length, sliceSize int64, nonZero map[int]int64, exponents []uint32) *sparseEnv {
	t.Helper()
	const name = "huge.bin"
	slices := int((length + sliceSize - 1) / sliceSize)
	env := &sparseEnv{Dir: t.TempDir(), Name: name, Length: length, SliceLen: sliceSize, NonZero: map[int][]byte{}}
	for i, seed := range nonZero {
		r := rand.New(rand.NewPCG(uint64(seed), 99))
		n := min(sliceSize, length-int64(i)*sliceSize)
		d := make([]byte, n)
		for j := range d {
			d[j] = byte(r.Uint32())
		}
		env.NonZero[i] = d
	}
	pad := func(i int) []byte {
		s := make([]byte, sliceSize)
		copy(s, env.NonZero[i])
		return s
	}

	// Data file.
	f, err := os.Create(filepath.Join(env.Dir, name))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(length))
	for i, d := range env.NonZero {
		_, err := f.WriteAt(d, int64(i)*sliceSize)
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())

	// Checksums and the full-file MD5, streamed.
	zero := make([]byte, sliceSize)
	zeroSum, zeroCRC := md5.Sum(zero), crc32.ChecksumIEEE(zero)
	full := md5.New()
	var ifsc []byte
	for i := range slices {
		sum, crc := zeroSum, zeroCRC
		if _, ok := env.NonZero[i]; ok {
			p := pad(i)
			sum, crc = md5.Sum(p), crc32.ChecksumIEEE(p)
		}
		ifsc = append(ifsc, sum[:]...)
		ifsc = binary.LittleEndian.AppendUint32(ifsc, crc)
		n := min(sliceSize, length-int64(i)*sliceSize)
		if d, ok := env.NonZero[i]; ok {
			full.Write(d)
		} else {
			full.Write(zero[:n])
		}
	}
	var fullSum [16]byte
	full.Sum(fullSum[:0])
	head := make([]byte, 16*1024)
	if d := env.NonZero[0]; d != nil {
		copy(head, d)
	}
	h16 := md5.Sum(head)
	id := md5sum(h16[:], binary.LittleEndian.AppendUint64(nil, uint64(length)), []byte(name))
	fd := append([]byte(nil), id[:]...)
	fd = append(fd, fullSum[:]...)
	fd = append(fd, h16[:]...)
	fd = binary.LittleEndian.AppendUint64(fd, uint64(length))
	fd = append(fd, name...)
	for len(fd)%4 != 0 {
		fd = append(fd, 0)
	}
	mb := mainBody(uint64(sliceSize), [][16]byte{id}, nil)
	setID := md5sum(mb)
	pkts := rawPacket(setID, typeMain, mb)
	pkts = append(pkts, rawPacket(setID, typeFileDesc, fd)...)
	pkts = append(pkts, rawPacket(setID, typeIFSC, append(append([]byte(nil), id[:]...), ifsc...))...)

	consts := gf16.InputConstants(slices)
	for _, e := range exponents {
		rec := make([]byte, sliceSize)
		for i := range env.NonZero {
			require.NoError(t, gf16.MulAdd(rec, pad(i), gf16.Pow(consts[i], e)))
		}
		pkts = append(pkts, rawPacket(setID, typeRecvSlic, recvSlicBody(e, rec))...)
	}
	parDir := t.TempDir()
	writeFile(t, parDir, "s.par2", pkts)
	env.Set, err = par2.LoadDir(parDir)
	require.NoError(t, err)
	return env
}

func (e *sparseEnv) path() string { return filepath.Join(e.Dir, e.Name) }

// garble overwrites slice i with junk.
func (e *sparseEnv) garble(t testing.TB, i int) {
	t.Helper()
	f, err := os.OpenFile(e.path(), os.O_WRONLY, 0)
	require.NoError(t, err)
	defer f.Close()
	n := min(e.SliceLen, e.Length-int64(i)*e.SliceLen)
	junk := make([]byte, n)
	for j := range junk {
		junk[j] = 0xEE
	}
	_, err = f.WriteAt(junk, int64(i)*e.SliceLen)
	require.NoError(t, err)
}

func (e *sparseEnv) requireSlice(t testing.TB, i int) {
	t.Helper()
	f, err := os.Open(e.path())
	require.NoError(t, err)
	defer f.Close()
	n := min(e.SliceLen, e.Length-int64(i)*e.SliceLen)
	got := make([]byte, n)
	_, err = f.ReadAt(got, int64(i)*e.SliceLen)
	require.NoError(t, err)
	want := e.NonZero[i]
	if want == nil {
		want = make([]byte, n)
	}
	require.Equal(t, want, got, "slice %d", i)
}

func TestRepair_PeakMemoryIsIndependentOfFileSize(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 512 MiB sparse set")
	}
	const sliceSize = 256 << 10
	length := int64(512<<20) - 1000 // 2048 slices, the last one partial
	last := int((length - 1) / sliceSize)
	env := newSparseEnv(t, length, sliceSize, map[int]int64{7: 1, 900: 2, last: 3}, exps(0, 4))
	for _, i := range []int{3, 900, last} { // a zero slice, a data slice and the partial one
		env.garble(t, i)
	}
	vr, err := par2.Verify(context.Background(), env.Dir, env.Set)
	require.NoError(t, err)
	require.EqualValues(t, 3, vr.DamagedSlices)

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak uint64
	res, err := par2.Repair(context.Background(), env.Dir, env.Set, vr, par2.WithChunkSize(64<<10),
		par2.WithAfterChunk(func(int) {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapAlloc)
		}))

	require.NoError(t, err)
	require.Equal(t, 3, res.RepairedSlices)
	require.NotZero(t, peak)
	growth := int64(peak) - int64(base.HeapAlloc)
	t.Logf("512 MiB set, 3 damaged slices: peak heap growth %d KiB", growth>>10)
	require.Less(t, growth, int64(8<<20))
	for _, i := range []int{3, 7, 900, last} {
		env.requireSlice(t, i)
	}
}

// BenchmarkRepair repairs 3 of the 256 slices (1.2%) of a 64 MiB set (4 files
// of 16 MiB, 256 KiB slices, 8 recovery slices) in each iteration.
func BenchmarkRepair(b *testing.B) {
	const sliceSize = 256 << 10
	files := testFiles(16<<20, 16<<20, 16<<20, 16<<20)
	ts := newSet(sliceSize, files...)
	recs := encodeRecovery(b, files, sliceSize, exps(0, 8))
	vol := ts.mainPacket()
	for e := range uint32(8) {
		vol = append(vol, rawPacket(ts.SetID, typeRecvSlic, recvSlicBody(e, recs[e]))...)
	}
	parDir, dir := b.TempDir(), b.TempDir()
	writeFile(b, parDir, "s.par2", concat(ts.Index(), vol))
	set, err := par2.LoadDir(parDir)
	if err != nil {
		b.Fatal(err)
	}
	for _, f := range files {
		writeFile(b, dir, f.Name, f.Data)
	}
	b.SetBytes(int64(len(files)) * (16 << 20))
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		for i, f := range files {
			path := filepath.Join(dir, f.Name)
			data := append([]byte(nil), f.Data...)
			switch i {
			case 0:
				data[3*sliceSize] ^= 0xFF
			case 2:
				data[9*sliceSize+5] ^= 0xFF
				data[40*sliceSize+7] ^= 0xFF
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				b.Fatal(err)
			}
		}
		vr, err := par2.Verify(context.Background(), dir, set)
		if err != nil || vr.DamagedSlices != 3 {
			b.Fatalf("verify: %v, damaged %d", err, vr.DamagedSlices)
		}
		b.StartTimer()
		if _, err := par2.Repair(context.Background(), dir, set, vr); err != nil {
			b.Fatal(err)
		}
	}
	// Repair ends with a Verify of the whole (now intact) set; report what
	// that costs so the reconstruction itself is ns/op minus verify-ns/op.
	b.StopTimer()
	start := time.Now()
	if _, err := par2.Verify(context.Background(), dir, set); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(time.Since(start).Nanoseconds()), "verify-ns/op")
}
