package nntptest_test

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
	"github.com/jroosing/bitnews/internal/usenet/yenc"
)

func randomBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// decode runs lines through the real yEnc decoder.
func decode(t *testing.T, lines []string) (yenc.Part, error) {
	t.Helper()
	var d yenc.Decoder
	for _, l := range lines {
		if err := d.Write([]byte(l)); err != nil {
			return yenc.Part{}, err
		}
	}
	return d.Finish()
}

func TestEncodeArticlesRoundTripsThroughTheDecoder(t *testing.T) {
	cases := map[string]struct {
		file              []byte
		partSize, lineLen int
	}{
		"empty":           {nil, 100, 128},
		"single part":     {randomBytes(1, 500), 1000, 128},
		"multipart":       {randomBytes(2, 50_001), 10_000, 128},
		"all byte values": {bytes.Repeat(allBytes(), 8), 700, 7},
		"one char a line": {allBytes(), 4096, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			articles := nntptest.EncodeArticles("f.bin", tc.file, tc.partSize, tc.lineLen)
			got := make([]byte, len(tc.file))
			for i, lines := range articles {
				p, err := decode(t, lines)
				require.NoError(t, err)
				assert.True(t, p.CRCMatch())
				assert.Equal(t, i+1, p.PartNumber)
				copy(got[p.Begin-1:], p.Data)
			}
			assert.True(t, bytes.Equal(tc.file, got), "reassembled file differs")
		})
	}
}

func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestCorruptServesBodiesThatFailTheCRCCheck(t *testing.T) {
	s := nntptest.NewServer("bad")
	file := randomBytes(3, 5000)
	good := nntptest.EncodeArticles("f.bin", file, 5000, 128)[0]
	s.AddArticle("a@x", good)
	s.AddArticle("b@x", good)
	s.Corrupt("a@x")
	c := dial(t, s)

	bad, err := fetch(t, c, "a@x")
	require.NoError(t, err)
	clean, err := fetch(t, c, "b@x")
	require.NoError(t, err)

	p, err := decode(t, bad)
	require.NoError(t, err, "corruption keeps the structure and the length valid")
	assert.False(t, p.CRCMatch())
	assert.NotEqual(t, file, p.Data)
	p, err = decode(t, clean)
	require.NoError(t, err)
	assert.True(t, p.CRCMatch())
	assert.Equal(t, good, clean, "the stored article is not modified")
}

func TestCorruptEmptyPayloadStillFailsCRC(t *testing.T) {
	s := nntptest.NewServer("bad")
	s.AddArticle("a@x", nntptest.EncodeArticles("f.bin", nil, 100, 128)[0])
	s.Corrupt("a@x")

	lines, err := fetch(t, dial(t, s), "a@x")
	require.NoError(t, err)

	p, err := decode(t, lines)
	require.NoError(t, err)
	assert.False(t, p.CRCMatch())
}
