package yenc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Limits on what a hostile article can make the decoder accept.
const (
	// MaxFileSize is the largest file size a =ybegin may announce (1 TiB).
	MaxFileSize = 1 << 40
	// MaxPartSize is the largest decoded part (16 MiB), which also bounds
	// the memory one Decoder uses.
	MaxPartSize = 16 << 20
)

// ErrMalformed reports a part whose header, trailer or data violates the
// yEnc syntax.
var ErrMalformed = errors.New("yenc: malformed part")

// ErrLength reports a part whose decoded length differs from its declared
// range or trailer size.
var ErrLength = errors.New("yenc: decoded length mismatch")

// ErrRange reports a =ypart range that is empty, inverted or outside the file.
var ErrRange = errors.New("yenc: invalid part range")

// ErrTooLarge reports a file or part larger than MaxFileSize or MaxPartSize.
var ErrTooLarge = errors.New("yenc: file or part too large")

// ErrNoHeader reports an article without a =ybegin line.
var ErrNoHeader = errors.New("yenc: no =ybegin header")

// Part is one decoded yEnc part.
type Part struct {
	// Name is the file name from =ybegin, exactly as posted. It is hostile
	// input and must be sanitized before use as a path.
	Name string
	// Size is the size of the whole file in bytes, from =ybegin.
	Size int64
	// PartNumber and TotalParts are 1 and 1 for a single-part file.
	PartNumber int
	TotalParts int
	// Begin and End are the 1-based inclusive offsets of Data within the
	// file. For a single-part file Begin is 1 and End is Size.
	Begin, End int64
	// Data holds the decoded bytes; len(Data) == End-Begin+1.
	Data []byte
	// CRC32 is the IEEE CRC32 computed over Data.
	CRC32 uint32
	// ExpectedCRC32 is the CRC32 announced by =yend, valid only when
	// HasExpectedCRC is true.
	ExpectedCRC32  uint32
	HasExpectedCRC bool
}

// CRCMatch reports whether the article announced a CRC32 for this part and
// it equals the CRC32 of Data.
func (p Part) CRCMatch() bool {
	return p.HasExpectedCRC && p.CRC32 == p.ExpectedCRC32
}

// Decoder decodes one yEnc part, fed one line at a time. The zero value is
// ready to use.
type Decoder struct {
	state     state
	part      Part
	multipart bool
	buf       []byte // decoded data, sized to the declared part length
	n         int    // bytes of buf in use
	err       error  // first error; once set, every call returns it
}

type state int

const (
	seekingHeader state = iota // lines before =ybegin are ignored
	awaitingPart               // multipart =ybegin seen, =ypart must follow
	inData
	done // =yend seen
)

// Write decodes one line of the article body. A trailing CR LF is ignored.
//
// Lines must already be NNTP dot-unstuffed: the NNTP layer owns that, and
// Write takes data lines literally, so a line starting with ".." holds two
// '.' bytes.
// After an error, the Decoder is unusable and every call returns that error.
func (d *Decoder) Write(line []byte) error {
	if d.err == nil {
		d.err = d.write(line)
		if d.err != nil {
			d.buf = nil
		}
	}
	return d.err
}

func (d *Decoder) write(line []byte) error {
	line = bytes.TrimRight(line, "\r\n")
	switch d.state {
	case seekingHeader:
		if rest, ok := bytes.CutPrefix(line, []byte("=ybegin ")); ok {
			return d.begin(string(rest))
		}
		return nil
	case awaitingPart:
		rest, ok := bytes.CutPrefix(line, []byte("=ypart "))
		if !ok {
			return fmt.Errorf("%w: multipart =ybegin not followed by =ypart", ErrMalformed)
		}
		return d.ypart(string(rest))
	case inData:
		if !bytes.HasPrefix(line, []byte("=y")) {
			return d.data(line)
		}
		if rest, ok := bytes.CutPrefix(line, []byte("=yend ")); ok {
			return d.end(string(rest))
		}
		return fmt.Errorf("%w: unexpected keyword line in data", ErrMalformed)
	default:
		return nil // text after the trailer, such as a signature
	}
}

