package par2_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func offsets(pkts []par2.Packet) []int64 {
	var out []int64
	for _, p := range pkts {
		out = append(out, p.Offset)
	}
	return out
}

func TestScan_EmptyAndTinyStreamsYieldNothing(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("PAR2"), []byte("PAR2\x00PKT"), bytes.Repeat([]byte{0}, 63), []byte("PAR2\x00PKT" + string(make([]byte, 40)))} {
		require.Empty(t, scanBytes(t, in))
	}
}

func TestScan_ResyncsAfterGarbageBetweenPackets(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	a, b := set.mainPacket(), set.fileDescPacket(0)
	garbage1 := []byte("this is not a packet, PAR2 and PKT and PAR2\x00PK are decoys")
	garbage2 := bytes.Repeat([]byte{0xFF, 0x00, 'P'}, 1000)
	stream := concat(garbage1, a, garbage2, b, garbage1)

	pkts := scanBytes(t, stream)

	require.Equal(t, []int64{int64(len(garbage1)), int64(len(garbage1) + len(a) + len(garbage2))}, offsets(pkts))
	require.IsType(t, &par2.Main{}, pkts[0].Body)
	require.IsType(t, &par2.FileDesc{}, pkts[1].Body)
}

func TestScan_FindsPacketsAcrossWindowBoundaries(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	pkt := set.mainPacket()
	for pad := 256<<10 - 80; pad <= 256<<10+10; pad++ {
		stream := concat(make([]byte, pad), pkt, make([]byte, 100), pkt)
		pkts := scanBytes(t, stream)
		require.Equal(t, []int64{int64(pad), int64(pad + len(pkt) + 100)}, offsets(pkts), "pad %d", pad)
	}
}

func TestScan_TruncatedFinalPacketYieldsEarlierPackets(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	full := concat(set.mainPacket(), set.fileDescPacket(0), set.ifscPacket(0))
	last := len(set.ifscPacket(0))

	for cut := 1; cut < last; cut++ {
		pkts := scanBytes(t, full[:len(full)-cut])
		require.Len(t, pkts, 2, "cut %d bytes", cut)
	}
}

func TestScan_FlippedBodyByteSkipsOnlyThatPacket(t *testing.T) {
	set := newSet(8, testFiles(20, 30)...)
	a, b, c := set.mainPacket(), set.fileDescPacket(0), set.fileDescPacket(1)
	for i := 16; i < len(b); i++ { // everything after the magic and length, MD5 field included
		bad := bytes.Clone(b)
		bad[i] ^= 0x01
		pkts := scanBytes(t, concat(a, bad, c))
		require.Equal(t, []int64{0, int64(len(a) + len(b))}, offsets(pkts), "flipped byte %d", i)
	}
}

func TestScan_FlippedMagicSkipsOnlyThatPacket(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	a, b := set.mainPacket(), set.fileDescPacket(0)
	bad := bytes.Clone(a)
	bad[3] ^= 0x01

	pkts := scanBytes(t, concat(bad, b))

	require.Equal(t, []int64{int64(len(a))}, offsets(pkts))
}

func TestScan_ImpossibleLengthFieldsAreSkipped(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	good := set.fileDescPacket(0)
	fileSize := func(extra int) uint64 { return uint64(len(good)*2 + extra) }
	cases := map[string]uint64{
		"zero":                0,
		"below header":        63,
		"just header minus 4": 60,
		"not multiple of 4":   uint64(len(good)) + 2,
		"past end of file":    fileSize(4),
		"over 1 GiB":          1<<30 + 4,
		"max u64":             math.MaxUint64,
		"max u64 minus 3":     math.MaxUint64 - 3,
		"sign bit":            1 << 63,
	}
	for name, length := range cases {
		t.Run(name, func(t *testing.T) {
			bad := bytes.Clone(good)
			binary.LittleEndian.PutUint64(bad[8:16], length)
			reseal(bad) // MD5 is valid for the bytes; only the length is impossible

			pkts := scanBytes(t, concat(bad, good))

			require.Equal(t, []int64{int64(len(bad))}, offsets(pkts))
		})
	}
}

func TestScan_UnknownPacketTypeIsIgnored(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	unknown := rawPacket(set.SetID, "PAR 2.0\x00Future\x00\x00", []byte("whatever"))

	pkts := scanBytes(t, concat(set.mainPacket(), unknown, set.fileDescPacket(0)))

	require.Len(t, pkts, 2)
	require.IsType(t, &par2.FileDesc{}, pkts[1].Body)
}

func TestScan_UnknownPacketWithBadMD5DoesNotHideLaterPackets(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	unknown := rawPacket(set.SetID, "PAR 2.0\x00Future\x00\x00", []byte("whatever"))
	unknown[len(unknown)-1] ^= 1

	pkts := scanBytes(t, concat(unknown, set.mainPacket()))

	require.Len(t, pkts, 1)
}

