package fsutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxNameBytes = 255
	// maxExtBytes bounds what counts as an extension (including the dot) when
	// truncating, so that a hostile ".aaaa…" suffix cannot pin the whole budget.
	maxExtBytes = 32
)

// SafeName turns an untrusted string (NZB subject, yEnc name=, archive entry,
// torrent file name) into a single safe file name component.
//
// It never returns a name that could be a path or that Windows/SMB shares would
// treat specially:
//
//   - invalid UTF-8 bytes become '_';
//   - control characters (all of Unicode C*: C0/C1, DEL, bidi overrides and
//     isolates, zero-width and format characters, private use) and the line and
//     paragraph separators are removed;
//   - '/', '\' and the SMB-unsafe characters : * ? " < > | become '_';
//   - leading spaces and trailing spaces and dots are trimmed;
//   - Windows device names (CON, PRN, AUX, NUL, COM1-9, LPT1-9, with any
//     extension, case-insensitive) get a '_' prefix;
//   - the result is capped at 255 bytes by shortening the base name on a rune
//     boundary and keeping the extension (an "extension" is the part from the
//     last dot, at most 32 bytes, and never the whole of a dotfile).
//
// An empty result, "." or ".." is rejected with ErrInvalidName.
//
// Unicode normalization is deliberately limited to the above: names are not
// converted to NFC, because that needs golang.org/x/text. Two canonically
// equivalent spellings therefore stay distinct names.
func SafeName(s string) (string, error) {
	s = strings.Map(mapRune, sanitizeUTF8(s))
	s = strings.TrimLeft(s, " ")
	s = strings.TrimRight(s, " .")
	if s == "" || s == "." || s == ".." {
		return "", ErrInvalidName
	}
	if isWindowsReserved(s) {
		s = "_" + s
	}
	s = capName(s)
	if s == "" || s == "." || s == ".." {
		return "", ErrInvalidName
	}
	return s, nil
}

// sanitizeUTF8 replaces every invalid byte with '_'.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('_')
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func mapRune(r rune) rune {
	switch {
	case unicode.In(r, unicode.C, unicode.Zl, unicode.Zp):
		return -1
	case strings.ContainsRune(`/\:*?"<>|`, r):
		return '_'
	}
	return r
}

// isWindowsReserved reports whether name, ignoring everything from the first
// dot and trailing spaces before it, is a Windows device name.
func isWindowsReserved(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) > 3 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
		switch stem[3:] {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}

// splitExt splits name at its last dot into base and extension (with dot).
// A leading dot or an overlong extension is not treated as an extension.
func splitExt(name string) (base, ext string) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || len(name)-i > maxExtBytes {
		return name, ""
	}
	return name[:i], name[i:]
}

// capName truncates name to maxNameBytes, cutting the base on a rune boundary
// and keeping the extension.
func capName(name string) string {
	if len(name) <= maxNameBytes {
		return name
	}
	base, ext := splitExt(name)
	base = truncateRunes(base, maxNameBytes-len(ext))
	base = strings.TrimRight(base, " .")
	return base + ext
}

// truncateRunes cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