func (d *Decoder) begin(line string) error {
	name, rest, err := cutName(line)
	if err != nil {
		return err
	}
	f, err := parseFields(rest, "size", "line", "part", "total")
	if err != nil {
		return err
	}
	size, err := f.number("size")
	if err != nil {
		return err
	}
	if size > MaxFileSize {
		return fmt.Errorf("%w: size=%d", ErrTooLarge, size)
	}
	if _, _, err := f.optNumber("line"); err != nil {
		return err
	}
	part, multipart, err := f.optNumber("part")
	if err != nil {
		return err
	}
	total, hasTotal, err := f.optNumber("total")
	if err != nil {
		return err
	}
	if (multipart && (part < 1 || part > math.MaxInt32)) ||
		(hasTotal && (total < 1 || total > math.MaxInt32 || part > total)) {
		return fmt.Errorf("%w: part=%d total=%d", ErrMalformed, part, total)
	}

	d.part = Part{Name: name, Size: size, PartNumber: 1, TotalParts: 1, Begin: 1, End: size}
	if multipart {
		d.multipart = true
		d.part.PartNumber, d.part.TotalParts = int(part), int(total)
		d.state = awaitingPart
		return nil
	}
	d.state = inData
	return d.allocate()
}

// allocate checks the declared part length and reserves room for it.
func (d *Decoder) allocate() error {
	n := d.part.End - d.part.Begin + 1
	if n > MaxPartSize {
		return fmt.Errorf("%w: part of %d bytes", ErrTooLarge, n)
	}
	d.buf = make([]byte, n)
	return nil
}

func (d *Decoder) ypart(line string) error {
	f, err := parseFields(line, "begin", "end")
	if err != nil {
		return err
	}
	begin, err := f.number("begin")
	if err != nil {
		return err
	}
	end, err := f.number("end")
	if err != nil {
		return err
	}
	if begin < 1 || end < begin || end > d.part.Size {
		return fmt.Errorf("%w: begin=%d end=%d size=%d", ErrRange, begin, end, d.part.Size)
	}
	d.part.Begin, d.part.End = begin, end
	d.state = inData
	return d.allocate()
}

func (d *Decoder) end(line string) error {
	d.state = done
	f, err := parseFields(line, "size", "part", "pcrc32", "crc32")
	if err != nil {
		return err
	}
	size, err := f.number("size")
	if err != nil {
		return err
	}
	if size != int64(d.n) {
		return fmt.Errorf("%w: decoded %d bytes, =yend says %d", ErrLength, d.n, size)
	}
	part, ok, err := f.optNumber("part")
	if err != nil {
		return err
	}
	if ok && part != int64(d.part.PartNumber) {
		return fmt.Errorf("%w: =yend part=%d, =ybegin part=%d", ErrMalformed, part, d.part.PartNumber)
	}
	pcrc, hasPCRC, err := f.optCRC("pcrc32")
	if err != nil {
		return err
	}
	crc, hasCRC, err := f.optCRC("crc32")
	if err != nil {
		return err
	}
	switch {
	case hasPCRC:
		d.part.ExpectedCRC32, d.part.HasExpectedCRC = pcrc, true
	case hasCRC && !d.multipart:
		// For a single-part file the whole-file CRC is the part's CRC.
		d.part.ExpectedCRC32, d.part.HasExpectedCRC = crc, true
	}
	return nil
}

func (d *Decoder) data(line []byte) error {
	n, err := decodeLine(d.buf[d.n:], line)
	d.n += n
	return err
}

// decodeLine decodes src into dst and returns the number of bytes written.
// It fails rather than write past the end of dst.
//
// The hot path copies the runs between escape characters, found with the
// vectorized bytes.IndexByte, in a loop without bounds checks.
func decodeLine(dst, src []byte) (int, error) {
	if bytes.IndexByte(src, '\r') >= 0 || bytes.IndexByte(src, '\n') >= 0 {
		src = withoutCRLF(src)
	}
	n := 0
	for len(src) > 0 {
		run := src
		if i := bytes.IndexByte(src, '='); i >= 0 {
			run = src[:i]
		}
		if len(run) > len(dst)-n {
			return n, errOverflow
		}
		subtract42(dst[n:n+len(run)], run)
		n += len(run)
		src = src[len(run):]
		if len(src) == 0 {
			break
		}
		// src[0] is an escape character.
		if len(src) == 1 {
			return n, fmt.Errorf("%w: line ends in escape character", ErrMalformed)
		}
		if n == len(dst) {
			return n, errOverflow
		}
		dst[n] = src[1] - 64 - 42
		n++
		src = src[2:]
	}
	return n, nil
}

