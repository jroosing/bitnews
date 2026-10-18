package yenc_test

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func allByteValues(repeat int) []byte {
	var b []byte
	for range repeat {
		for c := range 256 {
			b = append(b, byte(c))
		}
	}
	return b
}

func randomBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestRoundTripsFiles(t *testing.T) {
	cases := []struct {
		name              string
		file              []byte
		partSize, lineLen int
	}{
		{"empty single part", nil, 100, 128},
		{"all byte values single part", allByteValues(4), 4096, 128},
		{"all byte values one per line", allByteValues(1), 4096, 1},
		{"all byte values short lines", allByteValues(3), 4096, 7},
		{"whitespace and dots at line edges", bytes.Repeat([]byte{0xF6, 0xDF, 0x04, 0x01}, 300), 4096, 3},
		{"random multipart", randomBytes(1, 100_000), 16_384, 128},
		{"random multipart uneven last part", randomBytes(2, 50_001), 10_000, 256},
		{"critical bytes multipart", bytes.Repeat([]byte{0xD6, 0xE0, 0xE3, 0x13}, 5000), 3000, 128},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			articles := encodeArticles("file.bin", tc.file, tc.partSize, tc.lineLen)
			got := make([]byte, len(tc.file))
			// Decode out of order, as the scheduler may deliver them.
			for i, article := range slices.Backward(articles) {
				p := mustDecode(t, article...)
				assert.Equal(t, "file.bin", p.Name, "Name")
				assert.Equal(t, int64(len(tc.file)), p.Size, "Size")
				assert.Equal(t, i+1, p.PartNumber, "PartNumber")
				assert.Equal(t, len(articles), p.TotalParts, "TotalParts")
				assert.True(t, p.CRCMatch())
				copy(got[p.Begin-1:], p.Data)
			}
			assert.True(t, bytes.Equal(got, tc.file), "reassembled file differs from original")
		})
	}
}
