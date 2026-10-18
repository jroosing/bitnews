package par2_test

import (
	"crypto/md5"
	"encoding/binary"
	"hash/crc32"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file is a test-only PAR2 packet builder. It constructs valid packets
// and sets from scratch, computing every ID and MD5 itself, so the parser is
// tested against an independent implementation of the packet layout.
// Recovery slice payloads are arbitrary bytes, not real Reed-Solomon data:
// the parser never looks at them.

const (
	typeMain     = "PAR 2.0\x00Main\x00\x00\x00\x00"
	typeFileDesc = "PAR 2.0\x00FileDesc"
	typeIFSC     = "PAR 2.0\x00IFSC\x00\x00\x00\x00"
	typeRecvSlic = "PAR 2.0\x00RecvSlic"
	typeCreator  = "PAR 2.0\x00Creator\x00"
)

const headerLen = 64

func md5sum(parts ...[]byte) [16]byte {
	h := md5.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out [16]byte
	h.Sum(out[:0])
	return out
}

// rawPacket builds a packet with a correct MD5. body must be a multiple of 4.
func rawPacket(setID [16]byte, typ string, body []byte) []byte {
	if len(typ) != 16 {
		panic("type must be 16 bytes")
	}
	p := make([]byte, headerLen+len(body))
	copy(p[0:8], "PAR2\x00PKT")
	binary.LittleEndian.PutUint64(p[8:16], uint64(len(p)))
	copy(p[32:48], setID[:])
	copy(p[48:64], typ)
	copy(p[64:], body)
	sum := md5.Sum(p[32:])
	copy(p[16:32], sum[:])
	return p
}

// reseal recomputes the packet MD5 after a test mutated a packet's bytes.
func reseal(p []byte) []byte {
	sum := md5.Sum(p[32:])
	copy(p[16:32], sum[:])
	return p
}

func mainBody(sliceSize uint64, recovery, nonRecovery [][16]byte) []byte {
	b := binary.LittleEndian.AppendUint64(nil, sliceSize)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(recovery)))
	for _, id := range recovery {
		b = append(b, id[:]...)
	}
	for _, id := range nonRecovery {
		b = append(b, id[:]...)
	}
	return b
}

type fileSpec struct {
	Name string
	Data []byte
}

func (f fileSpec) md516k() [16]byte {
	d := f.Data
	if len(d) > 16*1024 {
		d = d[:16*1024]
	}
	return md5sum(d)
}

func (f fileSpec) ID() [16]byte {
	h := f.md516k()
	return md5sum(h[:], binary.LittleEndian.AppendUint64(nil, uint64(len(f.Data))), []byte(f.Name))
}

func fileDescBody(id [16]byte, f fileSpec) []byte {
	full := md5sum(f.Data)
	h16 := f.md516k()
	b := append([]byte(nil), id[:]...)
	b = append(b, full[:]...)
	b = append(b, h16[:]...)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(f.Data)))
	b = append(b, f.Name...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func sliceCount(length, sliceSize uint64) uint64 { return (length + sliceSize - 1) / sliceSize }

func ifscBody(id [16]byte, f fileSpec, sliceSize int) []byte {
	b := append([]byte(nil), id[:]...)
	for off := 0; off < len(f.Data); off += sliceSize {
		end := min(off+sliceSize, len(f.Data))
		slice := make([]byte, sliceSize)
		copy(slice, f.Data[off:end])
		sum := md5.Sum(slice)
		b = append(b, sum[:]...)
		b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(slice))
	}
	return b
}

func recvSlicBody(exponent uint32, data []byte) []byte {
	return append(binary.LittleEndian.AppendUint32(nil, exponent), data...)
}

// testSet is a valid recovery set built from scratch.
type testSet struct {
	SliceSize int
	Files     []fileSpec
	IDs       [][16]byte // file IDs, in Files order
	SetID     [16]byte
	MainBody  []byte
}

// newSet builds a set whose recovery-set file IDs are listed in ascending
// order, as par2cmdline writes them.
func newSet(sliceSize int, files ...fileSpec) *testSet {
	s := &testSet{SliceSize: sliceSize, Files: files}
	for _, f := range files {
		s.IDs = append(s.IDs, f.ID())
	}
	s.MainBody = mainBody(uint64(sliceSize), s.IDs, nil)
	s.SetID = md5sum(s.MainBody)
	return s
}

func (s *testSet) mainPacket() []byte { return rawPacket(s.SetID, typeMain, s.MainBody) }

func (s *testSet) fileDescPacket(i int) []byte {
	return rawPacket(s.SetID, typeFileDesc, fileDescBody(s.IDs[i], s.Files[i]))
}

func (s *testSet) ifscPacket(i int) []byte {
	return rawPacket(s.SetID, typeIFSC, ifscBody(s.IDs[i], s.Files[i], s.SliceSize))
}

func (s *testSet) creatorPacket(text string) []byte {
	b := []byte(text)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return rawPacket(s.SetID, typeCreator, b)
}

// sliceData returns deterministic payload for a recovery slice.
func (s *testSet) sliceData(exponent uint32) []byte {
	r := rand.New(rand.NewPCG(uint64(exponent), 7))
	d := make([]byte, s.SliceSize)
	for i := range d {
		d[i] = byte(r.Uint32())
	}
	return d
}

func (s *testSet) recvPacket(exponent uint32) []byte {
	return rawPacket(s.SetID, typeRecvSlic, recvSlicBody(exponent, s.sliceData(exponent)))
}

// Index is the critical packets: Creator, Main, FileDesc and IFSC per file.
func (s *testSet) Index() []byte {
	out := s.creatorPacket("bitnews test builder")
	out = append(out, s.mainPacket()...)
	for i := range s.Files {
		out = append(out, s.fileDescPacket(i)...)
		out = append(out, s.ifscPacket(i)...)
	}
	return out
}

// Volume is a recovery volume: the Main packet followed by recovery slices.
func (s *testSet) Volume(exponents ...uint32) []byte {
	out := s.mainPacket()
	for _, e := range exponents {
		out = append(out, s.recvPacket(e)...)
	}
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func writeFile(t testing.TB, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// testFiles returns n deterministic files of the given sizes.
func testFiles(sizes ...int) []fileSpec {
	var out []fileSpec
	for i, n := range sizes {
		r := rand.New(rand.NewPCG(uint64(i)+100, 3))
		d := make([]byte, n)
		for j := range d {
			d[j] = byte(r.Uint32())
		}
		out = append(out, fileSpec{Name: "file" + string(rune('A'+i)) + ".bin", Data: d})
	}
	return out
}
