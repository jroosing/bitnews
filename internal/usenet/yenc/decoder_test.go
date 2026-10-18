package yenc_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/usenet/yenc"
)

// decode feeds lines to a fresh Decoder and finishes it.
func decode(lines ...string) (yenc.Part, error) {
	var d yenc.Decoder
	for _, l := range lines {
		if err := d.Write([]byte(l)); err != nil {
			return yenc.Part{}, err
		}
	}
	return d.Finish()
}

func mustDecode(t *testing.T, lines ...string) yenc.Part {
	t.Helper()
	p, err := decode(lines...)
	require.NoError(t, err)
	return p
}

func TestDecodesSinglePartArticle(t *testing.T) {
	p := mustDecode(t,
		"=ybegin line=128 size=9 name=check.txt",
		`[\]^_`+"`"+`abc`,
		"=yend size=9 crc32=cbf43926",
	)
	assert.Equal(t, []byte("123456789"), p.Data)
	assert.Equal(t, "check.txt", p.Name, "Name")
	assert.Equal(t, int64(9), p.Size, "Size")
	assert.Equal(t, int64(1), p.Begin, "Begin")
	assert.Equal(t, int64(9), p.End, "End")
	assert.Equal(t, 1, p.PartNumber, "PartNumber")
	assert.Equal(t, 1, p.TotalParts, "TotalParts")
	assert.Equal(t, uint32(0xcbf43926), p.CRC32, "CRC32")
	assert.True(t, p.CRCMatch())
}

func TestDecodesEscapedCriticalCharacters(t *testing.T) {
	// D6 E0 E3 13 map to NUL LF CR '=' and must be escaped; F6 DF 04 map to
	// space, tab and '.', which encoders escape at line edges.
	p := mustDecode(t,
		"=ybegin line=128 size=7 name=escapes.bin",
		"=@=J=M=}=`=I=n",
		"=yend size=7 crc32=de67a556",
	)
	assert.Equal(t, []byte{0xD6, 0xE0, 0xE3, 0x13, 0xF6, 0xDF, 0x04}, p.Data)
	assert.True(t, p.CRCMatch())
}

func TestRejectsLineEndingInEscapeCharacter(t *testing.T) {
	_, err := decode(
		"=ybegin line=128 size=1 name=x",
		"=",
		"=yend size=1",
	)
	assert.ErrorIs(t, err, yenc.ErrMalformed)
}

func TestIgnoresLineTerminators(t *testing.T) {
	p := mustDecode(t,
		"=ybegin line=128 size=9 name=check.txt\r\n",
		"[\\]^\r\n",
		"_`a\n",
		"b\rc\r\n",
		"=yend size=9 crc32=cbf43926\r\n",
	)
	assert.Equal(t, []byte("123456789"), p.Data)
	assert.Equal(t, "check.txt", p.Name, "Name")
	assert.True(t, p.CRCMatch())
}

func TestTakesLeadingDoubleDotLiterally(t *testing.T) {
	// Dot-unstuffing is the NNTP layer's job. A line that still starts with
	// ".." holds two '.' bytes, each decoding to 0x2E-42.
	p := mustDecode(t,
		"=ybegin line=128 size=3 name=dot.bin",
		"..[",
		"=yend size=3",
	)
	assert.Equal(t, []byte{0x04, 0x04, '1'}, p.Data)
}

func TestDecodesMultipartPost(t *testing.T) {
	// A 9-byte file "123456789" posted in two Articles.
	articles := []struct {
		lines      []string
		data       string
		begin, end int64
		number     int
	}{
		{
			lines: []string{
				"=ybegin part=1 total=2 line=128 size=9 name=my file (1).txt",
				"=ypart begin=1 end=4",
				`[\]^`,
				"=yend size=4 part=1 pcrc32=9be3e0a3",
			},
			data: "1234", begin: 1, end: 4, number: 1,
		},
		{
			lines: []string{
				"=ybegin part=2 total=2 line=128 size=9 name=my file (1).txt",
				"=ypart begin=5 end=9",
				"_`abc",
				"=yend size=5 part=2 pcrc32=131da070 crc32=cbf43926",
			},
			data: "56789", begin: 5, end: 9, number: 2,
		},
	}
	for _, a := range articles {
		p := mustDecode(t, a.lines...)
		assert.Equal(t, []byte(a.data), p.Data)
		assert.Equal(t, "my file (1).txt", p.Name, "Name")
		assert.Equal(t, int64(9), p.Size, "Size")
		assert.Equal(t, a.number, p.PartNumber, "PartNumber")
		assert.Equal(t, 2, p.TotalParts, "TotalParts")
		assert.Equal(t, a.begin, p.Begin, "Begin")
		assert.Equal(t, a.end, p.End, "End")
		assert.True(t, p.CRCMatch())
	}
}

