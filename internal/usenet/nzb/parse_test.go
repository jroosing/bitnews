package nzb_test

import (
	"encoding/xml"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nzb"
)

func parseString(t *testing.T, s string) *nzb.NZB {
	t.Helper()
	n, err := nzb.Parse(strings.NewReader(s), nzb.DefaultLimits())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return n
}

func TestParse_SingleFileSingleSegment(t *testing.T) {
	n := parseString(t, `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="bob@example.com" date="1700000000" subject="test.bin yEnc (1/1)">
  <groups><group>alt.binaries.test</group></groups>
  <segments><segment bytes="739" number="1">abc@example.com</segment></segments>
 </file>
</nzb>`)
	if len(n.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(n.Files))
	}
	f := n.Files[0]
	if f.Subject != "test.bin yEnc (1/1)" || f.Poster != "bob@example.com" {
		t.Errorf("subject/poster = %q/%q", f.Subject, f.Poster)
	}
	want := nzb.Segment{Number: 1, Bytes: 739, MessageID: "abc@example.com"}
	if len(f.Segments) != 1 || f.Segments[0] != want {
		t.Errorf("segments = %+v, want [%+v]", f.Segments, want)
	}
}

func TestParse_FileDateAndGroups(t *testing.T) {
	n := parseString(t, `<nzb>
 <file poster="p" date="1700000000" subject="s">
  <groups>
   <group> alt.binaries.test </group>
   <group>alt.binaries.misc</group>
  </groups>
  <segments><segment bytes="1" number="1">a@b</segment></segments>
 </file>
</nzb>`)
	f := n.Files[0]
	if want := time.Unix(1700000000, 0).UTC(); !f.Date.Equal(want) {
		t.Errorf("date = %v, want %v", f.Date, want)
	}
	if want := []string{"alt.binaries.test", "alt.binaries.misc"}; !slices.Equal(f.Groups, want) {
		t.Errorf("groups = %q, want %q", f.Groups, want)
	}
}

func TestParse_MetaFirstOfEachTypeWins(t *testing.T) {
	n := parseString(t, `<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <head>
  <meta type="name">Some.Release.2024</meta>
  <meta type="password"> s3cret </meta>
  <meta type="category">TV &gt; HD</meta>
  <meta type="name">ignored</meta>
  <meta type="tag">unrelated</meta>
 </head>
 <file poster="p" date="1" subject="s"><segments><segment number="1">a@b</segment></segments></file>
</nzb>`)
	want := nzb.Meta{Name: "Some.Release.2024", Password: "s3cret", Category: "TV > HD"}
	if n.Meta != want {
		t.Errorf("meta = %+v, want %+v", n.Meta, want)
	}
}

// oneFile wraps segment XML in a minimal single-file NZB.
func oneFile(segments string) string {
	return `<nzb><file poster="p" date="1" subject="s"><segments>` + segments + `</segments></file></nzb>`
}

func segmentNumbers(f nzb.File) []int {
	var ns []int
	for _, s := range f.Segments {
		ns = append(ns, s.Number)
	}
	return ns
}

func TestParse_SegmentsSortedByNumber(t *testing.T) {
	n := parseString(t, oneFile(`
		<segment number="3">c@x</segment>
		<segment number="1">a@x</segment>
		<segment number="2">b@x</segment>`))
	if got, want := segmentNumbers(n.Files[0]), []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("numbers = %v, want %v", got, want)
	}
	if got := n.Files[0].Segments[0].MessageID; got != "a@x" {
		t.Errorf("first message id = %q, want a@x", got)
	}
}

func TestParse_DuplicateSegmentNumbersKeepFirst(t *testing.T) {
	n := parseString(t, oneFile(`
		<segment number="2">first-two@x</segment>
		<segment number="1">a@x</segment>
		<segment number="2">second-two@x</segment>`))
	segs := n.Files[0].Segments
	if got, want := segmentNumbers(n.Files[0]), []int{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("numbers = %v, want %v", got, want)
	}
	if segs[1].MessageID != "first-two@x" {
		t.Errorf("kept %q, want first-two@x", segs[1].MessageID)
	}
}

