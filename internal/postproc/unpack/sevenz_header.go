package unpack

import (
	"encoding/binary"
	"fmt"
	"math"
)

// This file parses a 7z header just far enough to check it. The 7z library
// allocates from the counts a header declares before it reads the bytes that
// should back them, so the declared numbers are checked here first, against the
// bytes that are actually there and against hard caps. See sevenz_preflight.go.
//
// The parser mirrors the layout the library reads (see its types.go), and is
// stricter wherever the library would later fail anyway.

// 7z property ids used in headers.
const (
	idEnd               = 0x00
	idHeader            = 0x01
	idArchiveProperties = 0x02
	idAdditionalStreams = 0x03
	idMainStreamsInfo   = 0x04
	idFilesInfo         = 0x05
	idPackInfo          = 0x06
	idUnpackInfo        = 0x07
	idSubStreamsInfo    = 0x08
	idSize              = 0x09
	idCRC               = 0x0a
	idFolder            = 0x0b
	idCodersUnpackSize  = 0x0c
	idNumUnpackStream   = 0x0d
	idEmptyStream       = 0x0e
	idEmptyFile         = 0x0f
	idName              = 0x11
	idCTime             = 0x12
	idATime             = 0x13
	idMTime             = 0x14
	idWinAttributes     = 0x15
	idEncodedHeader     = 0x17
	idDummy             = 0x19
)

// Caps on the shape of a header. Real archives stay far below all of them.
const (
	maxCodersPerFolder = 32
	maxStreamsPerCoder = 32
	maxStreamsPerFold  = 64
	maxCoderProps      = 256
	maxIDBytes         = 8
	maxSize            = 1 << 62 // no stream is this long
	streamsPerFolder   = 4       // pack streams allowed per folder on average, BCJ2 uses four

	// Memory the library needs per item, measured and rounded up (see
	// TestHeaderMemoryEstimateCoversTheLibrary).
	bytesPerFile   = 400
	bytesPerFolder = 640
	bytesPerStream = 96
)

var errTruncated = fmt.Errorf("%w: 7z header is truncated", ErrCorrupt)

func corruptf(format string, a ...any) error {
	return fmt.Errorf("%w: 7z header: %s", ErrCorrupt, fmt.Sprintf(format, a...))
}

func unsupportedf(format string, a ...any) error {
	return fmt.Errorf("%w: 7z header: %s", ErrUnsupported, fmt.Sprintf(format, a...))
}

// hdrReader reads a header held in memory. The first error sticks and every
// later read returns zero, so a parse runs straight through and checks err at
// the points that matter.
type hdrReader struct {
	b   []byte
	pos int
	err error
}

func (r *hdrReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *hdrReader) remaining() uint64 { return uint64(len(r.b) - r.pos) }

func (r *hdrReader) byte() byte {
	if r.err != nil {
		return 0
	}
	if r.pos >= len(r.b) {
		r.fail(errTruncated)
		return 0
	}
	c := r.b[r.pos]
	r.pos++
	return c
}

// num reads a 7z variable length number: the leading one bits of the first byte
// count the extra bytes, little endian, and the remaining bits are the top.
func (r *hdrReader) num() uint64 {
	first := r.byte()
	if r.err != nil {
		return 0
	}
	var v uint64
	mask := byte(0x80)
	for i := range uint(8) {
		if first&mask == 0 {
			return v | uint64(first&(mask-1))<<(8*i)
		}
		v |= uint64(r.byte()) << (8 * i)
		mask >>= 1
	}
	return v
}

// take returns the next n bytes, without copying them.
func (r *hdrReader) take(n uint64) []byte {
	if r.err != nil {
		return nil
	}
	if n > r.remaining() {
		r.fail(errTruncated)
		return nil
	}
	b := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return b
}

// bitCount counts the set bits among the first n bits of a 7z bit vector,
// reading the ceil(n/8) bytes it takes.
func (r *hdrReader) bitCount(n uint64) uint64 {
	vec := r.take((n + 7) / 8)
	if r.err != nil {
		return 0
	}
	return countBits(vec, n)
}