// subtract42 sets out[i] = in[i] - 42 (mod 256); len(out) == len(in). It
// works on eight bytes at a time, adding 0xD6 (-42) to each byte lane of a
// uint64 without letting carries cross lanes.
func subtract42(out, in []byte) {
	const (
		low  = 0x7f7f7f7f7f7f7f7f
		high = 0x8080808080808080
		k    = 0xd6d6d6d6d6d6d6d6
	)
	i := 0
	for ; i+8 <= len(in) && i+8 <= len(out); i += 8 {
		x := binary.LittleEndian.Uint64(in[i:])
		binary.LittleEndian.PutUint64(out[i:], ((x&low)+(k&low))^((x^k)&high))
	}
	for ; i < len(in) && i < len(out); i++ {
		out[i] = in[i] - 42
	}
}

var errOverflow = fmt.Errorf("%w: data exceeds declared part length", ErrLength)

// withoutCRLF returns a copy of b with all CR and LF bytes removed.
func withoutCRLF(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c != '\r' && c != '\n' {
			out = append(out, c)
		}
	}
	return out
}

// Finish validates the part and returns it.
func (d *Decoder) Finish() (Part, error) {
	if d.err != nil {
		return Part{}, d.err
	}
	if d.state == seekingHeader {
		return Part{}, ErrNoHeader
	}
	if d.state != done {
		return Part{}, fmt.Errorf("%w: no =yend trailer", ErrMalformed)
	}
	if d.n != len(d.buf) {
		return Part{}, fmt.Errorf("%w: decoded %d bytes, range holds %d", ErrLength, d.n, len(d.buf))
	}
	d.part.Data = d.buf
	d.part.CRC32 = crc32.ChecksumIEEE(d.part.Data)
	return d.part, nil
}

// cutName splits a =ybegin argument list into the name, which is the rest
// of the line after "name=", and the fields before it.
func cutName(s string) (name, rest string, err error) {
	if v, ok := strings.CutPrefix(s, "name="); ok {
		return v, "", nil
	}
	if before, after, ok := strings.Cut(s, " name="); ok {
		return after, before, nil
	}
	return "", "", fmt.Errorf("%w: =ybegin without name", ErrMalformed)
}

// fields holds the known key=value pairs of a keyword line.
type fields map[string]string

// parseFields collects the known keys of a keyword line. Real posters add
// odd extras, so tokens that are not a known key=value pair are ignored. A
// repeated known key is malformed. A known key with an empty value is kept
// so that its typed accessor rejects it.
func parseFields(s string, known ...string) (fields, error) {
	f := fields{}
	for tok := range strings.FieldsSeq(s) {
		k, v, _ := strings.Cut(tok, "=")
		if !slices.Contains(known, k) {
			continue
		}
		if _, dup := f[k]; dup {
			return nil, fmt.Errorf("%w: duplicate field %q", ErrMalformed, k)
		}
		f[k] = v
	}
	return f, nil
}

// number returns the required decimal field key.
func (f fields) number(key string) (int64, error) {
	n, ok, err := f.optNumber(key)
	if err == nil && !ok {
		err = fmt.Errorf("%w: missing field %q", ErrMalformed, key)
	}
	return n, err
}

// optNumber returns the decimal field key, if present. Only plain digits
// are accepted: no sign, prefix or separators.
func (f fields) optNumber(key string) (n int64, ok bool, err error) {
	v, ok := f[key]
	if !ok {
		return 0, false, nil
	}
	if strings.Trim(v, "0123456789") != "" {
		return 0, false, fmt.Errorf("%w: %s=%q is not a number", ErrMalformed, key, v)
	}
	n, err = strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s=%q is out of range", ErrMalformed, key, v)
	}
	return n, true, nil
}

// optCRC returns the hexadecimal CRC32 field key, if present.
func (f fields) optCRC(key string) (crc uint32, ok bool, err error) {
	v, ok := f[key]
	if !ok {
		return 0, false, nil
	}
	c, err := strconv.ParseUint(v, 16, 32)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s=%q is not a CRC32", ErrMalformed, key, v)
	}
	return uint32(c), true, nil
}