func TestReturnsDataWithCRCMismatch(t *testing.T) {
	p := mustDecode(t,
		"=ybegin line=128 size=9 name=check.txt",
		`[\]^_`+"`"+`abd`, // last byte corrupted
		"=yend size=9 crc32=cbf43926",
	)
	assert.Equal(t, []byte("12345678:"), p.Data)
	assert.True(t, p.HasExpectedCRC)
	assert.Equal(t, uint32(0xcbf43926), p.ExpectedCRC32, "ExpectedCRC32")
	assert.False(t, p.CRCMatch())
}

func TestPartWithoutAnnouncedCRCDoesNotMatch(t *testing.T) {
	p := mustDecode(t,
		"=ybegin line=128 size=9 name=check.txt",
		`[\]^_`+"`"+`abc`,
		"=yend size=9",
	)
	assert.False(t, p.HasExpectedCRC)
	assert.Equal(t, uint32(0xcbf43926), p.CRC32, "CRC32")
	assert.False(t, p.CRCMatch())
}

func TestMultipartIgnoresWholeFileCRC(t *testing.T) {
	// crc32= in a multipart trailer covers the whole file, not this part.
	p := mustDecode(t,
		"=ybegin part=2 total=2 line=128 size=9 name=check.txt",
		"=ypart begin=5 end=9",
		"_`abc",
		"=yend size=5 part=2 crc32=cbf43926",
	)
	assert.False(t, p.HasExpectedCRC)
}

func TestRejectsArticleWithoutHeader(t *testing.T) {
	cases := map[string][]string{
		"empty":     nil,
		"data only": {`[\]^_` + "`" + `abc`},
		"trailer":   {`[\]^_` + "`" + `abc`, "=yend size=9 crc32=cbf43926"},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decode(lines...)
			assert.ErrorIs(t, err, yenc.ErrNoHeader)
		})
	}
}

func TestRejectsDecodedLengthDifferentFromRange(t *testing.T) {
	cases := map[string][]string{
		"short multipart": {
			"=ybegin part=1 total=2 line=128 size=9 name=x",
			"=ypart begin=1 end=4",
			`[\]`,
			"=yend size=3 part=1",
		},
		"long multipart": {
			"=ybegin part=1 total=2 line=128 size=9 name=x",
			"=ypart begin=1 end=4",
			`[\]^_`,
			"=yend size=5 part=1",
		},
		"short single part": {
			"=ybegin line=128 size=9 name=x",
			`[\]^_`,
			"=yend size=5",
		},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decode(lines...)
			assert.ErrorIs(t, err, yenc.ErrLength)
		})
	}
}

func TestRejectsTrailerSizeDifferentFromDecodedLength(t *testing.T) {
	_, err := decode(
		"=ybegin part=1 total=2 line=128 size=9 name=x",
		"=ypart begin=1 end=4",
		`[\]^`,
		"=yend size=5 part=1",
	)
	assert.ErrorIs(t, err, yenc.ErrLength)
}

func TestRejectsInvalidPartRange(t *testing.T) {
	cases := map[string]string{
		"end before begin": "=ypart begin=5 end=4",
		"begin zero":       "=ypart begin=0 end=4",
		"end past size":    "=ypart begin=5 end=10",
	}
	for name, ypart := range cases {
		t.Run(name, func(t *testing.T) {
			var d yenc.Decoder
			err := d.Write([]byte("=ybegin part=2 total=2 line=128 size=9 name=x"))
			if err == nil {
				err = d.Write([]byte(ypart))
			}
			assert.ErrorIs(t, err, yenc.ErrRange)
		})
	}
}

func TestRejectsOversizedFileOrPart(t *testing.T) {
	cases := map[string][]string{
		"file over limit": {
			fmt.Sprintf("=ybegin part=1 line=128 size=%d name=x", yenc.MaxFileSize+1),
		},
		"single part over limit": {
			fmt.Sprintf("=ybegin line=128 size=%d name=x", yenc.MaxPartSize+1),
		},
		"multipart range over limit": {
			fmt.Sprintf("=ybegin part=1 line=128 size=%d name=x", yenc.MaxFileSize),
			fmt.Sprintf("=ypart begin=1 end=%d", yenc.MaxPartSize+1),
		},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decode(lines...)
			assert.ErrorIs(t, err, yenc.ErrTooLarge)
		})
	}
}

