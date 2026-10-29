package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// urlInText finds URLs in free text: a scheme followed by "://" or, as in a
// magnet link, by ":?".
var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*:(?://|\?)[^\s"']+`)

// URLsIn redacts every URL in free text, such as an error message or a log
// line: each is passed through URL. A magnet link keeps only its scheme,
// because its trackers carry passkeys.
func URLsIn(s string) string {
	return urlInText.ReplaceAllStringFunc(s, func(u string) string {
		if len(u) >= 7 && strings.EqualFold(u[:7], "magnet:") {
			return "magnet:?[redacted]"
		}
		return URL(u)
	})
}

// Printable keeps at most n bytes of s and replaces everything but printable
// ASCII with "?", for log lines.
func Printable(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return '?'
		}
		return r
	}, s)
	if len(s) > n {
		s = s[:n]
	}
	return s
}
