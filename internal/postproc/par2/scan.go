package par2

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"io"
	"iter"
)

// Limits that bound what a hostile .par2 file can make the parser do.
const (
	headerLen = 64

	// MaxPacketLen is the largest packet length the scanner accepts.
	MaxPacketLen = 1 << 30
	// MaxFiles is the largest number of file IDs a Main packet may list.
	MaxFiles = 100_000
	// MaxNameLen is the largest FileDesc name field (padding included) and
	// the largest Creator text accepted.
	MaxNameLen = 64 << 10
	// MaxSlicesPerFile is the largest number of IFSC pairs accepted for one
	// file. PAR2 tools cap a set at 32 768 source slices, so this is generous.
	MaxSlicesPerFile = 1 << 17

	windowSize = 256 << 10
	hashChunk  = 64 << 10
)

var magic = []byte("PAR2\x00PKT")

const (
	typeMain     = "PAR 2.0\x00Main\x00\x00\x00\x00"
	typeFileDesc = "PAR 2.0\x00FileDesc"
	typeIFSC     = "PAR 2.0\x00IFSC\x00\x00\x00\x00"
	typeRecvSlic = "PAR 2.0\x00RecvSlic"
	typeCreator  = "PAR 2.0\x00Creator\x00"
)

// Scan yields every valid packet in ra, which holds size bytes.
//
// The stream may be corrupt or truncated. The scanner finds packets by their
// magic, verifies each packet's MD5 and skips anything else, so garbage
// between packets and damaged packets cost only themselves. Packets of
// unknown type are verified and ignored. RecvSlic slice data is never held in
// memory: the packet reports where the data lives in ra.
//
// An error is yielded (once, with a zero Packet) only when reading ra fails.
// Work on corrupt input is bounded: the bytes hashed for packets that fail
// verification are limited to a small multiple of size.
func Scan(ra io.ReaderAt, size int64) iter.Seq2[Packet, error] {
	return scan(ra, size, nil)
}

// scan is Scan with a hook that is told about every region it skips.
func scan(ra io.ReaderAt, size int64, skip func(off int64, reason string)) iter.Seq2[Packet, error] {
	return func(yield func(Packet, error) bool) {
		s := &scanner{ra: ra, size: size, budget: 4*size + 1<<20, skip: skip}
		s.run(yield)
	}
}

type scanner struct {
	ra   io.ReaderAt
	size int64

	win    []byte // read window: bytes [winOff, winOff+len(win)) of the stream
	winOff int64

	body []byte // scratch for in-memory packet bodies
	hbuf []byte // scratch for streaming hashes

	budget int64 // bytes of failed hashing still allowed
	skip   func(off int64, reason string)
}

func (s *scanner) skipped(off int64, reason string) {
	if s.skip != nil {
		s.skip(off, reason)
	}
}