func TestParse_MessageIDAngleBracketsTrimmed(t *testing.T) {
	n := parseString(t, oneFile(`<segment number="1"> &lt;part1of1.abc@news.example&gt; </segment>`))
	if got := n.Files[0].Segments[0].MessageID; got != "part1of1.abc@news.example" {
		t.Errorf("message id = %q", got)
	}
}

func TestParse_InvalidMessageIDSkipsSegment(t *testing.T) {
	cases := map[string]string{
		"empty":         ``,
		"no at sign":    `noatsign.example`,
		"inner space":   `a b@x`,
		"tab":           "a\tb@x",
		"non-ascii":     `é@x`,
		"DEL":           "a\x7f@x",
		"over 250":      strings.Repeat("a", 249) + "@x",
		"only brackets": `&lt;&gt;`,
		"inner angle >": `a&gt;b@x`,
		"inner angle <": `a&lt;b@x`,
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			n := parseString(t, oneFile(`<segment number="1">ok@x</segment><segment number="2">`+id+`</segment>`))
			if got, want := segmentNumbers(n.Files[0]), []int{1}; !slices.Equal(got, want) {
				t.Errorf("numbers = %v, want %v", got, want)
			}
			if n.SkippedSegments != 1 {
				t.Errorf("skipped = %d, want 1", n.SkippedSegments)
			}
		})
	}
}

func TestParse_MessageIDOf250CharsAccepted(t *testing.T) {
	id := strings.Repeat("a", 248) + "@x"
	n := parseString(t, oneFile(`<segment number="1">`+id+`</segment>`))
	if got := n.Files[0].Segments[0].MessageID; got != id {
		t.Errorf("message id = %q, want 250-char id", got)
	}
}

func TestParse_InvalidSegmentNumberSkipsSegment(t *testing.T) {
	cases := map[string]string{
		"missing":  `<segment>b@x</segment>`,
		"empty":    `<segment number="">b@x</segment>`,
		"zero":     `<segment number="0">b@x</segment>`,
		"negative": `<segment number="-2">b@x</segment>`,
		"text":     `<segment number="two">b@x</segment>`,
		"overflow": `<segment number="99999999999999999999999">b@x</segment>`,
	}
	for name, seg := range cases {
		t.Run(name, func(t *testing.T) {
			n := parseString(t, oneFile(`<segment number="1">ok@x</segment>`+seg))
			if got, want := segmentNumbers(n.Files[0]), []int{1}; !slices.Equal(got, want) {
				t.Errorf("numbers = %v, want %v", got, want)
			}
			if n.SkippedSegments != 1 {
				t.Errorf("skipped = %d, want 1", n.SkippedSegments)
			}
		})
	}
}

func TestParse_MissingOrInvalidBytesBecomeZero(t *testing.T) {
	cases := map[string]string{
		"missing":  `<segment number="1">a@x</segment>`,
		"text":     `<segment bytes="lots" number="1">a@x</segment>`,
		"negative": `<segment bytes="-5" number="1">a@x</segment>`,
	}
	for name, seg := range cases {
		t.Run(name, func(t *testing.T) {
			n := parseString(t, oneFile(seg))
			if len(n.Files[0].Segments) != 1 {
				t.Fatalf("segment was dropped; want it kept")
			}
			if got := n.Files[0].Segments[0].Bytes; got != 0 {
				t.Errorf("bytes = %d, want 0", got)
			}
		})
	}
}

func TestParse_FileWithoutValidSegmentsDropped(t *testing.T) {
	n := parseString(t, `<nzb>
 <file poster="p" date="1" subject="empty"><segments></segments></file>
 <file poster="p" date="1" subject="all bad"><segments><segment number="1">nope</segment></segments></file>
 <file poster="p" date="1" subject="good"><segments><segment number="1">a@x</segment></segments></file>
</nzb>`)
	if len(n.Files) != 1 || n.Files[0].Subject != "good" {
		t.Errorf("files = %+v, want only the good one", n.Files)
	}
}

