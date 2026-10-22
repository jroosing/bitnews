package unpack

import (
	"bytes"
	"compress/bzip2"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"unicode/utf16"

	"github.com/ulikunitz/xz/lzma"
)

// 7z header pre-flight.
//
// github.com/bodgit/sevenzip allocates from counts the archive header
// declares (files, folders, streams, coder properties, CRC tables) before it
// has read the bytes that should back them: a 100-byte archive can make it
// request 240 GiB, and a compressed header can inflate into hundreds of MiB
// of file records. An out-of-memory kill takes the whole process down, and
// Go cannot recover from it.
//
// So the library never sees an archive that has not passed preflight7z. It
// reads the signature header, the (encoded) header, decodes the header itself
// with a size cap, checks every declared count against the bytes that are
// there and against hard caps, and clamps decoder dictionaries to what the
// stream needs. The library then gets a view of the archive whose header is
// the validated one: the signature header and the header are replaced by the
// bytes preflight7z checked (compressed headers arrive decompressed), the
// packed streams are read from disk as before.

const sigHeaderSize = 32

// Coder methods, as method keys.
var (
	mCopy    = methodKey([]byte{0x00})
	mDelta   = methodKey([]byte{0x03})
	mLZMA    = methodKey([]byte{0x03, 0x01, 0x01})
	mLZMA2   = methodKey([]byte{0x21})
	mPPMd    = methodKey([]byte{0x03, 0x04, 0x01})
	mAES     = methodKey([]byte{0x06, 0xf1, 0x07, 0x01})
	mDeflate = methodKey([]byte{0x04, 0x01, 0x08})
	mBZip2   = methodKey([]byte{0x04, 0x02, 0x02})

	// Methods the library decodes and that have no property worth checking.
	otherMethods = map[uint64]bool{
		methodKey([]byte{0x03, 0x03, 0x01, 0x03}): true, // BCJ
		methodKey([]byte{0x03, 0x03, 0x01, 0x1b}): true, // BCJ2
		methodKey([]byte{0x03, 0x03, 0x02, 0x05}): true, // PPC
		methodKey([]byte{0x03, 0x03, 0x05, 0x01}): true, // ARM
		methodKey([]byte{0x03, 0x03, 0x08, 0x05}): true, // SPARC
		methodKey([]byte{0x0a}):                   true, // ARM64
		methodKey([]byte{0x04, 0xf7, 0x11, 0x01}): true, // Zstandard
		methodKey([]byte{0x04, 0xf7, 0x11, 0x02}): true, // Brotli
		methodKey([]byte{0x04, 0xf7, 0x11, 0x04}): true, // LZ4
	}
)

// minDict is the smallest LZMA dictionary.
const minDict = 4096

// maxAESCycles is the key derivation cost cap of the library (2^24 rounds).
const maxAESCycles = 24

// headerOverlay is the archive as the library sees it: a signature header and
// a header that preflight7z validated around the packed streams of the file.
type headerOverlay struct {
	sig  [sigHeaderSize]byte
	body io.ReaderAt // the original archive, for the packed streams
	off  int64       // length of the packed streams
	hdr  []byte
}

func (o *headerOverlay) size() int64 { return sigHeaderSize + o.off + int64(len(o.hdr)) }

// ReadAt serves the signature header and the header from memory and
// everything between from the original archive.
func (o *headerOverlay) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("unpack: negative offset")
	}
	n := 0
	for len(p) > 0 && off < o.size() {
		var k int
		var err error
		switch {
		case off < sigHeaderSize:
			k = copy(p, o.sig[off:])
		case off < sigHeaderSize+o.off:
			end := min(int64(len(p)), sigHeaderSize+o.off-off)
			k, err = o.body.ReadAt(p[:end], off)
			if err != nil && !(errors.Is(err, io.EOF) && k == int(end)) {
				return n + k, err
			}
		default:
			k = copy(p, o.hdr[off-sigHeaderSize-o.off:])
		}
		n += k
		p = p[k:]
		off += int64(k)
		if k == 0 {
			break
		}
	}
	if len(p) > 0 {
		return n, io.EOF
	}
	return n, nil
}

