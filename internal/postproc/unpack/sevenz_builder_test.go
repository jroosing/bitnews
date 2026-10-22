package unpack

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"unicode/utf16"

	"github.com/ulikunitz/xz/lzma"
)

// Builders for 7z archives, valid and hostile. The hostile ones carry correct
// CRCs on purpose: the checksums only protect against accidents, a crafted
// archive always has them right.

// Coder ids for the builders.
const (
	zIDLZMA  = "\x03\x01\x01"
	zIDCopy  = "\x00"
	zIDLZMA2 = "\x21"
)

// zb is a byte builder with the 7z number encoding.
type zb struct{ bytes.Buffer }

func (b *zb) u8(v ...byte) *zb { b.Write(v); return b }

func (b *zb) u32(v uint32) *zb {
	_ = binary.Write(b, binary.LittleEndian, v)
	return b
}

// num writes a 7z variable length number.
func (b *zb) num(v uint64) *zb {
	for i := range 8 {
		if v < 1<<(7*(uint(i)+1)) {
			first := byte(0xff << (8 - uint(i)))
			b.WriteByte(first | byte(v>>(8*uint(i))))
			for j := range i {
				b.WriteByte(byte(v >> (8 * uint(j))))
			}
			return b
		}
	}
	b.WriteByte(0xff)
	_ = binary.Write(b, binary.LittleEndian, v)
	return b
}

func (b *zb) raw(p []byte) *zb { b.Write(p); return b }