func parseErr(t *testing.T, s string, lim nzb.Limits) error {
	t.Helper()
	n, err := nzb.Parse(strings.NewReader(s), lim)
	if err == nil {
		t.Fatalf("Parse succeeded with %d files; want error", len(n.Files))
	}
	if n != nil {
		t.Errorf("Parse returned non-nil NZB with error")
	}
	return err
}

func TestParse_NoUsableFilesIsErrNoFiles(t *testing.T) {
	cases := map[string]string{
		"empty root":        `<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"></nzb>`,
		"only head":         `<nzb><head><meta type="name">x</meta></head></nzb>`,
		"no valid segments": oneFile(`<segment number="1">bad</segment>`),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := parseErr(t, doc, nzb.DefaultLimits()); !errors.Is(err, nzb.ErrNoFiles) {
				t.Errorf("err = %v, want ErrNoFiles", err)
			}
		})
	}
}

func TestParse_MalformedXMLIsErrMalformed(t *testing.T) {
	cases := map[string]string{
		"truncated":     `<nzb><file subject="s"><segments><segment number="1">a@x`,
		"mismatched":    `<nzb><file subject="s"></segments></file></nzb>`,
		"garbage":       `this is not xml <<<`,
		"empty input":   ``,
		"bad attribute": `<nzb><file subject=s></file></nzb>`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseErr(t, doc, nzb.DefaultLimits())
			if !errors.Is(err, nzb.ErrMalformed) {
				t.Errorf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestParse_MalformedWrapsXMLSyntaxError(t *testing.T) {
	err := parseErr(t, `<nzb><file>`, nzb.DefaultLimits())
	if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
		t.Errorf("err = %v, want it to wrap *xml.SyntaxError", err)
	}
}

func TestParse_RootMustBeNZB(t *testing.T) {
	doc := `<html><body><file subject="s"><segments><segment number="1">a@x</segment></segments></file></body></html>`
	if err := parseErr(t, doc, nzb.DefaultLimits()); !errors.Is(err, nzb.ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

// nFiles builds an NZB with n files of one valid segment each.
func nFiles(n int) string {
	var sb strings.Builder
	sb.WriteString("<nzb>")
	for range n {
		sb.WriteString(`<file subject="s"><segments><segment number="1">a@x</segment></segments></file>`)
	}
	sb.WriteString("</nzb>")
	return sb.String()
}

func TestParse_FileLimit(t *testing.T) {
	lim := nzb.DefaultLimits()
	lim.MaxFiles = 2
	if _, err := nzb.Parse(strings.NewReader(nFiles(2)), lim); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if err := parseErr(t, nFiles(3), lim); !errors.Is(err, nzb.ErrTooManyFiles) {
		t.Errorf("over the limit: err = %v, want ErrTooManyFiles", err)
	}
}

func TestParse_SegmentLimitIsTotalAcrossFiles(t *testing.T) {
	lim := nzb.DefaultLimits()
	lim.MaxSegments = 2
	if _, err := nzb.Parse(strings.NewReader(nFiles(2)), lim); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if err := parseErr(t, nFiles(3), lim); !errors.Is(err, nzb.ErrTooManySegments) {
		t.Errorf("over the limit: err = %v, want ErrTooManySegments", err)
	}
}

func TestParse_SegmentLimitCountsInvalidSegments(t *testing.T) {
	lim := nzb.DefaultLimits()
	lim.MaxSegments = 2
	doc := oneFile(`<segment number="1">a@x</segment><segment number="x">b@x</segment><segment number="2">c@x</segment>`)
	if err := parseErr(t, doc, lim); !errors.Is(err, nzb.ErrTooManySegments) {
		t.Errorf("err = %v, want ErrTooManySegments", err)
	}
}

func TestParse_InputSizeLimit(t *testing.T) {
	doc := nFiles(1)
	lim := nzb.DefaultLimits()
	lim.MaxBytes = int64(len(doc))
	if _, err := nzb.Parse(strings.NewReader(doc), lim); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	lim.MaxBytes--
	if err := parseErr(t, doc, lim); !errors.Is(err, nzb.ErrTooLarge) {
		t.Errorf("over the limit: err = %v, want ErrTooLarge", err)
	}
}

// spaces is an endless stream of XML whitespace.
type spaces struct{}

func (spaces) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestParse_TrailingDataPastLimitIsErrTooLarge(t *testing.T) {
	lim := nzb.DefaultLimits()
	lim.MaxBytes = 1 << 20
	r := io.MultiReader(strings.NewReader(nFiles(1)), spaces{})
	n, err := nzb.Parse(r, lim)
	if !errors.Is(err, nzb.ErrTooLarge) || n != nil {
		t.Errorf("Parse = %v, %v; want nil, ErrTooLarge", n, err)
	}
}

func TestParse_ReadErrorIsReturnedNotMalformed(t *testing.T) {
	boom := errors.New("connection reset")
	r := io.MultiReader(strings.NewReader(`<nzb><file subject="s">`), errReader{boom})
	_, err := nzb.Parse(r, nzb.DefaultLimits())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the read error", err)
	}
	if errors.Is(err, nzb.ErrMalformed) {
		t.Errorf("err = %v; a read error must not be reported as ErrMalformed", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestParse_Latin1DeclaredEncodingIsDecoded(t *testing.T) {
	doc := "<?xml version=\"1.0\" encoding=\"iso-8859-1\" ?>\n" +
		"<nzb><file subject=\"Caf\xe9.mkv\"><segments><segment number=\"1\">a@x</segment></segments></file></nzb>"
	n := parseString(t, doc)
	if got := n.Files[0].Subject; got != "Café.mkv" {
		t.Errorf("subject = %q, want %q", got, "Café.mkv")
	}
}

func TestParse_UnsupportedEncodingIsMalformed(t *testing.T) {
	doc := `<?xml version="1.0" encoding="shift_jis"?>` + nFiles(1)
	if err := parseErr(t, doc, nzb.DefaultLimits()); !errors.Is(err, nzb.ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

func TestParse_DeeplyNestedElementsDoNotCrash(t *testing.T) {
	const depth = 1_000_000
	nest := strings.Repeat("<x>", depth) + strings.Repeat("</x>", depth)
	cases := map[string]string{
		"under root":    `<nzb>` + nest + validBody + `</nzb>`,
		"under segment": oneFile(`<segment number="1">a@x` + nest + `</segment>`),
		"under head":    `<nzb><head>` + nest + `</head>` + validBody + `</nzb>`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			n := parseString(t, doc)
			if len(n.Files) != 1 || n.Files[0].Segments[0].MessageID != "a@x" {
				t.Errorf("unknown nested elements were not skipped cleanly: %+v", n.Files)
			}
		})
	}
}

func TestParse_PrefixedNamespaceAccepted(t *testing.T) {
	n := parseString(t, `<n:nzb xmlns:n="http://www.newzbin.com/DTD/2003/nzb">
 <n:head><n:meta type="name">x</n:meta></n:head>
 <n:file subject="s"><n:segments><n:segment number="1">a@x</n:segment></n:segments></n:file>
</n:nzb>`)
	if n.Meta.Name != "x" || len(n.Files) != 1 {
		t.Errorf("got %+v", n)
	}
}

func TestParse_ExtremeByteLimitsDoNotPanic(t *testing.T) {
	doc := nFiles(1)
	lim := nzb.DefaultLimits()

	lim.MaxBytes = math.MaxInt64
	if _, err := nzb.Parse(strings.NewReader(doc), lim); err != nil {
		t.Errorf("MaxInt64: %v", err)
	}
	for _, max := range []int64{0, -1, math.MinInt64} {
		lim.MaxBytes = max
		if err := parseErr(t, doc, lim); !errors.Is(err, nzb.ErrTooLarge) {
			t.Errorf("MaxBytes=%d: err = %v, want ErrTooLarge", max, err)
		}
	}
}
