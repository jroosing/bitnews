package blackbox_test

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
)

// A release is an NZB together with the articles it points at and the files
// they decode to. The yEnc encoder is the independent one of package
// nntptest, so the daemon's decoder is checked against known bytes.

// relSpec describes one file of a release.
type relSpec struct {
	// Name is the name in the yEnc =ybegin header, which names the file on
	// disk.
	Name string
	// Subject is the NZB subject; the default quotes Name.
	Subject string
	Data    []byte
}

// relFile is a file of a release as the NZB lists it.
type relFile struct {
	Name    string
	Data    []byte
	IDs     []string // message ID of each part, in part order
	Encoded []int    // the NZB segment size of each part
	Offsets []int    // where each part starts in Data
}

// release is the NZB plus the articles.
type release struct {
	NZB      []byte
	Files    []relFile
	articles map[string][]string
	// Total is the sum of all segment sizes in the NZB.
	Total int64
}

// IDs lists every message ID of the release, file by file.
func (r release) IDs() []string {
	var ids []string
	for _, f := range r.Files {
		ids = append(ids, f.IDs...)
	}
	return ids
}

// Decoded is the sum of the sizes of the files.
func (r release) Decoded() int64 {
	var n int64
	for _, f := range r.Files {
		n += int64(len(f.Data))
	}
	return n
}

// Segments is the number of articles of the release.
func (r release) Segments() int { return len(r.IDs()) }

type relOptions struct {
	date     time.Time
	meta     [][2]string
	lineLen  int
	partSize int
}

type relOption func(*relOptions)

// relMeta adds a <meta type="..."> head element.
func relMeta(typ, value string) relOption {
	return func(o *relOptions) { o.meta = append(o.meta, [2]string{typ, value}) }
}

// relDate sets the posting date of every file.
func relDate(t time.Time) relOption { return func(o *relOptions) { o.date = t } }

var relCounter atomic.Int64

// newRelease encodes the files and writes an NZB for them. Message IDs are
// unique per call, so releases never collide on a server.
func newRelease(t testing.TB, files []relSpec, opts ...relOption) release {
	t.Helper()
	o := relOptions{date: time.Now().Add(-24 * time.Hour), lineLen: nntptest.DefaultLineLength, partSize: 3000}
	for _, f := range opts {
		f(&o)
	}
	tag := fmt.Sprintf("r%d%x", relCounter.Add(1), rand.Uint32())
	rel := release{articles: map[string][]string{}}
	var doc bytes.Buffer
	doc.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	doc.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	if len(o.meta) > 0 {
		doc.WriteString("  <head>\n")
		for _, m := range o.meta {
			fmt.Fprintf(&doc, "    <meta type=%q>%s</meta>\n", m[0], xmlText(m[1]))
		}
		doc.WriteString("  </head>\n")
	}
	for fi, spec := range files {
		parts := nntptest.EncodeArticles(spec.Name, spec.Data, o.partSize, o.lineLen)
		f := relFile{Name: spec.Name, Data: spec.Data}
		var segs strings.Builder
		for i, lines := range parts {
			id := fmt.Sprintf("%s.%d.%d@bb.test", tag, fi, i+1)
			size := 0
			for _, l := range lines {
				size += len(l) + 2
			}
			f.IDs = append(f.IDs, id)
			f.Encoded = append(f.Encoded, size)
			f.Offsets = append(f.Offsets, i*o.partSize)
			rel.articles[id] = lines
			rel.Total += int64(size)
			fmt.Fprintf(&segs, "      <segment bytes=\"%d\" number=\"%d\">%s</segment>\n", size, i+1, id)
		}
		rel.Files = append(rel.Files, f)
		subject := spec.Subject
		if subject == "" {
			subject = fmt.Sprintf(`"%s" yEnc (1/%d)`, spec.Name, len(parts))
		}
		fmt.Fprintf(&doc, "  <file poster=\"poster@bb.test\" date=\"%d\" subject=\"%s\">\n", o.date.Unix(), xmlText(subject))
		doc.WriteString("    <groups><group>alt.binaries.bb</group></groups>\n    <segments>\n")
		doc.WriteString(segs.String())
		doc.WriteString("    </segments>\n  </file>\n")
	}
	doc.WriteString("</nzb>\n")
	rel.NZB = doc.Bytes()
	return rel
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// randBytes returns n deterministic pseudo-random bytes. Every byte value
// occurs, so the yEnc escapes get used.
func randBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}

// oneFile is a release of one random file with the given number of articles
// (the last one shorter).
func oneFile(t testing.TB, name string, seed uint64, parts int, opts ...relOption) release {
	t.Helper()
	size := parts*3000 - 1234
	if parts == 1 {
		size = 2000
	}
	return newRelease(t, []relSpec{{Name: name, Data: randBytes(seed, size)}}, opts...)
}

// requireSameBytes asserts equal contents without printing megabytes.
func requireSameBytes(t testing.TB, want, got []byte, msg string) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	at := 0
	for at < len(want) && at < len(got) && want[at] == got[at] {
		at++
	}
	require.Failf(t, "different bytes", "%s: want %d bytes, got %d; first difference at offset %d", msg, len(want), len(got), at)
}