// readFull fills p from offset off. A short read at the end of the stream is
// an error because callers have already checked the range against size.
func (s *scanner) readFull(p []byte, off int64) error {
	n, err := s.ra.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// window returns the bytes of the stream starting at pos: at least need of
// them, or everything up to the end of the stream when fewer remain.
func (s *scanner) window(pos int64, need int) ([]byte, error) {
	winEnd := s.winOff + int64(len(s.win))
	if pos >= s.winOff && pos < winEnd && (winEnd-pos >= int64(need) || winEnd >= s.size) {
		return s.win[pos-s.winOff:], nil
	}
	n := int(min(int64(windowSize), s.size-pos))
	if n <= 0 {
		s.win, s.winOff = s.win[:0], pos
		return nil, nil
	}
	if cap(s.win) < n {
		s.win = make([]byte, windowSize)
	}
	s.win = s.win[:n]
	s.winOff = pos
	if err := s.readFull(s.win, pos); err != nil {
		s.win = s.win[:0]
		return nil, err
	}
	return s.win, nil
}

// findMagic returns the offset of the next magic at or after pos, or -1.
func (s *scanner) findMagic(pos int64) (int64, error) {
	for pos < s.size {
		w, err := s.window(pos, len(magic))
		if err != nil {
			return -1, err
		}
		if i := bytes.Index(w, magic); i >= 0 {
			return pos + int64(i), nil
		}
		end := pos + int64(len(w))
		if end >= s.size {
			return -1, nil
		}
		pos = end - int64(len(magic)) + 1 // keep a possible partial magic
	}
	return -1, nil
}

func (s *scanner) run(yield func(Packet, error) bool) {
	pos := int64(0)
	for {
		at, err := s.findMagic(pos)
		if err != nil {
			yield(Packet{}, err)
			return
		}
		if at < 0 {
			return
		}
		if at > pos {
			s.skipped(pos, "garbage before packet")
		}
		p, next, ok, err := s.packetAt(at)
		if err != nil {
			yield(Packet{}, err)
			return
		}
		if s.budget < 0 {
			s.skipped(at, "scan work budget exhausted")
			return
		}
		if !ok {
			pos = next
			continue
		}
		pos = next
		if p.Body != nil && !yield(p, nil) {
			return
		}
	}
}

// packetAt tries to read a packet whose magic is at pos. On success it returns
// the packet (Body nil for an ignored type) and the offset after it. Otherwise
// it returns the offset to resume scanning from.
func (s *scanner) packetAt(pos int64) (p Packet, next int64, ok bool, err error) {
	w, err := s.window(pos, headerLen)
	if err != nil {
		return p, 0, false, err
	}
	if len(w) < headerLen {
		s.skipped(pos, "truncated header")
		return p, s.size, false, nil // nothing can follow within 64 bytes
	}
	hdr := w[:headerLen]
	length := binary.LittleEndian.Uint64(hdr[8:16])
	switch {
	case length < headerLen || length%4 != 0 || length > MaxPacketLen:
		s.skipped(pos, "invalid packet length")
		return p, pos + 1, false, nil
	case length > uint64(s.size-pos):
		s.skipped(pos, "packet runs past end of file")
		return p, pos + 1, false, nil
	}
	n := int64(length)
	typ := string(hdr[48:64])
	if !bodyLenOK(typ, n-headerLen) {
		s.skipped(pos, "invalid body length")
		return p, pos + 1, false, nil
	}

	var sum [16]byte
	copy(sum[:], hdr[16:32])
	p = Packet{Offset: pos, Length: n}
	copy(p.SetID[:], hdr[32:48])
	hh := md5.New()
	hh.Write(hdr[32:])

	var body []byte
	switch typ {
	case typeMain, typeFileDesc, typeIFSC, typeCreator:
		blen := int(n - headerLen)
		if cap(s.body) < blen {
			s.body = make([]byte, blen)
		}
		body = s.body[:blen]
		if err := s.readFull(body, pos+headerLen); err != nil {
			return p, 0, false, err
		}
		hh.Write(body)
	default:
		var exp [4]byte
		if typ == typeRecvSlic {
			if err := s.readFull(exp[:], pos+headerLen); err != nil {
				return p, 0, false, err
			}
		}
		if err := s.hashRange(hh, pos+headerLen, n-headerLen); err != nil {
			return p, 0, false, err
		}
		body = exp[:]
	}
	if !bytes.Equal(hh.Sum(nil), sum[:]) {
		s.budget -= n + 4096
		s.skipped(pos, "packet MD5 mismatch")
		return p, pos + 1, false, nil
	}

	switch typ {
	case typeMain:
		p.Body, ok = parseMain(body)
	case typeFileDesc:
		p.Body, ok = parseFileDesc(body)
	case typeIFSC:
		p.Body, ok = parseIFSC(body)
	case typeCreator:
		p.Body, ok = &Creator{Text: string(bytes.TrimRight(body, "\x00"))}, true
	case typeRecvSlic:
		p.Body, ok = &RecvSlic{
			Exponent:   binary.LittleEndian.Uint32(body),
			DataOffset: pos + headerLen + 4,
			DataLength: n - headerLen - 4,
		}, true
	default:
		ok = true // valid packet of an unknown type: skipped, Body stays nil
	}
	if !ok {
		s.skipped(pos, "malformed packet body")
		return p, pos + n, false, nil
	}
	return p, pos + n, true, nil
}

// hashRange feeds size bytes of the stream starting at off into h.
func (s *scanner) hashRange(h io.Writer, off, size int64) error {
	if s.hbuf == nil {
		s.hbuf = make([]byte, hashChunk)
	}
	for size > 0 {
		c := s.hbuf[:min(size, int64(len(s.hbuf)))]
		if err := s.readFull(c, off); err != nil {
			return err
		}
		h.Write(c)
		off += int64(len(c))
		size -= int64(len(c))
	}
	return nil
}

// bodyLenOK reports whether a body of n bytes is structurally possible for a
// packet type. It is checked before anything is read or allocated.
func bodyLenOK(typ string, n int64) bool {
	switch typ {
	case typeMain:
		return n >= 12 && (n-12)%16 == 0 && (n-12)/16 <= MaxFiles
	case typeFileDesc:
		return n >= 56 && n-56 <= MaxNameLen
	case typeIFSC:
		return n >= 16 && (n-16)%20 == 0 && (n-16)/20 <= MaxSlicesPerFile
	case typeRecvSlic:
		return n >= 4
	case typeCreator:
		return n <= MaxNameLen
	}
	return true // unknown type: verified and ignored
}

func parseMain(body []byte) (Body, bool) {
	total := (len(body) - 12) / 16
	n := binary.LittleEndian.Uint32(body[8:12])
	if uint64(n) > uint64(total) {
		return nil, false
	}
	m := &Main{
		SliceSize:    binary.LittleEndian.Uint64(body[0:8]),
		DerivedSetID: md5.Sum(body),
	}
	ids := make([]ID, total)
	for i := range ids {
		copy(ids[i][:], body[12+i*16:])
	}
	m.RecoveryIDs = ids[:n:n]
	m.NonRecoveryIDs = ids[n:]
	return m, true
}

func parseFileDesc(body []byte) (Body, bool) {
	fd := &FileDesc{Length: binary.LittleEndian.Uint64(body[48:56])}
	copy(fd.FileID[:], body[0:16])
	copy(fd.MD5[:], body[16:32])
	copy(fd.MD516k[:], body[32:48])
	fd.Name = string(bytes.TrimRight(body[56:], "\x00"))
	return fd, true
}

func parseIFSC(body []byte) (Body, bool) {
	ifsc := &IFSC{}
	copy(ifsc.FileID[:], body[0:16])
	pairs := body[16:]
	ifsc.Slices = make([]SliceChecksum, len(pairs)/20)
	for i := range ifsc.Slices {
		e := pairs[i*20:]
		copy(ifsc.Slices[i].MD5[:], e[:16])
		ifsc.Slices[i].CRC32 = binary.LittleEndian.Uint32(e[16:20])
	}
	return ifsc, true
}