// makeSignatureHeader builds the 32 bytes that start an archive.
func makeSignatureHeader(nextOff, nextSize uint64, nextCRC uint32) (sig [sigHeaderSize]byte) {
	copy(sig[:], magic7z)
	sig[7] = 4 // minor version
	binary.LittleEndian.PutUint64(sig[12:], nextOff)
	binary.LittleEndian.PutUint64(sig[20:], nextSize)
	binary.LittleEndian.PutUint32(sig[28:], nextCRC)
	binary.LittleEndian.PutUint32(sig[8:], crc32.ChecksumIEEE(sig[12:]))
	return sig
}

// preflight7z validates the archive in r (size bytes, packed bytes on disk)
// and returns the view to hand to the library. Every error is classified.
func preflight7z(r io.ReaderAt, size int64, cfg *config, packed int64) (*headerOverlay, error) {
	if size < sigHeaderSize {
		return nil, corruptf("archive of %d bytes is shorter than the signature header", size)
	}
	var sig [sigHeaderSize]byte
	if err := readFull(r, sig[:], 0); err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(sig[:], magic7z) {
		return nil, corruptf("no 7z signature at the start of the archive")
	}
	if sig[6] != 0 {
		return nil, unsupportedf("format version %d.%d", sig[6], sig[7])
	}
	if crc32.ChecksumIEEE(sig[12:]) != binary.LittleEndian.Uint32(sig[8:]) {
		return nil, corruptf("start header checksum mismatch")
	}
	nextOff := binary.LittleEndian.Uint64(sig[12:])
	nextSize := binary.LittleEndian.Uint64(sig[20:])
	nextCRC := binary.LittleEndian.Uint32(sig[28:])

	avail := uint64(size - sigHeaderSize)
	if nextOff > avail || nextSize > avail-nextOff {
		return nil, corruptf("header at %d, %d bytes, lies beyond the %d bytes of the archive", nextOff, nextSize, size)
	}
	if nextSize == 0 {
		return nil, corruptf("archive has no header")
	}
	if nextSize > uint64(cfg.maxHeader) {
		return nil, fmt.Errorf("%w: 7z header of %d bytes, cap is %d", ErrTooLarge, nextSize, cfg.maxHeader)
	}
	buf := make([]byte, nextSize)
	if err := readFull(r, buf, sigHeaderSize+int64(nextOff)); err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(buf) != nextCRC {
		return nil, corruptf("header checksum mismatch")
	}

	p := &headerParser{cfg: cfg, streamRegion: nextOff}
	plain := buf
	var enc decodeState
	var err error
	if buf[0] == idEncodedHeader {
		plain, enc, err = p.decodeEncodedHeader(r, buf[1:])
	}
	if err == nil && plain[0] != idHeader {
		err = corruptf("unexpected id %#x at the start of the header", plain[0])
	}
	if err == nil {
		_, _, err = p.parse(&hdrReader{b: plain[1:]}, packed)
	}
	if err != nil {
		if enc.aes && !enc.verified {
			return nil, passwordError(cfg, err)
		}
		return nil, err
	}

	// The dictionary clamps edited the header: seal it again.
	sig = makeSignatureHeader(nextOff, uint64(len(plain)), crc32.ChecksumIEEE(plain))
	return &headerOverlay{sig: sig, body: r, off: int64(nextOff), hdr: plain}, nil
}

func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return corruptf("archive ends at %d, expected %d more bytes", off+int64(n), len(p)-n)
	}
	return err
}

func passwordError(cfg *config, why error) error {
	if cfg.password == "" {
		return fmt.Errorf("%w: archive is encrypted: %v", ErrPassword, why)
	}
	return fmt.Errorf("%w: %v", ErrPassword, why)
}

// decodeState says what the decoding of an encoded header learned.
type decodeState struct {
	aes      bool // the header is encrypted
	verified bool // its CRC32 matched, so the password is right
}