func countBits(vec []byte, n uint64) uint64 {
	var c uint64
	for i := range n {
		if vec[i/8]&(0x80>>(i%8)) != 0 {
			c++
		}
	}
	return c
}

// zDigest is an optional CRC32.
type zDigest struct {
	ok bool
	v  uint32
}

// digests reads a digest table for n items. Only callers that need the values
// ask for them with keep.
func (r *hdrReader) digests(n uint64, keep bool) []zDigest {
	all := r.byte()
	var vec []byte
	if all == 0 {
		vec = r.take((n + 7) / 8)
	}
	if r.err != nil {
		return nil
	}
	var defined uint64
	if all != 0 {
		defined = n
	} else {
		defined = countBits(vec, n)
	}
	vals := r.take(4 * defined)
	if !keep || r.err != nil {
		return nil
	}
	out := make([]zDigest, n)
	var next int
	for i := range out {
		if all != 0 || vec[i/8]&(0x80>>(i%8)) != 0 {
			out[i] = zDigest{ok: true, v: binary.LittleEndian.Uint32(vals[next:])}
			next += 4
		}
	}
	return out
}

// zCoder is one coder of a folder.
type zCoder struct {
	key   uint64 // method id: length in the top byte, the id bytes below
	props []byte // aliases the header buffer, so a patch edits the header
	size  uint64 // unpack size of its output stream
}

// zFolder is the part of a folder the checks need.
type zFolder struct {
	coders  []zCoder
	bind    [][2]int // in stream, out stream
	packed  []int    // in streams fed from pack streams
	mainOut int      // the out stream that is not bound
	numIn   int      // input streams over all coders
	crc     zDigest  // CRC32 of the unpacked folder
}

func (f *zFolder) unpackSize() uint64 { return f.coders[f.mainOut].size }

// zStreams is a parsed StreamsInfo.
type zStreams struct {
	packPos   uint64
	packSizes []uint64
	folders   []zFolder
	hasUnpack bool
	// subCounts holds the number of streams in each folder, nil when the
	// header has no SubStreamsInfo and every folder is one stream.
	subCounts  []uint64
	subStreams uint64
}

// zFiles is a parsed FilesInfo.
type zFiles struct {
	count        uint64
	emptyStreams uint64
	nameBytes    uint64
}

// hdrInfo is a parsed header.
type hdrInfo struct {
	streams *zStreams
	files   *zFiles
}

// headerParser parses with the limits of one Unpack.
type headerParser struct {
	cfg *config
	// streamRegion is the length of the area between the signature header and the
	// header, where the pack streams live.
	streamRegion uint64
}

// budget charges n items of the given cost against the header memory cap.
type budget struct{ used, limit int64 }

func (b *budget) charge(n uint64, each int64) error {
	if n > uint64(math.MaxInt64/each) || b.used+int64(n)*each > b.limit {
		return fmt.Errorf("%w: the 7z header would take more than %d MiB to read", ErrTooLarge, b.limit>>20)
	}
	b.used += int64(n) * each
	return nil
}

// parseStreamsInfo reads a StreamsInfo up to and including its end marker.
func (p *headerParser) parseStreamsInfo(r *hdrReader, bud *budget) (*zStreams, error) {
	s := &zStreams{}
	id := r.byte()
	if id == idPackInfo {
		if err := p.parsePackInfo(r, s, bud); err != nil {
			return nil, err
		}
		id = r.byte()
	}
	if id == idUnpackInfo {
		if err := p.parseUnpackInfo(r, s, bud); err != nil {
			return nil, err
		}
		id = r.byte()
	}
	if id == idSubStreamsInfo {
		if !s.hasUnpack {
			return nil, corruptf("sub streams without unpack info")
		}
		if err := p.parseSubStreams(r, s, bud); err != nil {
			return nil, err
		}
		id = r.byte()
	}
	if r.err != nil {
		return nil, r.err
	}
	if id != idEnd {
		return nil, corruptf("unexpected id %#x in streams info", id)
	}
	if err := p.checkStreams(s); err != nil {
		return nil, err
	}
	return s, nil
}

