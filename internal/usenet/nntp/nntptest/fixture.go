package nntptest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Fixture is an NZB together with the Articles it points at.
type Fixture struct {
	// NZB is the document, parseable by package nzb.
	NZB []byte
	// Articles maps message ID to the yEnc body lines of each Article.
	Articles map[string][]string
	// Files describes the posted files, sorted by name.
	Files []FixtureFile
}

// FixtureFile is one file of a Fixture.
type FixtureFile struct {
	Name string
	Data []byte
	// MessageIDs lists the Article of each part, in part order.
	MessageIDs []string
}

// MessageIDs lists every message ID of the Fixture, file by file in part
// order.
func (f Fixture) MessageIDs() []string {
	var ids []string
	for _, file := range f.Files {
		ids = append(ids, file.MessageIDs...)
	}
	return ids
}

// Install adds all Articles of the Fixture to s. It may be called for
// several Servers, and for several Fixtures on one Server because message
// IDs depend on the file name and content.
func (f Fixture) Install(s *Server) {
	for id, lines := range f.Articles {
		s.AddArticle(id, lines)
	}
}

// FixtureOption tunes BuildFixture.
type FixtureOption func(*fixtureConfig)

type fixtureConfig struct {
	date    time.Time
	lineLen int
}

// WithDate sets the posting time written into the NZB, which the scheduler
// compares with Server retention. The default is 2024-01-01 UTC.
func WithDate(d time.Time) FixtureOption { return func(c *fixtureConfig) { c.date = d } }

// WithLineLength sets the encoded characters per data line (default
// DefaultLineLength).
func WithLineLength(n int) FixtureOption { return func(c *fixtureConfig) { c.lineLen = n } }

// BuildFixture yEnc-encodes every file into Articles of at most partSize
// decoded bytes and writes an NZB listing them. The result is deterministic:
// the same input gives the same document and message IDs. File names must be
// non-empty and free of quotes and control characters; a bad argument fails
// the test.
func BuildFixture(t testing.TB, files map[string][]byte, partSize int, opts ...FixtureOption) Fixture {
	t.Helper()
	cfg := fixtureConfig{date: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), lineLen: DefaultLineLength}
	for _, o := range opts {
		o(&cfg)
	}
	if partSize <= 0 || cfg.lineLen <= 0 {
		t.Fatalf("nntptest: partSize %d and line length %d must be positive", partSize, cfg.lineLen)
	}

	names := make([]string, 0, len(files))
	for name := range files {
		if name == "" || strings.ContainsAny(name, "\"\r\n\x00") {
			t.Fatalf("nntptest: unusable file name %q", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)

	fx := Fixture{Articles: map[string][]string{}}
	var doc bytes.Buffer
	doc.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	doc.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	for _, name := range names {
		data := files[name]
		sum := sha256.Sum256(append([]byte(name+"\x00"), data...))
		prefix := hex.EncodeToString(sum[:8])

		articles := EncodeArticles(name, data, partSize, cfg.lineLen)
		ff := FixtureFile{Name: name, Data: data}
		var segs strings.Builder
		for i, lines := range articles {
			id := fmt.Sprintf("%s.%d@fixture.test", prefix, i+1)
			ff.MessageIDs = append(ff.MessageIDs, id)
			fx.Articles[id] = lines
			size := 0
			for _, l := range lines {
				size += len(l) + 2
			}
			fmt.Fprintf(&segs, "      <segment bytes=\"%d\" number=\"%d\">%s</segment>\n", size, i+1, id)
		}
		fx.Files = append(fx.Files, ff)

		subject := fmt.Sprintf(`"%s" yEnc (1/%d)`, name, len(articles))
		fmt.Fprintf(&doc, "  <file poster=\"poster@fixture.test\" date=\"%d\" subject=\"%s\">\n", cfg.date.Unix(), escapeAttr(subject))
		doc.WriteString("    <groups><group>alt.binaries.fixture</group></groups>\n    <segments>\n")
		doc.WriteString(segs.String())
		doc.WriteString("    </segments>\n  </file>\n")
	}
	doc.WriteString("</nzb>\n")
	fx.NZB = doc.Bytes()
	return fx
}

func escapeAttr(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