// decodeEncodedHeader decodes a kEncodedHeader: a StreamsInfo that describes
// the real header as a packed stream. The decoded size is capped before
// anything is decoded, and the decoders are the stdlib and xz ones with a
// limit on their output.
func (p *headerParser) decodeEncodedHeader(r io.ReaderAt, body []byte) ([]byte, decodeState, error) {
	var st decodeState
	hr := &hdrReader{b: body}
	s, err := p.parseStreamsInfo(hr, &budget{limit: p.cfg.maxHeadMem})
	if err != nil {
		return nil, st, err
	}
	if hr.remaining() != 0 {
		return nil, st, corruptf("%d bytes after the encoded header info", hr.remaining())
	}
	if len(s.folders) != 1 || s.subCounts != nil {
		return nil, st, corruptf("the encoded header must be one folder")
	}
	f := &s.folders[0]
	if len(f.packed) != 1 || f.numIn != len(f.coders) {
		return nil, st, unsupportedf("encoded header with several packed streams")
	}
	for _, c := range f.coders {
		if c.size > uint64(p.cfg.maxHeader) {
			return nil, st, fmt.Errorf("%w: encoded 7z header declares %d bytes, cap is %d", ErrTooLarge, c.size, p.cfg.maxHeader)
		}
		if c.key == mAES {
			st.aes = true
		}
	}
	packSize := s.packSizes[0]
	if packSize > uint64(p.cfg.maxHeader) {
		return nil, st, fmt.Errorf("%w: encoded 7z header packed into %d bytes, cap is %d", ErrTooLarge, packSize, p.cfg.maxHeader)
	}
	packed := make([]byte, packSize)
	if err := readFull(r, packed, sigHeaderSize+int64(s.packPos)); err != nil {
		return nil, st, err
	}

	var stream io.Reader = bytes.NewReader(packed)
	cur, steps := f.packed[0], 0
	for {
		steps++
		c := f.coders[cur]
		stream, err = p.coderReader(c, stream)
		if err != nil {
			return nil, st, err
		}
		stream = io.LimitReader(stream, int64(c.size))
		if cur == f.mainOut {
			break
		}
		next := -1
		for _, bp := range f.bind {
			if bp[1] == cur {
				next = bp[0]
			}
		}
		if next < 0 || steps >= len(f.coders) {
			return nil, st, corruptf("the coders of the encoded header do not form a chain")
		}
		cur = next
	}
	if steps != len(f.coders) {
		return nil, st, corruptf("the coders of the encoded header do not form a chain")
	}
	want := f.unpackSize()
	out, err := io.ReadAll(stream)
	if err != nil {
		return nil, st, corruptf("decoding the header: %v", err)
	}
	if uint64(len(out)) != want {
		return nil, st, corruptf("header decodes to %d bytes, declared %d", len(out), want)
	}
	if f.crc.ok {
		if crc32.ChecksumIEEE(out) != f.crc.v {
			return nil, st, corruptf("decoded header checksum mismatch")
		}
		st.verified = true
	}
	if len(out) == 0 {
		return nil, st, corruptf("empty header")
	}
	return out, st, nil
}

// coderReader returns the decoder of one coder of an encoded header.
func (p *headerParser) coderReader(c zCoder, in io.Reader) (io.Reader, error) {
	switch c.key {
	case mCopy:
		return in, nil
	case mLZMA:
		// checkCoders clamped the dictionary, so this allocates what the
		// header needs, at most.
		var h [13]byte
		copy(h[:], c.props[:5])
		binary.LittleEndian.PutUint64(h[5:], c.size)
		lr, err := lzma.NewReader(io.MultiReader(bytes.NewReader(h[:]), in))
		if err != nil {
			return nil, corruptf("lzma: %v", err)
		}
		return lr, nil
	case mLZMA2:
		cfg := lzma.Reader2Config{DictCap: int(min(lzma2Dict(c.props[0]), uint64(p.cfg.maxHeader)+minDict))}
		lr, err := cfg.NewReader2(in)
		if err != nil {
			return nil, corruptf("lzma2: %v", err)
		}
		return lr, nil
	case mDeflate:
		return flate.NewReader(in), nil
	case mBZip2:
		return bzip2.NewReader(in), nil
	case mAES:
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, corruptf("aes: %v", err)
		}
		plain, err := aesDecrypt(c.props, data, p.cfg.password)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(plain), nil
	}
	return nil, unsupportedf("method %x for an encoded header", c.key&(1<<56-1))
}

// lzma2Dict is the dictionary size LZMA2 stores in one byte.
func lzma2Dict(b byte) uint64 {
	if b > 40 {
		return 0
	}
	return uint64(2|b&1) << (b/2 + 11)
}