func (p *headerParser) parsePackInfo(r *hdrReader, s *zStreams, bud *budget) error {
	s.packPos = r.num()
	n := r.num()
	if r.err != nil {
		return r.err
	}
	if n == 0 {
		return corruptf("no pack streams")
	}
	if n > streamLimit(p.cfg) {
		return fmt.Errorf("%w: %d pack streams", ErrTooManyEntries, n)
	}
	// Every size takes a byte, so the header must be able to hold them.
	if n > r.remaining() {
		return errTruncated
	}
	if err := bud.charge(n, bytesPerStream); err != nil {
		return err
	}
	id := r.byte()
	if id != idSize {
		return corruptf("pack streams without sizes")
	}
	s.packSizes = make([]uint64, 0, min(n, 1024))
	total := s.packPos
	for range n {
		sz := r.num()
		if r.err != nil {
			return r.err
		}
		if sz > maxSize {
			return corruptf("pack stream of %d bytes", sz)
		}
		total = satAdd(total, sz)
		s.packSizes = append(s.packSizes, sz)
	}
	if total > p.streamRegion {
		return corruptf("pack streams reach %d bytes, the archive has %d before the header", total, p.streamRegion)
	}
	id = r.byte()
	if id == idCRC {
		r.digests(n, false)
		id = r.byte()
	}
	if r.err != nil {
		return r.err
	}
	if id != idEnd {
		return corruptf("unexpected id %#x in pack info", id)
	}
	return nil
}

func streamLimit(cfg *config) uint64 {
	return uint64(max(cfg.maxEntries, 0)) * streamsPerFolder
}

func (p *headerParser) parseUnpackInfo(r *hdrReader, s *zStreams, bud *budget) error {
	if id := r.byte(); r.err == nil && id != idFolder {
		return corruptf("unexpected id %#x, expected folders", id)
	}
	n := r.num()
	external := r.byte()
	if r.err != nil {
		return r.err
	}
	if n == 0 {
		return corruptf("no folders")
	}
	if n > uint64(max(p.cfg.maxEntries, 0)) {
		return fmt.Errorf("%w: %d folders", ErrTooManyEntries, n)
	}
	if external != 0 {
		return unsupportedf("external folder data")
	}
	if n > r.remaining() {
		return errTruncated
	}
	if err := bud.charge(n, bytesPerFolder); err != nil {
		return err
	}
	s.hasUnpack = true
	s.folders = make([]zFolder, 0, min(n, 1024))
	for range n {
		f, err := p.parseFolder(r)
		if err != nil {
			return err
		}
		s.folders = append(s.folders, f)
	}
	if id := r.byte(); r.err == nil && id != idCodersUnpackSize {
		return corruptf("unexpected id %#x, expected unpack sizes", id)
	}
	for i := range s.folders {
		f := &s.folders[i]
		for j := range f.coders {
			sz := r.num()
			if r.err != nil {
				return r.err
			}
			if sz > maxSize {
				return corruptf("unpack size of %d bytes", sz)
			}
			f.coders[j].size = sz
		}
	}
	id := r.byte()
	if id == idCRC {
		for i, d := range r.digests(n, true) {
			s.folders[i].crc = d
		}
		id = r.byte()
	}
	if r.err != nil {
		return r.err
	}
	if id != idEnd {
		return corruptf("unexpected id %#x in unpack info", id)
	}
	for i := range s.folders {
		if err := p.checkCoders(&s.folders[i]); err != nil {
			return err
		}
	}
	return nil
}