func TestAcceptsPartsAtTheLimits(t *testing.T) {
	var d yenc.Decoder
	for _, l := range []string{
		fmt.Sprintf("=ybegin part=2 line=128 size=%d name=x", yenc.MaxFileSize),
		fmt.Sprintf("=ypart begin=%d end=%d", yenc.MaxFileSize-yenc.MaxPartSize+1, yenc.MaxFileSize),
	} {
		require.NoError(t, d.Write([]byte(l)), l)
	}
}

func TestRejectsDataBeyondDeclaredRangeAsItArrives(t *testing.T) {
	var d yenc.Decoder
	for _, l := range []string{
		"=ybegin part=1 total=2 line=128 size=9 name=x",
		"=ypart begin=1 end=4",
		`[\]`,
	} {
		require.NoError(t, d.Write([]byte(l)), l)
	}
	assert.ErrorIs(t, d.Write([]byte("^_")), yenc.ErrLength)
}

func TestErrorsAreSticky(t *testing.T) {
	var d yenc.Decoder
	for _, l := range []string{
		"=ybegin part=1 total=2 line=128 size=9 name=x",
		"=ypart begin=1 end=4",
		`[\]^_`, // one byte too many: rejected
	} {
		_ = d.Write([]byte(l))
	}
	assert.ErrorIs(t, d.Write([]byte("=yend size=4 part=1")), yenc.ErrLength)
	_, err := d.Finish()
	assert.ErrorIs(t, err, yenc.ErrLength)
}

func TestRejectsArticleWithoutTrailer(t *testing.T) {
	_, err := decode(
		"=ybegin line=128 size=9 name=check.txt",
		`[\]^_`+"`"+`abc`,
	)
	assert.ErrorIs(t, err, yenc.ErrMalformed)
}

func TestIgnoresTextAroundTheEncodedPart(t *testing.T) {
	p := mustDecode(t,
		"",
		"Posted with SomeClient 1.0",
		"=ybegin line=128 size=9 name=check.txt",
		`[\]^_`+"`"+`abc`,
		"=yend size=9 crc32=cbf43926",
		"",
		"-- ",
		"signature",
	)
	assert.Equal(t, []byte("123456789"), p.Data)
	assert.True(t, p.CRCMatch())
}

func TestRejectsMisplacedKeywordLines(t *testing.T) {
	const multi = "=ybegin part=1 total=2 line=128 size=9 name=x"
	const single = "=ybegin line=128 size=9 name=x"
	cases := map[string][]string{
		"multipart without ypart": {multi, `[\]^`, "=yend size=4 part=1"},
		"ypart after data":        {multi, `[\]^`, "=ypart begin=1 end=4", "=yend size=4 part=1"},
		"ypart twice":             {multi, "=ypart begin=1 end=4", "=ypart begin=1 end=4", `[\]^`, "=yend size=4 part=1"},
		"ypart in single part":    {single, "=ypart begin=1 end=9", `[\]^_` + "`abc", "=yend size=9"},
		"nested ybegin":           {single, single, `[\]^_` + "`abc", "=yend size=9"},
		"unknown keyword":         {single, "=yfoo bar=1", `[\]^_` + "`abc", "=yend size=9"},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decode(lines...)
			assert.ErrorIs(t, err, yenc.ErrMalformed)
		})
	}
}

