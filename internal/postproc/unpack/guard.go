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
		s = s[:80] + "..."
	}
	// Names are hostile: keep control characters out of error messages and
	// logs.
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// guard applies the per-entry rules to one listing.
type guard struct {
	cfg *config
	// count is the number of entries seen, files is the number of files.
	count, files int
	declared     int64
	seen         map[string]bool // lower-cased path -> is a directory
}

func newGuard(cfg *config) *guard {
	return &guard{cfg: cfg, seen: map[string]bool{}}
}

// admit checks one entry of the listing. Only files and directories pass: a
// symlink, hardlink, device, FIFO or socket fails the whole Unpack with
// ErrPathViolation, as does any unsafe or colliding name.
func (g *guard) admit(e *entry) error {
	g.count++
	if g.count > g.cfg.maxEntries {
		return fmt.Errorf("%w: more than %d", ErrTooManyEntries, g.cfg.maxEntries)
	}
	rel, err := cleanName(e.name)
	if err != nil {
		return err
	}
	switch e.kind {
	case kindFile, kindDir:
	default:
		return fmt.Errorf("%w: the archive contains a %s %q", ErrPathViolation, e.kind, truncateForError(e.name))
	}
	if rel == "" {
		if e.kind == kindDir {
			return nil
		}
		return fmt.Errorf("%w: file named after the root %q", ErrPathViolation, truncateForError(e.name))
	}
	if err := g.claim(rel, e.kind == kindDir); err != nil {
		return err
	}
	if e.kind == kindFile {
		g.files++
		g.declared = addSat(g.declared, e.size)
	}
	return nil
}

// claim records rel and its parents, refusing a name that collides with an
// earlier one once case is ignored.
func (g *guard) claim(rel string, isDir bool) error {
	for i := 0; i < len(rel); i++ {
		if rel[i] != '/' {
			continue
		}
		parent := strings.ToLower(rel[:i])
		if dir, seen := g.seen[parent]; seen && !dir {
			return fmt.Errorf("%w: %q is both a file and a directory", ErrPathViolation, truncateForError(rel))
		}
		g.seen[parent] = true
	}
	key := strings.ToLower(rel)
	dir, seen := g.seen[key]
	switch {
	case !seen:
		g.seen[key] = isDir
	case dir && isDir:
	default:
		return fmt.Errorf("%w: duplicate entry %q", ErrPathViolation, truncateForError(rel))
	}
	return nil
}
