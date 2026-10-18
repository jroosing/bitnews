package yenc_test

import (
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/usenet/yenc"
)

// BenchmarkDecode decodes a typical ~700 KB Article part. Throughput is
// reported in decoded bytes.
func BenchmarkDecode(b *testing.B) {
	const partSize = 716_800
	file := randomBytes(42, 2*partSize)
	article := encodeArticles("bench.bin", file, partSize, 128)[0]
	lines := make([][]byte, len(article))
	for i, l := range article {
		lines[i] = []byte(l)
	}
	b.SetBytes(partSize)
	b.ReportAllocs()
	for b.Loop() {
		var d yenc.Decoder
		for _, l := range lines {
			if err := d.Write(l); err != nil {
				b.Fatal(err)
			}
		}
		p, err := d.Finish()
		if err != nil || !p.CRCMatch() {
			b.Fatalf("Finish: %v, CRCMatch %v", err, p.CRCMatch())
		}
	}
}