// checkCoders checks the coders of a folder against what the decoders allocate
// and clamps the LZMA and LZMA2 dictionaries in place to the size of the
// stream: a stream of n bytes never looks back further than n bytes, so a
// dictionary beyond that only costs memory.
func (p *headerParser) checkCoders(f *zFolder) error {
	for i := range f.coders {
		c := &f.coders[i]
		switch c.key {
		case mCopy, mDeflate, mBZip2:
		case mDelta:
			if len(c.props) != 1 {
				return corruptf("delta coder with %d property bytes", len(c.props))
			}
		case mLZMA:
			if len(c.props) != 5 || c.props[0] >= 9*5*5 {
				return corruptf("invalid LZMA properties")
			}
			dict := uint64(binary.LittleEndian.Uint32(c.props[1:]))
			need := min(dict, max(c.size, minDict))
			if err := p.checkDict(need); err != nil {
				return err
			}
			if dict > need {
				binary.LittleEndian.PutUint32(c.props[1:], uint32(need))
			}
		case mLZMA2:
			if len(c.props) != 1 || c.props[0] > 40 {
				return corruptf("invalid LZMA2 properties")
			}
			need := min(lzma2Dict(c.props[0]), max(c.size, minDict))
			if err := p.checkDict(need); err != nil {
				return err
			}
			b := c.props[0]
			for b > 0 && lzma2Dict(b-1) >= need {
				b--
			}
			c.props[0] = b
		case mPPMd:
			if len(c.props) != 5 || c.props[0] < 2 || c.props[0] > 64 {
				return corruptf("invalid PPMd properties")
			}
			mem := uint64(binary.LittleEndian.Uint32(c.props[1:]))
			if mem < 1<<11 || mem > 0xffffffff-12*3 {
				return corruptf("invalid PPMd memory size %d", mem)
			}
			if err := p.checkDict(mem); err != nil {
				return err
			}
		case mAES:
			if len(c.props) < 2 || len(c.props) > 2+32 {
				return corruptf("invalid AES properties")
			}
			if cycles := c.props[0] & 0x3f; cycles > maxAESCycles && cycles != 0x3f {
				return unsupportedf("AES key derivation with 2^%d rounds", cycles)
			}
		default:
			if !otherMethods[c.key] {
				return unsupportedf("coder method %x", c.key&(1<<56-1))
			}
		}
	}
	return nil
}

func (p *headerParser) checkDict(need uint64) error {
	if p.cfg.maxDict < 0 || need > uint64(p.cfg.maxDict) {
		return fmt.Errorf("%w: decoder needs %d bytes of memory, cap is %d", ErrUnsupported, need, p.cfg.maxDict)
	}
	return nil
}

// aesDecrypt decrypts a 7zAES stream (AES-256-CBC, key from SHA-256 rounds over
// salt, password and a counter), as the library does for the data streams.
func aesDecrypt(props, data []byte, password string) ([]byte, error) {
	saltLen := int(props[0]>>7&1 + props[1]>>4)
	ivLen := int(props[0]>>6&1 + props[1]&0x0f)
	if len(props) != 2+saltLen+ivLen {
		return nil, corruptf("invalid AES properties")
	}
	salt := props[2 : 2+saltLen]
	iv := make([]byte, aes.BlockSize)
	copy(iv, props[2+saltLen:])

	pw := make([]byte, 0, 2*len(password))
	for _, u := range utf16.Encode([]rune(password)) {
		pw = append(pw, byte(u), byte(u>>8))
	}
	material := append(append([]byte{}, salt...), pw...)
	key := make([]byte, sha256.Size)
	if cycles := props[0] & 0x3f; cycles == 0x3f {
		copy(key, material)
	} else {
		h := sha256.New()
		var ctr [8]byte
		for i := range uint64(1) << cycles {
			binary.LittleEndian.PutUint64(ctr[:], i)
			h.Write(material)
			h.Write(ctr[:])
		}
		h.Sum(key[:0])
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, corruptf("aes: %v", err)
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, corruptf("encrypted header of %d bytes is not a whole number of blocks", len(data))
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	return out, nil
}
