package nzb

import (
	"cmp"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Limits bounds the resources Parse may spend on one NZB. Counts include
// elements that are later dropped as invalid, so the work done is bounded
// regardless of how much of the input survives normalization. A zero or
// negative field allows nothing; start from DefaultLimits.
type Limits struct {
	MaxBytes    int64 // input bytes, including anything after </nzb>
	MaxFiles    int   // <file> elements
	MaxSegments int   // <segment> elements, summed over all files
}

// DefaultLimits returns the limits used for NZBs from untrusted sources.
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:    64 << 20,
		MaxFiles:    10_000,
		MaxSegments: 1_000_000,
	}
}

// Parse reads an NZB document from r and returns its normalized model.
//
// Both the standard namespace (http://www.newzbin.com/DTD/2003/nzb) and
// documents without one are accepted; elements are matched by local name.
// For each file, segments are sorted by number, duplicates are dropped
// (the first in the document is kept), and files left without segments
// are dropped. The error is one of the package's sentinel errors, or a
// wrapped read error from r.
func Parse(r io.Reader, lim Limits) (*NZB, error) {
	cr := &capReader{r: r, left: max(lim.MaxBytes, 0), max: max(lim.MaxBytes, 0)}
	p := &parser{d: xml.NewDecoder(cr), lim: lim, seenMeta: map[string]bool{}}
	p.d.Strict = true // Entity is left nil: no entity beyond the XML built-ins is expanded
	p.d.CharsetReader = charsetReader
	if err := p.parse(); err != nil {
		return nil, cr.classify(err)
	}
	if len(p.nzb.Files) == 0 {
		return nil, ErrNoFiles
	}
	return &p.nzb, nil
}

type parser struct {
	d        *xml.Decoder
	lim      Limits
	files    int // <file> elements seen
	segments int // <segment> elements seen
	nzb      NZB
	seenMeta map[string]bool
}

func (p *parser) parse() error {
	root, err := p.rootElement()
	if err != nil {
		return err
	}
	if root.Name.Local != "nzb" {
		return fmt.Errorf("%w: root element is <%s>, want <nzb>", ErrMalformed, root.Name.Local)
	}
	if err := p.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "head":
			return p.parseHead()
		case "file":
			return p.parseFile(se)
		}
		return p.d.Skip()
	}); err != nil {
		return err
	}
	return p.drain()
}

// rootElement returns the document's first start element.
func (p *parser) rootElement() (xml.StartElement, error) {
	for {
		tok, err := p.d.Token()
		if errors.Is(err, io.EOF) {
			return xml.StartElement{}, fmt.Errorf("%w: no root element", ErrMalformed)
		}
		if err != nil {
			return xml.StartElement{}, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se, nil
		}
	}
}

