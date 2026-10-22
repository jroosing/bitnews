package unpack

// A small writer for RAR archives that use the "store" method, so the tests
// can build the archives they need (multi-volume sets, hostile names, links,
// encrypted files) without a RAR tool. The layouts follow the public format
// descriptions (RAR 4.x technote and RAR 5.0 technote) and what
// 7-Zip reads. Stored files are not compressed; the decoders are
// covered by the real archives in testdata.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"hash/crc32"
	"unicode/utf16"
)

// Unix mode type bits as RAR stores them in the attributes.
const (
	modeReg     = 0x8000
	modeDir     = 0x4000
	modeSymlink = 0xA000
	modeFIFO    = 0x1000
	modeChar    = 0x2000
	modeSocket  = 0xC000
)

// linkKind is a RAR5 redirection record type.
type linkKind int

const (
	linkNone linkKind = iota
	linkUnixSymlink
	linkWinSymlink
	linkJunction
	linkHardlink
	linkFileCopy
)

type rarEntry struct {
	name string
	data []byte // content; for a link, the target
	dir  bool
	// attr is the Unix mode including type bits; zero means a plain 0644 file
	// or 0755 directory.
	attr uint32
	link linkKind // RAR5 only
	// password encrypts the data of this file (RAR4: AES-128 with a salted
	// SHA-1 key; RAR5: AES-256 with PBKDF2-HMAC-SHA256).
	password string
}

func (e rarEntry) mode() uint32 {
	switch {
	case e.attr != 0:
		return e.attr
	case e.dir:
		return modeDir | 0o755
	}
	return modeReg | 0o644
}

// rarSpec controls the layout.
type rarSpec struct {
	// perVolume is the number of data bytes after which a new volume starts;
	// zero builds a single volume.
	perVolume int
	// newNaming marks the volumes as .partNN.rar (RAR4 only; RAR5 always is).
	newNaming bool
	// solid sets the solid archive flag (the data is still stored).
	solid bool
}

// ---- RAR 4.x ----

func rar4Header(typ byte, flags uint16, body []byte) []byte {
	size := 7 + len(body)
	h := make([]byte, 0, size)
	h = append(h, typ, byte(flags), byte(flags>>8), byte(size), byte(size>>8))
	h = append(h, body...)
	crc := crc32.ChecksumIEEE(h)
	return append([]byte{byte(crc), byte(crc >> 8)}, h...)
}

func rar4FileHeader(e rarEntry, split int, unpSize uint32, fileCRC uint32, packed []byte, salt []byte) []byte {
	const (
		splitBefore = 0x0001
		splitAfter  = 0x0002
		encrypted   = 0x0004
		windowDir   = 0x00e0
		hasSalt     = 0x0400
		hasData     = 0x8000
	)
	flags := uint16(hasData)
	if split&1 != 0 {
		flags |= splitBefore
	}
	if split&2 != 0 {
		flags |= splitAfter
	}
	if e.dir {
		flags |= windowDir
	}
	if salt != nil {
		flags |= encrypted | hasSalt
	}
	name := []byte(e.name)
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w(uint32(len(packed)))
	w(unpSize)
	b.WriteByte(3) // host OS: Unix
	w(fileCRC)
	w(uint32(0x5a21_0000)) // an arbitrary DOS time
	b.WriteByte(29)        // unpack version
	b.WriteByte(0x30)      // method: store
	w(uint16(len(name)))
	w(e.mode())
	b.Write(name)
	if salt != nil {
		b.Write(salt)
	}
	return rar4Header(0x74, flags, b.Bytes())
}

// calcAes30 mirrors the RAR 3.x/4.x key derivation.
func calcAes30(password string, salt []byte) (key, iv []byte) {
	var p []byte
	for _, v := range utf16.Encode([]rune(password)) {
		p = append(p, byte(v), byte(v>>8))
	}
	p = append(p, salt...)
	h := sha1.New()
	iv = make([]byte, 16)
	const rounds = 0x40000
	s := make([]byte, h.Size())
	b := s[:3]
	for i := range rounds {
		h.Write(p)
		b[0], b[1], b[2] = byte(i), byte(i>>8), byte(i>>16)
		h.Write(b)
		if i%(rounds/16) == 0 {
			s = h.Sum(s[:0])
			iv[i/(rounds/16)] = s[4*4+3]
		}
	}
	key = h.Sum(s[:0])[:16]
	for k := key; len(k) >= 4; k = k[4:] {
		k[0], k[1], k[2], k[3] = k[3], k[2], k[1], k[0]
	}
	return key, iv
}