func (p *headerParser) parseFolder(r *hdrReader) (zFolder, error) {
	var f zFolder
	nc := r.num()
	if r.err != nil {
		return f, r.err
	}
	if nc == 0 || nc > maxCodersPerFolder {
		return f, corruptf("folder with %d coders", nc)
	}
	var totalIn, totalOut uint64
	f.coders = make([]zCoder, 0, nc)
	for range nc {
		flags := r.byte()
		if r.err != nil {
			return f, r.err
		}
		idLen := uint64(flags & 0x0f)
		if flags&0x80 != 0 {
			return f, unsupportedf("alternative coder methods")
		}
		if idLen > maxIDBytes {
			return f, corruptf("coder id of %d bytes", idLen)
		}
		id := r.take(idLen)
		in, out := uint64(1), uint64(1)
		if flags&0x10 != 0 {
			in, out = r.num(), r.num()
		}
		var props []byte
		if flags&0x20 != 0 {
			n := r.num()
			if r.err == nil && n > maxCoderProps {
				return f, corruptf("coder properties of %d bytes", n)
			}
			props = r.take(n)
		}
		if r.err != nil {
			return f, r.err
		}
		if in == 0 || out == 0 || in > maxStreamsPerCoder || out > maxStreamsPerCoder {
			return f, corruptf("coder with %d inputs and %d outputs", in, out)
		}
		if out != 1 {
			return f, unsupportedf("coder with %d outputs", out)
		}
		totalIn += in
		totalOut += out
		f.coders = append(f.coders, zCoder{key: methodKey(id), props: props})
	}
	if totalIn > maxStreamsPerFold || totalOut > maxStreamsPerFold || totalIn < totalOut {
		return f, corruptf("folder with %d inputs and %d outputs", totalIn, totalOut)
	}
	f.numIn = int(totalIn)
	nb := int(totalOut) - 1
	usedIn := make([]bool, totalIn)
	usedOut := make([]bool, totalOut)
	for range nb {
		in, out := r.num(), r.num()
		if r.err != nil {
			return f, r.err
		}
		if in >= totalIn || out >= totalOut || usedIn[in] || usedOut[out] {
			return f, corruptf("invalid bind pair %d, %d", in, out)
		}
		usedIn[in], usedOut[out] = true, true
		f.bind = append(f.bind, [2]int{int(in), int(out)})
	}
	for i, used := range usedOut {
		if !used {
			f.mainOut = i
		}
	}
	np := int(totalIn) - nb
	if np == 1 {
		for i, used := range usedIn {
			if !used {
				f.packed = append(f.packed, i)
			}
		}
	} else {
		for range np {
			idx := r.num()
			if r.err != nil {
				return f, r.err
			}
			if idx >= totalIn || usedIn[idx] {
				return f, corruptf("invalid packed stream index %d", idx)
			}
			usedIn[idx] = true
			f.packed = append(f.packed, int(idx))
		}
	}
	return f, nil
}

// methodKey packs a coder id into a number for the method table.
func methodKey(id []byte) uint64 {
	if len(id) > 7 {
		return math.MaxUint64 // no known method is that long
	}
	var v uint64
	for _, b := range id {
		v = v<<8 | uint64(b)
	}
	return uint64(len(id))<<56 | v
}