func TestScan_StructurallyMalformedPacketsAreSkipped(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	id := set.IDs[0]
	bodies := map[string]struct {
		typ  string
		body []byte
	}{
		"main too short":             {typeMain, make([]byte, 8)},
		"main ids not 16-aligned":    {typeMain, make([]byte, 12+17)},
		"main count beyond ids":      {typeMain, binary.LittleEndian.AppendUint32(make([]byte, 8), 5)},
		"main huge count":            {typeMain, binary.LittleEndian.AppendUint32(make([]byte, 8), math.MaxUint32)},
		"filedesc too short":         {typeFileDesc, make([]byte, 52)},
		"filedesc name over 64 KiB":  {typeFileDesc, make([]byte, 56+par2.MaxNameLen+4)},
		"ifsc without file id":       {typeIFSC, make([]byte, 12)},
		"ifsc partial pair":          {typeIFSC, append(id[:], make([]byte, 24)...)},
		"ifsc more pairs than limit": {typeIFSC, make([]byte, 16+20*(par2.MaxSlicesPerFile+1))},
		"recvslic without exponent":  {typeRecvSlic, nil},
		"creator over 64 KiB":        {typeCreator, make([]byte, par2.MaxNameLen+4)},
	}
	for name, c := range bodies {
		t.Run(name, func(t *testing.T) {
			bad := rawPacket(set.SetID, c.typ, c.body)

			pkts := scanBytes(t, concat(bad, set.mainPacket()))

			require.Equal(t, []int64{int64(len(bad))}, offsets(pkts))
		})
	}
}

func TestScan_MainWithTooManyFileIDsIsSkipped(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	ids := make([][16]byte, par2.MaxFiles+1)
	bad := rawPacket(set.SetID, typeMain, mainBody(8, ids, nil))

	pkts := scanBytes(t, concat(bad, set.mainPacket()))

	require.Equal(t, []int64{int64(len(bad))}, offsets(pkts))
}

func TestScan_MainAtFileLimitIsAccepted(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	ids := make([][16]byte, par2.MaxFiles-1)
	m := rawPacket(set.SetID, typeMain, mainBody(8, ids[:10], ids[10:]))

	pkts := scanBytes(t, m)

	require.Len(t, pkts, 1)
	main := pkts[0].Body.(*par2.Main)
	require.Len(t, main.RecoveryIDs, 10)
	require.Len(t, main.NonRecoveryIDs, par2.MaxFiles-1-10)
}

func TestScan_HugeClaimsDoNotAllocate(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	var stream []byte
	for range 50 {
		stream = append(stream, rawPacket(set.SetID, typeMain, binary.LittleEndian.AppendUint32(make([]byte, 8), math.MaxUint32))...)
		bad := rawPacket(set.SetID, typeIFSC, make([]byte, 16))
		binary.LittleEndian.PutUint64(bad[8:16], 1<<30) // claims 1 GiB in a tiny file
		stream = append(stream, bad...)
	}
	// A valid 64 MiB IFSC is far over the pair limit and must not be read in.
	z := newZeroPacket(set.SetID, typeIFSC, make([]byte, 16), 64<<20)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	require.Empty(t, scanBytes(t, stream))
	for range par2.Scan(z, z.size()) {
		t.Fatal("oversized IFSC must not be yielded")
	}
	runtime.ReadMemStats(&after)

	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8<<20))
}

// countingReader counts the bytes served by ReadAt.
type countingReader struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n.Add(int64(n))
	return n, err
}

func TestScan_HostileRepeatedHeadersDoNotCauseQuadraticWork(t *testing.T) {
	const size = 4 << 20
	set := newSet(8, testFiles(20)...)
	// Every 64 bytes starts a header that claims to run to (nearly) the end of
	// the file but has a bad MD5, so each candidate is expensive to reject.
	hdr := rawPacket(set.SetID, typeRecvSlic, make([]byte, 4))
	stream := make([]byte, 0, size)
	for len(stream) < size {
		h := bytes.Clone(hdr)
		binary.LittleEndian.PutUint64(h[8:16], uint64((size-len(stream))&^3))
		stream = append(stream, h...)
	}
	cr := &countingReader{r: bytes.NewReader(stream)}

	for range par2.Scan(cr, int64(len(stream))) {
	}

	require.Less(t, cr.n.Load(), int64(12*size), "bytes read while rejecting hostile headers")
}

func TestScan_StopsWhenConsumerBreaks(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	stream := concat(set.mainPacket(), set.fileDescPacket(0), set.ifscPacket(0))
	n := 0
	for range par2.Scan(bytes.NewReader(stream), int64(len(stream))) {
		n++
		break
	}
	require.Equal(t, 1, n)
}

type failingReader struct{ err error }

func (f failingReader) ReadAt([]byte, int64) (int, error) { return 0, f.err }

func TestScan_ReportsReadErrors(t *testing.T) {
	boom := errors.New("boom")
	var gotErr error
	for p, err := range par2.Scan(failingReader{boom}, 1000) {
		require.Nil(t, p.Body)
		gotErr = err
	}
	require.ErrorIs(t, gotErr, boom)
}

func TestScan_SizeLargerThanReaderIsAnErrorNotAPanic(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	stream := set.mainPacket()
	var gotErr error
	for _, err := range par2.Scan(bytes.NewReader(stream), int64(len(stream))+1000) {
		gotErr = err
	}
	require.Error(t, gotErr)
	_ = fmt.Sprint(gotErr)
}
