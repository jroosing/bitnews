package yenc_test

// A yEnc 1.3 encoder for tests. It has no dependencies on the decoder so it
// can be lifted into other test harnesses, such as the fake NNTP server.

import (
	"fmt"
	"hash/crc32"
)

// encodeArticles yEnc-encodes file as Article bodies of at most partSize
// decoded bytes each, with data lines of lineLen encoded characters (one
// more when the last character is escaped). Each body is returned as lines
// without terminators. A file that fits in one part is encoded as a
// single-part file: no part=, no =ypart, and crc32= in the trailer.
func encodeArticles(name string, file []byte, partSize, lineLen int) [][]string {
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