func cbcEncrypt(key, iv, data []byte) []byte {
	pad := (16 - len(data)%16) % 16
	buf := append(append([]byte{}, data...), make([]byte, pad)...)
	blk, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(buf, buf)
	return buf
}

// part is the slice of one entry's data that goes into one volume.
type part struct {
	e     rarEntry
	split int // bit 0: continues from the previous volume, bit 1: continues in the next
	data  []byte
}

// splitIntoVolumes distributes the entries over volumes of about perVolume
// data bytes. Directories and empty files never split.
func splitIntoVolumes(entries []rarEntry, perVolume int) [][]part {
	if perVolume <= 0 {
		var all []part
		for _, e := range entries {
			all = append(all, part{e: e, data: e.data})
		}
		return [][]part{all}
	}
	vols := [][]part{{}}
	room := perVolume
	for _, e := range entries {
		data := e.data
		first := true
		for {
			n := min(len(data), room)
			last := n == len(data)
			p := part{e: e, data: data[:n]}
			if !first {
				p.split |= 1
			}
			if !last {
				p.split |= 2
			}
			vols[len(vols)-1] = append(vols[len(vols)-1], p)
			data = data[n:]
			room -= n
			first = false
			if last {
				break
			}
			vols = append(vols, []part{})
			room = perVolume
		}
		if room == 0 {
			vols = append(vols, []part{})
			room = perVolume
		}
	}
	if len(vols[len(vols)-1]) == 0 && len(vols) > 1 {
		vols = vols[:len(vols)-1]
	}
	return vols
}

// partCRC is the CRC a volume's file header carries: the CRC of the part for a
// file that goes on in the next volume, the CRC of the whole file in the last
// part (this is what 7-Zip and unrar check).
func partCRC(p part) uint32 {
	if p.e.link != linkNone {
		return crc32.ChecksumIEEE(nil) // a link has no content
	}
	if p.split&2 != 0 {
		return crc32.ChecksumIEEE(p.data)
	}
	return crc32.ChecksumIEEE(p.e.data)
}

// buildRAR4 returns one byte slice per volume.
func buildRAR4(entries []rarEntry, spec rarSpec) [][]byte {
	vols := splitIntoVolumes(entries, spec.perVolume)
	multi := len(vols) > 1
	var out [][]byte
	for vi, parts := range vols {
		var b bytes.Buffer
		b.Write(magicRAR4)
		var arcFlags uint16
		if multi {
			arcFlags |= 0x0001
		}
		if spec.newNaming {
			arcFlags |= 0x0010
		}
		if spec.solid {
			arcFlags |= 0x0008
		}
		b.Write(rar4Header(0x73, arcFlags, make([]byte, 6)))
		for _, p := range parts {
			whole := p.e.data
			fileCRC := partCRC(p)
			content := p.data
			var salt []byte
			if p.e.password != "" {
				salt = []byte{1, 2, 3, 4, 5, 6, 7, byte(vi)}
				key, iv := calcAes30(p.e.password, salt)
				content = cbcEncrypt(key, iv, content)
			}
			b.Write(rar4FileHeader(p.e, p.split, uint32(len(whole)), fileCRC, content, salt))
			b.Write(content)
		}
		var endFlags uint16 = 0x4000
		if multi && vi < len(vols)-1 {
			endFlags |= 0x0001
		}
		b.Write(rar4Header(0x7b, endFlags, nil))
		out = append(out, b.Bytes())
	}
	return out
}

// ---- RAR 5 ----

func vint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// rar5Block assembles a block from its header-flag-dependent fields.
func rar5Block(typ, flags uint64, extra []byte, dataSize uint64, specific []byte) []byte {
	var h []byte
	h = append(h, vint(typ)...)
	if len(extra) > 0 {
		flags |= 0x1
	}
	if dataSize > 0 {
		flags |= 0x2
	}
	h = append(h, vint(flags)...)
	if len(extra) > 0 {
		h = append(h, vint(uint64(len(extra)))...)
	}
	if dataSize > 0 {
		h = append(h, vint(dataSize)...)
	}
	h = append(h, specific...)
	h = append(h, extra...)
	sized := append(vint(uint64(len(h))), h...)
	crc := crc32.ChecksumIEEE(sized)
	return append(binary.LittleEndian.AppendUint32(nil, crc), sized...)
}

