package nntptest

import (
	"fmt"
	"hash/crc32"
	"strings"
)

// DefaultLineLength is the number of encoded characters per data line that
// BuildFixture uses, as most posters do.
const DefaultLineLength = 128

// EncodeArticles yEnc-encodes file as Article bodies of at most partSize
// decoded bytes each, with data lines of lineLen encoded characters (one
// more when the last character is escaped). Each body is returned as lines
// without terminators, ready for Server.AddArticle. A file that fits in one
// part is encoded as a single-part file: no part=, no =ypart, and crc32= in
// the trailer. An empty file gives one article without data lines.
//
// It does not use the yenc package, so a decoder bug cannot hide behind a
// matching encoder bug.
func EncodeArticles(name string, file []byte, partSize, lineLen int) [][]string {
	if len(file) <= partSize {
		lines := []string{fmt.Sprintf("=ybegin line=%d size=%d name=%s", lineLen, len(file), name)}
		lines = append(lines, encodeLines(file, lineLen)...)
		lines = append(lines, fmt.Sprintf("=yend size=%d crc32=%08x", len(file), crc32.ChecksumIEEE(file)))
		return [][]string{lines}
	}
	total := (len(file) + partSize - 1) / partSize
	articles := make([][]string, 0, total)
	for n := 1; n <= total; n++ {
		begin := (n - 1) * partSize
		end := min(begin+partSize, len(file))
		data := file[begin:end]
		lines := []string{
			fmt.Sprintf("=ybegin part=%d total=%d line=%d size=%d name=%s", n, total, lineLen, len(file), name),
			fmt.Sprintf("=ypart begin=%d end=%d", begin+1, end),
		}
		lines = append(lines, encodeLines(data, lineLen)...)
		trailer := fmt.Sprintf("=yend size=%d part=%d pcrc32=%08x", len(data), n, crc32.ChecksumIEEE(data))
		if n == total {
			trailer += fmt.Sprintf(" crc32=%08x", crc32.ChecksumIEEE(file))
		}
		articles = append(articles, append(lines, trailer))
	}
	return articles
}

// encodeLines encodes data as yEnc data lines. NUL, LF, CR and '=' are
// always escaped; tab and space are escaped at the start or end of a line,
// and '.' at the start of a line, so the lines survive NNTP transport.
func encodeLines(data []byte, lineLen int) []string {
	var lines []string
	line := make([]byte, 0, lineLen+1)
	for i, b := range data {
		c := b + 42
		first := len(line) == 0
		last := len(line)+1 >= lineLen || i == len(data)-1
		var escape bool
		switch c {
		case 0, '\n', '\r', '=':
			escape = true
		case '\t', ' ':
			escape = first || last
		case '.':
			escape = first
		}
		if escape {
			line = append(line, '=', c+64)
		} else {
			line = append(line, c)
		}
		if len(line) >= lineLen {
			lines = append(lines, string(line))
			line = line[:0]
		}
	}
	if len(line) > 0 {
		lines = append(lines, string(line))
	}
	return lines
}

// corruptLines returns a copy of a body with one byte changed so that its
// CRC32 no longer matches while the structure and the decoded length stay
// valid: a data character is replaced by another plain one. A body without
// data lines gets a wrong CRC in its trailer instead.
func corruptLines(lines []string) []string {
	out := append([]string(nil), lines...)
	for i, l := range out {
		if strings.HasPrefix(l, "=y") {
			continue
		}
		for j := range len(l) {
			if l[j] == '=' || (j > 0 && l[j-1] == '=') {
				continue // part of an escape sequence
			}
			if repl, ok := plainNeighbor(l[j]); ok {
				out[i] = l[:j] + string([]byte{repl}) + l[j+1:]
				return out
			}
		}
	}
	for i, l := range out {
		if strings.HasPrefix(l, "=yend ") {
			out[i] = flipLastHexDigit(l)
			return out
		}
	}
	return out
}

// plainNeighbor finds a different byte that needs no escaping anywhere in a
// line.
func plainNeighbor(c byte) (byte, bool) {
	for bit := range 8 {
		r := c ^ (1 << bit)
		switch r {
		case 0, '\n', '\r', '=', ' ', '\t', '.':
			continue
		}
		return r, true
	}
	return 0, false
}

// flipLastHexDigit changes the last hex digit of the first crc32 value in a
// =yend line.
func flipLastHexDigit(l string) string {
	for _, key := range []string{"pcrc32=", "crc32="} {
		if at := strings.Index(l, key); at >= 0 {
			end := at + len(key) + 8
			if end <= len(l) {
				d := byte('0')
				if l[end-1] == '0' {
					d = '1'
				}
				return l[:end-1] + string(d) + l[end:]
			}
		}
	}
	return l
}