func (p *headerParser) parseSubStreams(r *hdrReader, s *zStreams, bud *budget) error {
	n := len(s.folders)
	counts := make([]uint64, n)
	for i := range counts {
		counts[i] = 1
	}
	id := r.byte()
	if id == idNumUnpackStream {
		for i := range counts {
			counts[i] = r.num()
			if r.err != nil {
				return r.err
			}
			if counts[i] > uint64(max(p.cfg.maxEntries, 0)) {
				return fmt.Errorf("%w: %d streams in one folder", ErrTooManyEntries, counts[i])
			}
			s.subStreams += counts[i]
			if s.subStreams > uint64(max(p.cfg.maxEntries, 0)) {
				return fmt.Errorf("%w: more than %d streams", ErrTooManyEntries, p.cfg.maxEntries)
			}
		}
		id = r.byte()
	} else {
		s.subStreams = uint64(n)
	}
	if err := bud.charge(s.subStreams, bytesPerStream); err != nil {
		return err
	}
	if id == idSize {
		for i, c := range counts {
			if c == 0 {
				continue
			}
			var sum uint64
			for range c - 1 {
				sz := r.num()
				if r.err != nil {
					return r.err
				}
				sum = satAdd(sum, sz)
			}
			if sum > s.folders[i].unpackSize() {
				return corruptf("streams of folder %d are larger than the folder", i)
			}
		}
		id = r.byte()
	} else {
		for i, c := range counts {
			if c > 1 {
				return corruptf("folder %d has %d streams and no sizes", i, c)
			}
		}
	}
	for id != idEnd {
		if r.err != nil {
			return r.err
		}
		if id != idCRC {
			return corruptf("unexpected id %#x in sub streams info", id)
		}
		var want uint64
		for i, c := range counts {
			if c == 1 && s.folders[i].crc.ok {
				continue
			}
			want += c
		}
		r.digests(want, false)
		id = r.byte()
	}
	if r.err != nil {
		return r.err
	}
	s.subCounts = counts
	return nil
}

// checkStreams cross-checks the parts of a StreamsInfo.
func (p *headerParser) checkStreams(s *zStreams) error {
	if s.packSizes == nil && !s.hasUnpack {
		return nil
	}
	if !s.hasUnpack {
		return corruptf("pack streams without folders")
	}
	var need int
	for _, f := range s.folders {
		need += len(f.packed)
	}
	if need != len(s.packSizes) {
		return corruptf("folders use %d pack streams, the header lists %d", need, len(s.packSizes))
	}
	return nil
}

// total returns the number of streams the folders hold.
func (s *zStreams) streams() uint64 {
	if s.subCounts == nil {
		return uint64(len(s.folders))
	}
	return s.subStreams
}

// parseFilesInfo reads a FilesInfo up to and including its end marker.
func (p *headerParser) parseFilesInfo(r *hdrReader, bud *budget) (*zFiles, error) {
	n := r.num()
	if r.err != nil {
		return nil, r.err
	}
	if n == 0 {
		return nil, corruptf("no files")
	}
	if n > uint64(max(p.cfg.maxEntries, 0)) {
		return nil, fmt.Errorf("%w: %d files", ErrTooManyEntries, n)
	}
	// Every file needs at least the two bytes that end its name.
	if 2*n > r.remaining() {
		return nil, errTruncated
	}
	if err := bud.charge(n, bytesPerFile); err != nil {
		return nil, err
	}
	f := &zFiles{count: n}
	var sawNames, sawEmptyStream bool
	for {
		prop := r.byte()
		if r.err != nil {
			return nil, r.err
		}
		if prop == idEnd {
			break
		}
		size := r.num()
		body := &hdrReader{b: r.take(size)}
		if r.err != nil {
			return nil, r.err
		}
		switch prop {
		case idEmptyStream:
			f.emptyStreams = body.bitCount(n)
			sawEmptyStream = true
		case idEmptyFile:
			if !sawEmptyStream || f.emptyStreams == 0 {
				return nil, corruptf("empty file flags without empty streams")
			}
			body.bitCount(f.emptyStreams)
		case idName:
			if err := checkNames(body, n); err != nil {
				return nil, err
			}
			f.nameBytes = size
			sawNames = true
		case idCTime, idATime, idMTime:
			readOptionalTable(body, n, 8)
		case idWinAttributes:
			readOptionalTable(body, n, 4)
		case idDummy:
			body.pos = len(body.b)
		default:
			return nil, unsupportedf("file property %#x", prop)
		}
		if body.err != nil {
			return nil, body.err
		}
		if body.pos != len(body.b) {
			return nil, corruptf("file property %#x has %d unread bytes", prop, len(body.b)-body.pos)
		}
	}
	if !sawNames {
		return nil, corruptf("files without names")
	}
	return f, nil
}

