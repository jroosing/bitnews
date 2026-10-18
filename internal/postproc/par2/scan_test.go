package par2_test

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"hash/crc32"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func scanBytes(t testing.TB, b []byte) []par2.Packet {
	t.Helper()
	var out []par2.Packet
	for p, err := range par2.Scan(bytes.NewReader(b), int64(len(b))) {
		require.NoError(t, err)
		out = append(out, p)
	}
	return out
}

func TestScan_YieldsMainPacket(t *testing.T) {
	set := newSet(8, testFiles(20, 5)...)
	stream := set.mainPacket()

	pkts := scanBytes(t, stream)

	require.Len(t, pkts, 1)
	require.Equal(t, par2.ID(set.SetID), pkts[0].SetID)
	require.Equal(t, int64(0), pkts[0].Offset)
	require.Equal(t, int64(len(stream)), pkts[0].Length)
	m, ok := pkts[0].Body.(*par2.Main)
	require.True(t, ok, "body is %T", pkts[0].Body)
	require.Equal(t, uint64(8), m.SliceSize)
	require.Equal(t, []par2.ID{par2.ID(set.IDs[0]), par2.ID(set.IDs[1])}, m.RecoveryIDs)
	require.Empty(t, m.NonRecoveryIDs)
	require.Equal(t, par2.ID(set.SetID), m.DerivedSetID)
}

func TestScan_ParsesFileDescPacket(t *testing.T) {
	files := testFiles(20)
	files[0].Name = "épisode 01.mkv" // non-ASCII, length not a multiple of 4
	set := newSet(8, files...)

	pkts := scanBytes(t, set.fileDescPacket(0))

	require.Len(t, pkts, 1)
	fd, ok := pkts[0].Body.(*par2.FileDesc)
	require.True(t, ok, "body is %T", pkts[0].Body)
	require.Equal(t, par2.ID(set.IDs[0]), fd.FileID)
	require.Equal(t, md5sum(files[0].Data), fd.MD5)
	require.Equal(t, files[0].md516k(), fd.MD516k)
	require.Equal(t, uint64(20), fd.Length)
	require.Equal(t, "épisode 01.mkv", fd.Name, "padding NULs are trimmed")
}

func TestScan_ParsesIFSCPacket(t *testing.T) {
	files := testFiles(20) // 3 slices of 8 bytes, last one padded
	set := newSet(8, files...)

	pkts := scanBytes(t, set.ifscPacket(0))

	require.Len(t, pkts, 1)
	ifsc, ok := pkts[0].Body.(*par2.IFSC)
	require.True(t, ok, "body is %T", pkts[0].Body)
	require.Equal(t, par2.ID(set.IDs[0]), ifsc.FileID)
	require.Len(t, ifsc.Slices, 3)
	last := make([]byte, 8)
	copy(last, files[0].Data[16:])
	require.Equal(t, md5sum(last), ifsc.Slices[2].MD5)
	require.Equal(t, crc32.ChecksumIEEE(last), ifsc.Slices[2].CRC32)
	require.Equal(t, md5sum(files[0].Data[:8]), ifsc.Slices[0].MD5)
}

func TestScan_RecvSlicReportsExponentAndDataLocation(t *testing.T) {
	set := newSet(8, testFiles(20)...)
	prefix := set.mainPacket()
	stream := concat(prefix, set.recvPacket(5), set.recvPacket(1000000))

	pkts := scanBytes(t, stream)

	require.Len(t, pkts, 3)
	rs, ok := pkts[1].Body.(*par2.RecvSlic)
	require.True(t, ok, "body is %T", pkts[1].Body)
	require.Equal(t, uint32(5), rs.Exponent)
	require.Equal(t, pkts[1].Offset+64+4, rs.DataOffset)
	require.Equal(t, int64(8), rs.DataLength)
	require.Equal(t, set.sliceData(5), stream[rs.DataOffset:rs.DataOffset+rs.DataLength])
	rs2 := pkts[2].Body.(*par2.RecvSlic)
	require.Equal(t, uint32(1000000), rs2.Exponent)
}

// zeroPacketReader is a virtual stream holding one RecvSlic packet whose slice
// data is n zero bytes, so tests can scan far more data than they allocate.
type zeroPacketReader struct {
	head []byte // header + exponent
	n    int64
}

func newZeroRecvSlic(setID [16]byte, exponent uint32, n int64) *zeroPacketReader {
	return newZeroPacket(setID, typeRecvSlic, recvSlicBody(exponent, nil), n)
}

// newZeroPacket is a packet with a valid MD5 whose body is prefix followed by
// n zero bytes.
func newZeroPacket(setID [16]byte, typ string, prefix []byte, n int64) *zeroPacketReader {
	head := rawPacket(setID, typ, prefix)
	binary.LittleEndian.PutUint64(head[8:16], uint64(int64(len(head))+n))
	h := md5.New()
	h.Write(head[32:])
	zeros := make([]byte, 1<<20)
	for left := n; left > 0; {
		c := min(left, int64(len(zeros)))
		h.Write(zeros[:c])
		left -= c
	}
	copy(head[16:32], h.Sum(nil))
	return &zeroPacketReader{head: head, n: n}
}

func (z *zeroPacketReader) size() int64 { return int64(len(z.head)) + z.n }

func (z *zeroPacketReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= z.size() {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off+int64(n) < z.size() {
		if pos := off + int64(n); pos < int64(len(z.head)) {
			n += copy(p[n:], z.head[pos:])
		} else {
			p[n] = 0
			n++
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestScan_LargeRecvSlicUsesBoundedMemory(t *testing.T) {
	const slice = 256 << 20
	z := newZeroRecvSlic([16]byte{1}, 3, slice)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var got []par2.Packet
	for p, err := range par2.Scan(z, z.size()) {
		require.NoError(t, err)
		got = append(got, p)
	}
	runtime.ReadMemStats(&after)

	require.Len(t, got, 1)
	require.Equal(t, int64(slice), got[0].Body.(*par2.RecvSlic).DataLength)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<20), "scanner must stream slice data")
}
