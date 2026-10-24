package nntptest_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/usenet/nntp"
	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
	"github.com/jroosing/bitnews/internal/usenet/nzb"
	"github.com/jroosing/bitnews/internal/usenet/yenc"
)

// download fetches every Article of the parsed NZB through c, decodes it and
// reassembles the files.
func download(t *testing.T, c nntp.ArticleConn, n *nzb.NZB) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, f := range n.Files {
		var file []byte
		for _, seg := range f.Segments {
			var d yenc.Decoder
			err := c.Body(context.Background(), seg.MessageID, func(l []byte) error { return d.Write(l) })
			require.NoError(t, err, "segment %d of %s", seg.Number, f.Name)
			p, err := d.Finish()
			require.NoError(t, err)
			require.True(t, p.CRCMatch(), "segment %d of %s", seg.Number, f.Name)
			if file == nil {
				file = make([]byte, p.Size)
			}
			copy(file[p.Begin-1:], p.Data)
		}
		out[f.Name] = file
	}
	return out
}

func TestBuildFixtureRoundTripsThroughNZBParserAndYencDecoder(t *testing.T) {
	files := map[string][]byte{
		"movie.mkv":     randomBytes(10, 95_000),
		"movie.nfo":     []byte("small file\n"),
		"empty.txt":     nil,
		"exact.bin":     randomBytes(11, 30_000),
		"movie.par2":    randomBytes(12, 4_321),
		"all bytes.dat": bytes.Repeat(allBytes(), 100),
	}
	fx := nntptest.BuildFixture(t, files, 30_000)
	s := nntptest.NewServer("main")
	fx.Install(s)

	parsed, err := nzb.Parse(bytes.NewReader(fx.NZB), nzb.DefaultLimits())
	require.NoError(t, err)
	require.Len(t, parsed.Files, len(files))
	assert.Zero(t, parsed.SkippedSegments)

	got := download(t, dial(t, s), parsed)

	assert.Len(t, got, len(files))
	for name, want := range files {
		assert.True(t, bytes.Equal(want, got[name]), "%s differs", name)
	}
	byName := map[string]*nzb.File{}
	for i := range parsed.Files {
		byName[parsed.Files[i].Name] = &parsed.Files[i]
	}
	assert.Len(t, byName["movie.mkv"].Segments, 4)
	assert.Len(t, byName["exact.bin"].Segments, 1)
	assert.Len(t, byName["empty.txt"].Segments, 1)
	assert.Equal(t, nzb.KindPar2Index, byName["movie.par2"].Kind)
}

func TestFixtureListsMessageIDsMatchingTheNZB(t *testing.T) {
	fx := nntptest.BuildFixture(t, map[string][]byte{
		"a.bin": randomBytes(1, 2500),
		"b.bin": randomBytes(2, 900),
	}, 1000)
	parsed, err := nzb.Parse(bytes.NewReader(fx.NZB), nzb.DefaultLimits())
	require.NoError(t, err)

	var fromNZB []string
	for _, f := range parsed.Files {
		for _, seg := range f.Segments {
			fromNZB = append(fromNZB, seg.MessageID)
		}
	}

	assert.ElementsMatch(t, fromNZB, fx.MessageIDs())
	assert.Len(t, fx.Articles, 4)
	require.Len(t, fx.Files, 2)
	assert.Equal(t, "a.bin", fx.Files[0].Name, "files are sorted by name")
	assert.Len(t, fx.Files[0].MessageIDs, 3)
}

func TestFixturesAreDeterministicAndHaveDisjointMessageIDs(t *testing.T) {
	a1 := nntptest.BuildFixture(t, map[string][]byte{"a.bin": randomBytes(1, 3000)}, 1000)
	a2 := nntptest.BuildFixture(t, map[string][]byte{"a.bin": randomBytes(1, 3000)}, 1000)
	b := nntptest.BuildFixture(t, map[string][]byte{"a.bin": randomBytes(2, 3000)}, 1000)

	assert.Equal(t, a1.NZB, a2.NZB)
	assert.Equal(t, a1.MessageIDs(), a2.MessageIDs())
	for _, id := range b.MessageIDs() {
		assert.NotContains(t, a1.MessageIDs(), id)
	}
}

func TestWithDateSetsTheFilePostingTime(t *testing.T) {
	posted := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	fx := nntptest.BuildFixture(t, map[string][]byte{"a.bin": {1, 2, 3}}, 1000, nntptest.WithDate(posted))

	parsed, err := nzb.Parse(bytes.NewReader(fx.NZB), nzb.DefaultLimits())
	require.NoError(t, err)
	assert.True(t, parsed.Files[0].Date.Equal(posted), "%v", parsed.Files[0].Date)
}

func TestWithLineLengthChangesTheEncodedLineLength(t *testing.T) {
	fx := nntptest.BuildFixture(t, map[string][]byte{"a.bin": randomBytes(1, 1000)}, 5000, nntptest.WithLineLength(40))

	for _, lines := range fx.Articles {
		for _, l := range lines[1 : len(lines)-1] {
			assert.LessOrEqual(t, len(l), 41)
		}
	}
}

func TestFixtureCorruptionHelpersWorkOnRealFixtures(t *testing.T) {
	fx := nntptest.BuildFixture(t, map[string][]byte{"a.bin": randomBytes(1, 3000)}, 1000)
	s := nntptest.NewServer("bad")
	fx.Install(s)
	s.Corrupt(fx.Files[0].MessageIDs[1])
	c := dial(t, s)

	var crcOK []bool
	for _, id := range fx.Files[0].MessageIDs {
		var d yenc.Decoder
		require.NoError(t, c.Body(context.Background(), id, func(l []byte) error { return d.Write(l) }))
		p, err := d.Finish()
		require.NoError(t, err)
		crcOK = append(crcOK, p.CRCMatch())
	}

	assert.Equal(t, []bool{true, false, true}, crcOK)
}