// signatureHeader is the 32-byte start of an archive, with both CRCs right.
func signatureHeader(nextOff, nextSize uint64, nextCRC uint32) []byte {
	var start [20]byte
	binary.LittleEndian.PutUint64(start[0:], nextOff)
	binary.LittleEndian.PutUint64(start[8:], nextSize)
	binary.LittleEndian.PutUint32(start[16:], nextCRC)
	out := append([]byte("7z\xbc\xaf\x27\x1c\x00\x04"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[8:], crc32.ChecksumIEEE(start[:]))
	return append(out, start[:]...)
}

// assemble7z puts streams and header behind a signature header whose fields
// all match.
func assemble7z(streams, header []byte) []byte {
	out := signatureHeader(uint64(len(streams)), uint64(len(header)), crc32.ChecksumIEEE(header))
	out = append(out, streams...)
	return append(out, header...)
}

// z7file is a file for the valid builders.
type z7file struct {
	name string
	data []byte
	dir  bool
}

func utf16le(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// filesInfo writes the FilesInfo block for the given files, empty streams
// (directories and empty files) flagged.
func filesInfo(files []z7file) []byte {
	var b zb
	b.u8(idFilesInfo).num(uint64(len(files)))
	var empty []bool
	anyEmpty := false
	for _, f := range files {
		e := f.dir || len(f.data) == 0
		empty = append(empty, e)
		anyEmpty = anyEmpty || e
	}
	if anyEmpty {
		bits := bitVector(empty)
		b.u8(idEmptyStream).num(uint64(len(bits))).raw(bits)
		var emptyFile []bool
		anyFile := false
		for i, f := range files {
			if empty[i] {
				emptyFile = append(emptyFile, !f.dir)
				anyFile = anyFile || !f.dir
			}
		}
		if anyFile {
			bits := bitVector(emptyFile)
			b.u8(idEmptyFile).num(uint64(len(bits))).raw(bits)
		}
	}
	var names []byte
	for _, f := range files {
		names = append(names, utf16le(f.name)...)
		names = append(names, 0, 0)
	}
	b.u8(idName).num(uint64(len(names) + 1)).u8(0).raw(names)
	var attrs zb
	attrs.u8(1, 0)
	for _, f := range files {
		if f.dir {
			attrs.u32(0x10) // FILE_ATTRIBUTE_DIRECTORY
		} else {
			attrs.u32(0x20)
		}
	}
	b.u8(idWinAttributes).num(uint64(attrs.Len())).raw(attrs.Bytes())
	b.u8(idEnd)
	return b.Bytes()
}

func bitVector(v []bool) []byte {
	out := make([]byte, (len(v)+7)/8)
	for i, x := range v {
		if x {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}

// storedStreams is the content of the non-empty files in one stream and the
// StreamsInfo that describes it as one solid folder with the given coder.
func streamsInfoFor(files []z7file, coderID string, props []byte, packed, unpacked []byte) []byte {
	var sizes []uint64
	var crcs []uint32
	for _, f := range files {
		if f.dir || len(f.data) == 0 {
			continue
		}
		sizes = append(sizes, uint64(len(f.data)))
		crcs = append(crcs, crc32.ChecksumIEEE(f.data))
	}
	var b zb
	b.u8(idMainStreamsInfo)
	b.u8(idPackInfo).num(0).num(1).u8(idSize).num(uint64(len(packed))).u8(idEnd)
	b.u8(idUnpackInfo).u8(idFolder).num(1).u8(0)
	b.num(1) // one coder
	flags := byte(len(coderID))
	if len(props) > 0 {
		flags |= 0x20
	}
	b.u8(flags).raw([]byte(coderID))
	if len(props) > 0 {
		b.num(uint64(len(props))).raw(props)
	}
	b.u8(idCodersUnpackSize).num(uint64(len(unpacked))).u8(idEnd)
	if len(sizes) > 0 {
		b.u8(idSubStreamsInfo).u8(idNumUnpackStream).num(uint64(len(sizes)))
		if len(sizes) > 1 {
			b.u8(idSize)
			for _, s := range sizes[:len(sizes)-1] {
				b.num(s)
			}
		}
		b.u8(idCRC).u8(1)
		for _, c := range crcs {
			b.u32(c)
		}
		b.u8(idEnd)
	}
	b.u8(idEnd)
	return b.Bytes()
}

func joinData(files []z7file) []byte {
	var out []byte
	for _, f := range files {
		if !f.dir {
			out = append(out, f.data...)
		}
	}
	return out
}

// build7zStored builds a valid archive with a plain header and the files
// stored by the copy coder in one solid folder.
func build7zStored(files ...z7file) []byte {
	data := joinData(files)
	var h zb
	h.u8(idHeader)
	if len(data) > 0 {
		h.raw(streamsInfoFor(files, zIDCopy, nil, data, data))
	}
	h.raw(filesInfo(files)).u8(idEnd)
	return assemble7z(data, h.Bytes())
}

// lzmaRaw compresses data into a raw LZMA stream as 7z stores it: the five
// property bytes are returned separately and the 13 byte .lzma header is cut.
func lzmaRaw(data []byte) (props, packed []byte) {
	var buf bytes.Buffer
	w, err := lzma.WriterConfig{Size: int64(len(data)), DictCap: 1 << 16}.NewWriter(&buf)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	b := buf.Bytes()
	return b[:5], b[13:]
}

// build7zEncodedHeader builds a valid archive like build7zStored, but with the
// header compressed by LZMA, the way 7-Zip writes it.
func build7zEncodedHeader(files ...z7file) []byte {
	data := joinData(files)
	var h zb
	h.u8(idHeader)
	if len(data) > 0 {
		h.raw(streamsInfoFor(files, zIDCopy, nil, data, data))
	}
	h.raw(filesInfo(files)).u8(idEnd)
	return encodeHeader(data, h.Bytes())
}

// encodeHeader compresses a plain header and writes the kEncodedHeader that
// points at it, after the streams.
func encodeHeader(streams, plain []byte) []byte {
	props, packed := lzmaRaw(plain)
	return assemble7z(append(append([]byte{}, streams...), packed...), encodedHeaderBlock(uint64(len(streams)), props, packed, uint64(len(plain)), crc32.ChecksumIEEE(plain)))
}

// encodedHeaderBlock is the kEncodedHeader StreamsInfo for one LZMA folder.
func encodedHeaderBlock(packPos uint64, props, packed []byte, unpackSize uint64, crc uint32) []byte {
	var e zb
	e.u8(idEncodedHeader)
	e.u8(idPackInfo).num(packPos).num(1).u8(idSize).num(uint64(len(packed))).u8(idEnd)
	e.u8(idUnpackInfo).u8(idFolder).num(1).u8(0)
	e.num(1).u8(byte(len(zIDLZMA)) | 0x20).raw([]byte(zIDLZMA)).num(uint64(len(props))).raw(props)
	e.u8(idCodersUnpackSize).num(unpackSize)
	e.u8(idCRC).u8(1).u32(crc).u8(idEnd)
	e.u8(idEnd)
	return e.Bytes()
}

// Hostile archives. Each helper writes only the part of the header that makes
// the lie, and ends the header there: a reader that allocates from the
// declared count does so before it finds out that the bytes are missing.

// hostileFiles declares n files and nothing else.
func hostileFiles(n uint64) []byte {
	var h zb
	h.u8(idHeader, idFilesInfo).num(n).u8(idEnd, idEnd)
	return assemble7z(nil, h.Bytes())
}

// hostileFolders declares n folders and ends.
func hostileFolders(n uint64) []byte {
	var h zb
	h.u8(idHeader, idMainStreamsInfo, idUnpackInfo, idFolder).num(n).u8(0)
	return assemble7z(nil, h.Bytes())
}

// hostilePackStreams declares n pack streams with an all-defined CRC table.
func hostilePackStreams(n uint64) []byte {
	var h zb
	h.u8(idHeader, idMainStreamsInfo, idPackInfo).num(0).num(n).u8(idCRC, 1)
	return assemble7z(nil, h.Bytes())
}

// hostileSubStreams is a valid one-folder archive whose SubStreamsInfo
// declares n streams in that folder.
func hostileSubStreams(n uint64) []byte {
	var h zb
	h.u8(idHeader, idMainStreamsInfo)
	h.u8(idPackInfo).num(0).num(1).u8(idSize).num(4).u8(idEnd)
	h.u8(idUnpackInfo, idFolder).num(1).u8(0).num(1).u8(1).raw([]byte(zIDCopy))
	h.u8(idCodersUnpackSize).num(4).u8(idEnd)
	h.u8(idSubStreamsInfo, idNumUnpackStream).num(n).u8(idSize)
	return assemble7z([]byte("abcd"), h.Bytes())
}

// hostileCoderProps declares a coder with a property blob of the given size.
func hostileCoderProps(size uint64) []byte {
	var h zb
	h.u8(idHeader, idMainStreamsInfo, idUnpackInfo, idFolder).num(1).u8(0).num(1)
	h.u8(1 | 0x20).raw([]byte(zIDCopy)).num(size)
	return assemble7z(nil, h.Bytes())
}

// hostileCoderStreams declares one coder with n input streams.
func hostileCoderStreams(n uint64) []byte {
	var h zb
	h.u8(idHeader, idMainStreamsInfo, idUnpackInfo, idFolder).num(1).u8(0).num(1)
	h.u8(1 | 0x10).raw([]byte(zIDCopy)).num(n).num(1)
	return assemble7z(nil, h.Bytes())
}

// hostileCoders declares n coders in one folder.
func hostileCoders(n uint64) []byte {
	var h zb
	h.u8(idHeader, idMainStreamsInfo, idUnpackInfo, idFolder).num(1).u8(0).num(n)
	return assemble7z(nil, h.Bytes())
}

// hostileEncodedHeader is an archive whose header is encoded, with a declared
// decoded size (0: the true size) and a packed stream that decodes to the given
// plain header.
func hostileEncodedHeader(plain []byte, declared uint64) []byte {
	if declared == 0 {
		declared = uint64(len(plain))
	}
	props, packed := lzmaRaw(plain)
	return assemble7z(packed, encodedHeaderBlock(0, props, packed, declared, crc32.ChecksumIEEE(plain)))
}

// namesBombHeader is a plain header that declares n files whose names are all
// empty. It is n*2+40 bytes and compresses to almost nothing.
func namesBombHeader(n uint64) []byte {
	var h zb
	h.u8(idHeader, idFilesInfo).num(n)
	h.u8(idName).num(2*n + 1).u8(0)
	h.raw(make([]byte, 2*n))
	h.u8(idEnd, idEnd)
	return h.Bytes()
}

// manyFiles is a valid archive of n empty files with realistic names,
// attributes and times, the header kind that costs the library the most per
// byte of header.
func manyFiles(n int) []byte {
	var h zb
	h.u8(idHeader, idFilesInfo).num(uint64(n))
	all := make([]bool, n)
	for i := range all {
		all[i] = true
	}
	bits := bitVector(all)
	h.u8(idEmptyStream).num(uint64(len(bits))).raw(bits)
	h.u8(idEmptyFile).num(uint64(len(bits))).raw(bits)
	var names []byte
	for i := range n {
		names = append(names, utf16le(fmt.Sprintf("release/dir%03d/file%07d.txt", i%1000, i))...)
		names = append(names, 0, 0)
	}
	h.u8(idName).num(uint64(len(names) + 1)).u8(0).raw(names)
	h.u8(idMTime).num(uint64(2+8*n)).u8(1, 0).raw(make([]byte, 8*n))
	h.u8(idWinAttributes).num(uint64(2+4*n)).u8(1, 0).raw(make([]byte, 4*n))
	h.u8(idEnd, idEnd)
	return assemble7z(nil, h.Bytes())
}

// lzma2Raw compresses data into a raw LZMA2 stream as 7z stores it.
func lzma2Raw(data []byte) []byte {
	var buf bytes.Buffer
	w, err := lzma.Writer2Config{DictCap: 1 << 16}.NewWriter2(&buf)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// buildWithCoder builds a valid archive with a plain header whose single
// folder uses the given coder, properties and packed stream for the files.
func buildWithCoder(coderID string, props, packed []byte, files ...z7file) []byte {
	var h zb
	h.u8(idHeader)
	h.raw(streamsInfoFor(files, coderID, props, packed, joinData(files)))
	h.raw(filesInfo(files)).u8(idEnd)
	return assemble7z(packed, h.Bytes())
}

// splitInto cuts b into n volumes of about the same size.
func splitInto(b []byte, n int) [][]byte {
	var out [][]byte
	step := (len(b) + n - 1) / n
	for len(b) > 0 {
		k := min(step, len(b))
		out = append(out, b[:k])
		b = b[k:]
	}
	return out
}

// hostileNameTable is a plain header for n files with a kName property that
// claims size bytes and holds none.
func hostileNameTable(n, size uint64) []byte {
	var h zb
	h.u8(idHeader, idFilesInfo).num(n).u8(idName).num(size)
	return assemble7z(nil, h.Bytes())
}

// hostileAttributes is a plain header for n files with an all-defined
// attribute table that holds no values.
func hostileAttributes(n uint64) []byte {
	var h zb
	h.u8(idHeader, idFilesInfo).num(n).u8(idWinAttributes).num(2).u8(1, 0)
	return assemble7z(nil, h.Bytes())
}

// hostilePackSize declares a pack stream of the given size in an archive that
// is a few bytes long.
func hostilePackSize(size uint64) []byte {
	var h zb
	h.u8(idHeader, idMainStreamsInfo)
	h.u8(idPackInfo).num(0).num(1).u8(idSize).num(size).u8(idEnd)
	h.u8(idUnpackInfo, idFolder).num(1).u8(0).num(1).u8(1).raw([]byte(zIDCopy))
	h.u8(idCodersUnpackSize).num(4).u8(idEnd, idEnd)
	return assemble7z([]byte("abcd"), h.Bytes())
}

// declaring7z is a valid archive of one stored file that declares unpackSize
// bytes while holding four.
func declaring7z(unpackSize uint64) []byte {
	var h zb
	h.u8(idHeader, idMainStreamsInfo)
	h.u8(idPackInfo).num(0).num(1).u8(idSize).num(4).u8(idEnd)
	h.u8(idUnpackInfo, idFolder).num(1).u8(0).num(1).u8(1).raw([]byte(zIDCopy))
	h.u8(idCodersUnpackSize).num(unpackSize).u8(idEnd, idEnd)
	h.raw(filesInfo([]z7file{{name: "big", data: []byte("abcd")}})).u8(idEnd)
	return assemble7z([]byte("abcd"), h.Bytes())
}

// withUnknownCoder is a valid archive whose folder uses a method nobody has.
func withUnknownCoder() []byte {
	return buildWithCoder("\x7f\x7f", nil, []byte("abcd"), z7file{name: "a", data: []byte("abcd")})
}
