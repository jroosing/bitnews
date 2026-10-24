package yenc_test

import (
	"bytes"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/jroosing/bitnews/internal/usenet/yenc"
)

// FuzzDecode feeds arbitrary bodies, split into lines, to the Decoder. It
// must never panic, never hold more than MaxPartSize decoded bytes, and any
// part it accepts must be consistent with its declared range.
func FuzzDecode(f *testing.F) {
	for _, a := range encodeArticles("seed.bin", allByteValues(2), 200, 32) {
		f.Add([]byte(strings.Join(a, "\r\n")))
	}
	f.Add([]byte(strings.Join(encodeArticles("one.bin", []byte("123456789"), 100, 128)[0], "\n")))
	f.Add([]byte("=ybegin part=1 total=2 line=128 size=9 name=x\n=ypart begin=1 end=4\n[\\]^\n=yend size=4 part=1 pcrc32=9be3e0a3\n"))
	f.Add([]byte("=ybegin line=128 size=1 name=x\n=\n=yend size=1\n"))

	f.Fuzz(func(t *testing.T, body []byte) {
		var d yenc.Decoder
		var err error
		for line := range bytes.Lines(body) {
			if err = d.Write(line); err != nil {
				break
			}
		}
		p, ferr := d.Finish()
		if err != nil && ferr == nil {
			t.Fatalf("Write failed with %v but Finish succeeded", err)
		}
		if ferr != nil {
			return
		}
		if int64(len(p.Data)) != p.End-p.Begin+1 {
			t.Fatalf("len(Data) = %d, range %d-%d", len(p.Data), p.Begin, p.End)
		}
		if cap(p.Data) > yenc.MaxPartSize {
			t.Fatalf("cap(Data) = %d exceeds MaxPartSize", cap(p.Data))
		}
		if p.Begin < 1 || p.End > p.Size || p.Size > yenc.MaxFileSize {
			t.Fatalf("Begin, End, Size = %d, %d, %d out of bounds", p.Begin, p.End, p.Size)
		}
		if p.CRC32 != crc32.ChecksumIEEE(p.Data) {
			t.Fatalf("CRC32 = %08x, not the CRC of Data", p.CRC32)
		}
	})
}