// checkNames checks the name table: an external flag, then n names of UTF-16
// code units that each end with a zero unit.
func checkNames(r *hdrReader, n uint64) error {
	if r.byte() != 0 {
		if r.err != nil {
			return r.err
		}
		return unsupportedf("external file names")
	}
	data := r.take(r.remaining())
	if r.err != nil {
		return r.err
	}
	if len(data)%2 != 0 {
		return corruptf("file names are not UTF-16")
	}
	var names uint64
	for i := 0; i < len(data); i += 2 {
		if data[i] == 0 && data[i+1] == 0 {
			names++
		}
	}
	if names != n {
		return corruptf("%d file names for %d files", names, n)
	}
	return nil
}

// readOptionalTable reads a table of n values of the given width where each may
// be undefined (times and attributes): an all-defined flag or a bit vector, an
// external flag, then the defined values.
func readOptionalTable(r *hdrReader, n, width uint64) {
	all := r.byte()
	defined := n
	if all == 0 {
		defined = r.bitCount(n)
	}
	if r.byte() != 0 && r.err == nil {
		r.fail(unsupportedf("external file data"))
	}
	r.take(defined * width)
}

// parse reads a plain header (without its leading id) and checks it as a whole.
func (p *headerParser) parse(r *hdrReader, packed int64) (*hdrInfo, uint64, error) {
	bud := &budget{limit: p.cfg.maxHeadMem}
	// The header bytes stay in memory next to what the library builds from them.
	if err := bud.charge(uint64(len(r.b)), 2); err != nil {
		return nil, 0, err
	}
	h := &hdrInfo{}
	id := r.byte()
	if id == idArchiveProperties || id == idAdditionalStreams {
		return nil, 0, unsupportedf("archive properties")
	}
	if id == idMainStreamsInfo {
		s, err := p.parseStreamsInfo(r, bud)
		if err != nil {
			return nil, 0, err
		}
		h.streams = s
		id = r.byte()
	}
	if id == idFilesInfo {
		f, err := p.parseFilesInfo(r, bud)
		if err != nil {
			return nil, 0, err
		}
		h.files = f
		id = r.byte()
	}
	if r.err != nil {
		return nil, 0, r.err
	}
	if id != idEnd {
		return nil, 0, corruptf("unexpected id %#x in header", id)
	}
	if r.remaining() != 0 {
		return nil, 0, corruptf("%d bytes after the end of the header", r.remaining())
	}
	if err := p.checkHeader(h); err != nil {
		return nil, 0, err
	}
	declared := declaredUnpack(h.streams)
	if err := p.checkDeclared(declared, packed); err != nil {
		return nil, 0, err
	}
	return h, declared, nil
}

// checkHeader checks that files and streams agree.
func (p *headerParser) checkHeader(h *hdrInfo) error {
	if h.files == nil {
		return nil
	}
	withStream := h.files.count - min(h.files.emptyStreams, h.files.count)
	var have uint64
	if h.streams != nil {
		have = h.streams.streams()
	}
	if withStream != have {
		return corruptf("%d files have data but the folders hold %d streams", withStream, have)
	}
	return nil
}

func declaredUnpack(s *zStreams) uint64 {
	if s == nil {
		return 0
	}
	var total uint64
	for i := range s.folders {
		total = satAdd(total, s.folders[i].unpackSize())
	}
	return total
}

// checkDeclared applies the size guards of the extractor to the sizes the
// header declares, so a hostile archive fails before the library opens it.
func (p *headerParser) checkDeclared(declared uint64, packed int64) error {
	if declared > math.MaxInt64 {
		return corruptf("unpack size of %d bytes", declared)
	}
	d := int64(declared)
	if p.cfg.maxBytes > 0 && d > p.cfg.maxBytes {
		return fmt.Errorf("%w: archive declares %d bytes, cap is %d", ErrTooLarge, d, p.cfg.maxBytes)
	}
	if d > p.cfg.bombMin && d/max(packed, 1) > p.cfg.bombRatio {
		return fmt.Errorf("%w: archive of %d bytes declares %d", ErrBomb, packed, d)
	}
	return nil
}

func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}
