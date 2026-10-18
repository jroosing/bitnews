package unpack

import (
	"fmt"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

const (
	// maxPathBytes and maxPathDepth bound a sanitized entry path. Real
	// releases stay far below both.
	maxPathBytes = 4096
	maxPathDepth = 128
)

// cleanName normalizes an archive entry name into a relative path with '/'
// separators in which every component went through fsutil.SafeName. It rejects
// absolute paths, drive letters, "..", NUL bytes and empty names with
// ErrPathViolation. It returns "" for names that mean the root itself, such as
// "./".
func cleanName(raw string) (string, error) {
	violation := func(why string) (string, error) {
		return "", fmt.Errorf("%w: %s %q", ErrPathViolation, why, truncateForError(raw))
	}
	if raw == "" {
		return violation("empty name")
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return violation("NUL byte in name")
	}
	if len(raw) > 4*maxPathBytes {
		return violation("name too long")
	}
	s := strings.ReplaceAll(raw, `\`, "/")
	if s[0] == '/' {
		return violation("absolute path")
	}
	if len(s) >= 2 && s[1] == ':' && isASCIILetter(s[0]) {
		return violation("drive letter")
	}
	var comps []string
	sawDot := false
	for c := range strings.SplitSeq(s, "/") {
		switch c {
		case "":
			continue
		case ".":
			sawDot = true
			continue
		case "..":
			return violation("parent directory reference")
		}
		safe, err := fsutil.SafeName(c)
		if err != nil {
			return violation("invalid name component")
		}
		comps = append(comps, safe)
	}
	if len(comps) == 0 {
		if sawDot {
			return "", nil
		}
		return violation("empty name")
	}
	if len(comps) > maxPathDepth {
		return violation("path too deep")
	}
	rel := strings.Join(comps, "/")
	if len(rel) > maxPathBytes {
		return violation("path too long")
	}
	return rel, nil
}

func isASCIILetter(b byte) bool { return b|0x20 >= 'a' && b|0x20 <= 'z' }

func truncateForError(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
