package nzb

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// charsetReader lets the XML decoder read documents that declare a
// non-UTF-8 encoding. Only ISO-8859-1 is supported: it is what the original
// newzbin NZB DTD examples declare. Anything else is rejected.
func charsetReader(label string, r io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1":
		return &latin1Reader{r: r}, nil
	}
	return nil, fmt.Errorf("unsupported encoding %q", label)
}

// latin1Reader transcodes ISO-8859-1 to UTF-8. Every byte maps to the rune
// of the same value, so output is at most twice the input.
type latin1Reader struct {
	r   io.Reader
	in  []byte
	out []byte // transcoded bytes not yet returned
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	if len(l.out) == 0 {
		if cap(l.in) == 0 {
			l.in = make([]byte, 4096)
		}
		n, err := l.r.Read(l.in[:cap(l.in)])
		for _, b := range l.in[:n] {
			l.out = utf8.AppendRune(l.out, rune(b))
		}
		if n == 0 {
			return 0, err
		}
	}
	n := copy(p, l.out)
	l.out = l.out[n:]
	return n, nil
}