func rar5Record(typ uint64, data []byte) []byte {
	body := append(vint(typ), data...)
	return append(vint(uint64(len(body))), body...)
}

// rar5Keys mirrors the RAR 5 key derivation: the AES key and the password
// check value.
func rar5Keys(password string, salt []byte, log2count int) (key, check []byte) {
	count := 1 << log2count
	key, err := pbkdf2.Key(sha256.New, password, salt, count, 32)
	if err != nil {
		panic(err)
	}
	raw, err := pbkdf2.Key(sha256.New, password, salt, count+32, 32)
	if err != nil {
		panic(err)
	}
	folded := make([]byte, 8)
	copy(folded, raw[:8])
	for i, v := range raw[8:] {
		folded[i&7] ^= v
	}
	sum := sha256.Sum256(folded)
	return key, append(folded, sum[:4]...)
}

func rar5FileBlock(e rarEntry, split int, unpSize uint64, fileCRC uint32, packed []byte, enc *rar5Enc) []byte {
	var flags uint64
	if split&1 != 0 {
		flags |= 0x8
	}
	if split&2 != 0 {
		flags |= 0x10
	}
	var fileFlags uint64 = 0x4 // CRC32 present
	if e.dir {
		fileFlags |= 0x1
	}
	var sp []byte
	sp = append(sp, vint(fileFlags)...)
	sp = append(sp, vint(unpSize)...)
	sp = append(sp, vint(uint64(e.mode()))...)
	sp = binary.LittleEndian.AppendUint32(sp, fileCRC)
	sp = append(sp, vint(0)...) // compression info: version 0, store
	sp = append(sp, vint(1)...) // host OS: Unix
	sp = append(sp, vint(uint64(len(e.name)))...)
	sp = append(sp, e.name...)
	var extra []byte
	if enc != nil {
		rec := vint(0)                // encryption version
		rec = append(rec, vint(1)...) // flags: password check present
		rec = append(rec, byte(enc.log2))
		rec = append(rec, enc.salt...)
		rec = append(rec, enc.iv...)
		rec = append(rec, enc.check...)
		extra = append(extra, rar5Record(1, rec)...)
	}
	if e.link != linkNone {
		rec := vint(uint64(e.link))
		rec = append(rec, vint(0)...)
		rec = append(rec, vint(uint64(len(e.data)))...)
		rec = append(rec, e.data...)
		extra = append(extra, rar5Record(5, rec)...)
	}
	return rar5Block(2, flags, extra, uint64(len(packed)), sp)
}

type rar5Enc struct {
	log2  int
	salt  []byte
	iv    []byte
	check []byte
	key   []byte
}

func buildRAR5(entries []rarEntry, spec rarSpec) [][]byte {
	vols := splitIntoVolumes(entries, spec.perVolume)
	multi := len(vols) > 1
	var out [][]byte
	for vi, parts := range vols {
		var b bytes.Buffer
		b.Write(magicRAR5)
		var arcFlags uint64
		var sp []byte
		if multi {
			arcFlags |= 0x1
		}
		if spec.solid {
			arcFlags |= 0x4
		}
		if vi > 0 {
			arcFlags |= 0x2
		}
		sp = append(sp, vint(arcFlags)...)
		if vi > 0 {
			sp = append(sp, vint(uint64(vi))...)
		}
		b.Write(rar5Block(1, 0, nil, 0, sp))
		for _, p := range parts {
			content := p.data
			link := p.e.link != linkNone
			if link {
				content = nil // a link's target lives in the redirection record
			}
			var enc *rar5Enc
			if p.e.password != "" {
				enc = &rar5Enc{log2: 4, salt: bytes.Repeat([]byte{7}, 16), iv: bytes.Repeat([]byte{9}, 16)}
				enc.key, enc.check = rar5Keys(p.e.password, enc.salt, enc.log2)
				content = cbcEncrypt(enc.key, enc.iv, content)
			}
			whole := p.e.data
			if link {
				whole = nil
			}
			b.Write(rar5FileBlock(p.e, p.split, uint64(len(whole)), partCRC(p), content, enc))
			b.Write(content)
		}
		var endFlags uint64
		if multi && vi < len(vols)-1 {
			endFlags = 1
		}
		b.Write(rar5Block(5, 0, nil, 0, vint(endFlags)))
		out = append(out, b.Bytes())
	}
	return out
}