// children calls fn for each child element of the element just opened and
// returns once that element ends. fn must consume the child completely.
func (p *parser) children(fn func(xml.StartElement) error) error {
	for {
		tok, err := p.d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := fn(t); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

// drain reads the rest of the input so that trailing bytes are still
// checked for well-formedness and counted against the size limit.
func (p *parser) drain() error {
	for {
		if _, err := p.d.Token(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (p *parser) parseHead() error {
	return p.children(func(se xml.StartElement) error {
		if se.Name.Local == "meta" {
			return p.parseMeta(se)
		}
		return p.d.Skip()
	})
}

func (p *parser) parseMeta(start xml.StartElement) error {
	v, err := p.text()
	if err != nil {
		return err
	}
	typ := strings.ToLower(strings.TrimSpace(attr(start, "type")))
	var dst *string
	switch typ {
	case "name":
		dst = &p.nzb.Meta.Name
	case "password":
		dst = &p.nzb.Meta.Password
	case "category":
		dst = &p.nzb.Meta.Category
	default:
		return nil
	}
	if !p.seenMeta[typ] {
		p.seenMeta[typ] = true
		*dst = strings.TrimSpace(v)
	}
	return nil
}

func (p *parser) parseFile(start xml.StartElement) error {
	if p.files++; p.files > p.lim.MaxFiles {
		return fmt.Errorf("%w: more than %d", ErrTooManyFiles, p.lim.MaxFiles)
	}
	f := File{
		Subject: attr(start, "subject"),
		Poster:  attr(start, "poster"),
	}
	if sec, err := strconv.ParseInt(strings.TrimSpace(attr(start, "date")), 10, 64); err == nil {
		f.Date = time.Unix(sec, 0).UTC()
	}
	err := p.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "groups":
			return p.children(func(se xml.StartElement) error {
				if se.Name.Local != "group" {
					return p.d.Skip()
				}
				g, err := p.text()
				if g = strings.TrimSpace(g); err == nil && g != "" {
					f.Groups = append(f.Groups, g)
				}
				return err
			})
		case "segments":
			return p.children(func(se xml.StartElement) error {
				if se.Name.Local != "segment" {
					return p.d.Skip()
				}
				s, ok, err := p.parseSegment(se)
				if err != nil {
					return err
				}
				if !ok {
					p.nzb.SkippedSegments++
					return nil
				}
				f.Segments = append(f.Segments, s)
				return nil
			})
		}
		return p.d.Skip()
	})
	if err != nil {
		return err
	}
	normalizeSegments(&f)
	f.Name = guessName(f.Subject)
	f.Kind, f.RecoveryBlocks = classifyName(f.Name)
	if len(f.Segments) > 0 {
		p.nzb.Files = append(p.nzb.Files, f)
	}
	return nil
}

// parseSegment reads one <segment>. ok is false when the segment is invalid
// and must be skipped.
func (p *parser) parseSegment(start xml.StartElement) (seg Segment, ok bool, err error) {
	if p.segments++; p.segments > p.lim.MaxSegments {
		return Segment{}, false, fmt.Errorf("%w: more than %d", ErrTooManySegments, p.lim.MaxSegments)
	}
	text, err := p.text()
	if err != nil {
		return Segment{}, false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(attr(start, "number")))
	if err != nil || n < 1 {
		return Segment{}, false, nil
	}
	b, err := strconv.ParseInt(strings.TrimSpace(attr(start, "bytes")), 10, 64)
	if err != nil || b < 0 {
		b = 0 // size is informational; unknown is allowed
	}
	id := strings.TrimSpace(text)
	id = strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">")
	if !validMessageID(id) {
		return Segment{}, false, nil
	}
	return Segment{Number: n, Bytes: b, MessageID: id}, true, nil
}

// maxMessageIDLen bounds a message ID (without angle brackets).
const maxMessageIDLen = 250

// validMessageID reports whether id is safe to send in an NNTP command:
// 1..250 printable ASCII characters (0x21-0x7E) including an '@', and no
// angle brackets, which frame the ID in "BODY <id>".
func validMessageID(id string) bool {
	if id == "" || len(id) > maxMessageIDLen || !strings.Contains(id, "@") {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e || id[i] == '<' || id[i] == '>' {
			return false
		}
	}
	return true
}

// text collects the character data of the current element up to its end.
func (p *parser) text() (string, error) {
	var sb strings.Builder
	depth := 0
	for {
		tok, err := p.d.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if depth == 0 {
				sb.Write(t)
			}
		case xml.StartElement:
			depth++
		case xml.EndElement:
			if depth == 0 {
				return sb.String(), nil
			}
			depth--
		}
	}
}

func attr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// normalizeSegments orders segments by number and drops duplicate numbers.
// The sort is stable, so the duplicate kept is the first in the document.
func normalizeSegments(f *File) {
	slices.SortStableFunc(f.Segments, func(a, b Segment) int { return cmp.Compare(a.Number, b.Number) })
	f.Segments = slices.CompactFunc(f.Segments, func(a, b Segment) bool { return a.Number == b.Number })
}

// capReader passes reads through until more than left bytes would be
// consumed, then fails with ErrTooLarge instead of truncating silently.
type capReader struct {
	r       io.Reader
	left    int64 // bytes still allowed
	max     int64 // the limit, for error messages
	readErr error // first non-EOF error from r, or ErrTooLarge
}

func (c *capReader) Read(b []byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	// Ask for at most one byte more than allowed so overflow is detected.
	// Comparing before adding keeps left+1 from overflowing.
	if c.left < int64(len(b)) {
		b = b[:c.left+1]
	}
	n, err := c.r.Read(b)
	if int64(n) > c.left {
		c.readErr = fmt.Errorf("%w: limit is %d bytes", ErrTooLarge, c.max)
		return 0, c.readErr
	}
	c.left -= int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		c.readErr = err
	}
	return n, err
}

// classify maps an error from parsing to the package's error kinds:
// errors from the parser itself and from the input reader pass through,
// anything else came from encoding/xml and means the document is malformed.
func (c *capReader) classify(err error) error {
	switch {
	case errors.Is(err, ErrTooLarge):
		return err
	case c.readErr != nil && errors.Is(err, c.readErr):
		return fmt.Errorf("nzb: read: %w", err)
	case errors.Is(err, ErrMalformed), errors.Is(err, ErrTooManyFiles), errors.Is(err, ErrTooManySegments):
		return err
	}
	return fmt.Errorf("%w: %w", ErrMalformed, err)
}