func TestRejectsMalformedFields(t *testing.T) {
	const data = `[\]^`
	const ypart = "=ypart begin=1 end=4"
	const yend = "=yend size=4 part=1"
	article := func(ybegin, ypart, yend string) []string {
		return []string{ybegin, ypart, data, yend}
	}
	const ybegin = "=ybegin part=1 total=2 line=128 size=9 name=x"
	cases := map[string][]string{
		"missing size":       article("=ybegin part=1 total=2 line=128 name=x", ypart, yend),
		"missing name":       article("=ybegin part=1 total=2 line=128 size=9", ypart, yend),
		"negative size":      article("=ybegin part=1 total=2 line=128 size=-9 name=x", ypart, yend),
		"signed size":        article("=ybegin part=1 total=2 line=128 size=+9 name=x", ypart, yend),
		"hex size":           article("=ybegin part=1 total=2 line=128 size=0x9 name=x", ypart, yend),
		"overflowing size":   article("=ybegin part=1 total=2 line=128 size=99999999999999999999 name=x", ypart, yend),
		"part zero":          article("=ybegin part=0 total=2 line=128 size=9 name=x", ypart, "=yend size=4 part=0"),
		"part not a number":  article("=ybegin part=one total=2 line=128 size=9 name=x", ypart, yend),
		"part beyond total":  article("=ybegin part=3 total=2 line=128 size=9 name=x", ypart, "=yend size=4 part=3"),
		"total zero":         article("=ybegin part=1 total=0 line=128 size=9 name=x", ypart, yend),
		"part beyond int32":  article("=ybegin part=4294967297 line=128 size=9 name=x", ypart, "=yend size=4 part=4294967297"),
		"total beyond int32": article("=ybegin part=1 total=4294967297 line=128 size=9 name=x", ypart, yend),
		"line not a number":  article("=ybegin part=1 total=2 line=x size=9 name=x", ypart, yend),
		"duplicate size":     article("=ybegin part=1 total=2 line=128 size=9 size=4 name=x", ypart, yend),
		"empty value":        article("=ybegin part=1 total=2 line=128 size= name=x", ypart, yend),
		"ypart missing end":  article(ybegin, "=ypart begin=1", yend),
		"ypart bad begin":    article(ybegin, "=ypart begin=a end=4", yend),
		"yend missing size":  article(ybegin, ypart, "=yend part=1"),
		"yend bad size":      article(ybegin, ypart, "=yend size=4x part=1"),
		"yend part mismatch": article(ybegin, ypart, "=yend size=4 part=2"),
		"pcrc32 not hex":     article(ybegin, ypart, "=yend size=4 part=1 pcrc32=xyz"),
		"pcrc32 too long":    article(ybegin, ypart, "=yend size=4 part=1 pcrc32=19be3e0a3"),
		"pcrc32 signed":      article(ybegin, ypart, "=yend size=4 part=1 pcrc32=+be3e0a3"),
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decode(lines...)
			assert.ErrorIs(t, err, yenc.ErrMalformed)
		})
	}
}

func TestIgnoresUnknownHeaderTokens(t *testing.T) {
	p := mustDecode(t,
		"=ybegin part=1 total=2 line=128 size=9 flavor=x junk flavor=y empty= name=x",
		"=ypart begin=1 end=4 extra=1 extra=2 odd",
		`[\]^`,
		"=yend size=4 part=1 pcrc32=9be3e0a3 extra=1 extra=2 odd",
	)
	assert.Equal(t, []byte("1234"), p.Data)
	assert.Equal(t, "x", p.Name)
	assert.True(t, p.CRCMatch())
}

func TestRejectsDuplicateKnownKeysInPartAndTrailer(t *testing.T) {
	const ybegin = "=ybegin part=1 total=2 line=128 size=9 name=x"
	cases := map[string][]string{
		"ypart end":   {ybegin, "=ypart begin=1 end=4 end=4", `[\]^`, "=yend size=4 part=1"},
		"yend pcrc32": {ybegin, "=ypart begin=1 end=4", `[\]^`, "=yend size=4 pcrc32=9be3e0a3 pcrc32=9be3e0a3"},
		"yend size":   {ybegin, "=ypart begin=1 end=4", `[\]^`, "=yend size=4 size=4"},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decode(lines...)
			assert.ErrorIs(t, err, yenc.ErrMalformed)
		})
	}
}

func TestNameIsRestOfHeaderLine(t *testing.T) {
	cases := map[string]string{
		"=ybegin part=1 total=2 line=128 size=9 name=a b  size=1.txt ": "a b  size=1.txt ",
		"=ybegin line=128 size=9 part=1 name=../../etc/passwd":         "../../etc/passwd",
		"=ybegin part=1 size=9 name=":                                  "",
	}
	for ybegin, want := range cases {
		t.Run(ybegin, func(t *testing.T) {
			p := mustDecode(t, ybegin, "=ypart begin=1 end=4", `[\]^`, "=yend size=4 part=1")
			assert.Equal(t, want, p.Name, "Name")
		})
	}
}

func TestParsesUppercaseCRC(t *testing.T) {
	p := mustDecode(t,
		"=ybegin part=1 total=2 line=128 size=9 name=x",
		"=ypart begin=1 end=4",
		`[\]^`,
		"=yend size=4 part=1 pcrc32=9BE3E0A3",
	)
	assert.True(t, p.CRCMatch())
}
