package par2_test

import (
	"crypto/md5"
	"encoding/binary"
	"hash/crc32"
)

// This file is a test-only PAR2 packet builder. It writes packets and small
// sets from scratch, computing every ID and MD5 itself, so the name reader is
// tested against an independent implementation of the packet layout and
// hostile file names can be put into otherwise valid sets. Recovery slice
// payloads are arbitrary bytes, not Reed-Solomon data.

const (
	typeMain     = "PAR 2.0\x00Main\x00\x00\x00\x00"
	typeFileDesc = "PAR 2.0\x00FileDesc"
	typeIFSC     = "PAR 2.0\x00IFSC\x00\x00\x00\x00"
	typeRecvSlic = "PAR 2.0\x00RecvSlic"
)

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
	p := make([]byte, 64+len(body))
	copy(p[0:8], "PAR2\x00PKT")
	binary.LittleEndian.PutUint64(p[8:16], uint64(len(p)))
	copy(p[32:48], setID[:])
	copy(p[48:64], typ)
	copy(p[64:], body)
	sum := md5.Sum(p[32:])
	copy(p[16:32], sum[:])
	return p
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

func (f fileSpec) id() [16]byte {
	h := f.md516k()
	return md5sum(h[:], binary.LittleEndian.AppendUint64(nil, uint64(len(f.Data))), []byte(f.Name))
}

func (f fileSpec) fileDescBody() []byte {
	id := f.id()
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

func (f fileSpec) ifscBody(sliceSize int) []byte {
	id := f.id()
	b := append([]byte(nil), id[:]...)
	for off := 0; off < len(f.Data); off += sliceSize {
		slice := make([]byte, sliceSize)
		copy(slice, f.Data[off:min(off+sliceSize, len(f.Data))])
		sum := md5.Sum(slice)
		b = append(b, sum[:]...)
		b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(slice))
	}
	return b
}

// testSet is a structurally valid recovery set whose file names are whatever
// the test says.
type testSet struct {
	sliceSize int
	files     []fileSpec
	setID     [16]byte
	mainBody  []byte
}

func newSet(sliceSize int, files ...fileSpec) *testSet {
	s := &testSet{sliceSize: sliceSize, files: files}
	s.mainBody = binary.LittleEndian.AppendUint64(nil, uint64(sliceSize))
	s.mainBody = binary.LittleEndian.AppendUint32(s.mainBody, uint32(len(files)))
	for _, f := range files {
		id := f.id()
		s.mainBody = append(s.mainBody, id[:]...)
	}
	s.setID = md5sum(s.mainBody)
	return s
}

func (s *testSet) mainPacket() []byte { return rawPacket(s.setID, typeMain, s.mainBody) }

func (s *testSet) fileDescPacket(i int) []byte {
	return rawPacket(s.setID, typeFileDesc, s.files[i].fileDescBody())
}

// index is Main, then FileDesc and IFSC per file.
func (s *testSet) index() []byte {
	out := s.mainPacket()
	for i, f := range s.files {
		out = append(out, s.fileDescPacket(i)...)
		out = append(out, rawPacket(s.setID, typeIFSC, f.ifscBody(s.sliceSize))...)
	}
	return out
}

// volume is the Main packet followed by recovery packets with the given
// payload (a multiple of 4 bytes each).
func (s *testSet) volume(payloads ...[]byte) []byte {
	out := s.mainPacket()
	for i, p := range payloads {
		body := binary.LittleEndian.AppendUint32(nil, uint32(i))
		out = append(out, rawPacket(s.setID, typeRecvSlic, append(body, p...))...)
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
