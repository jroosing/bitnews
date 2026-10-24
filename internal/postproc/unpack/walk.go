package unpack

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jroosing/bitnews/internal/fsutil"
)

// walker checks and normalizes the tree 7zz wrote into the staging directory.
// It trusts nothing 7zz was told: it looks at what is on disk.
//
//   - A symlink, hardlink, device, FIFO or socket anywhere fails the Unpack
//     with ErrPathViolation (the listing refused them already; this is the
//     second line of defence, for a listing that lied or a 7zz that did more
//     than it was asked).
//   - Every name goes through fsutil.SafeName; a name it changes ("a:b",
//     "con.txt", trailing dots) is renamed in place, and two names that end up
//     equal once case is ignored fail with ErrPathViolation.
//   - Files get mode 0644 and directories 0755, so an archive cannot plant a
//     setuid or executable file, or a directory the daemon cannot delete.
type walker struct {
	maxEntries int
	files      []string
	bytes      int64
	count      int
}

func (w *walker) run(ctx context.Context, root string) error {
	return w.dir(ctx, root, "", 0)
}

func (w *walker) dir(ctx context.Context, abs, rel string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > maxPathDepth {
		return fmt.Errorf("%w: directories nested deeper than %d", ErrPathViolation, maxPathDepth)
	}
	if err := os.Chmod(abs, 0o755); err != nil {
		return fmt.Errorf("unpack: normalize directory mode: %w", err)
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	// Decide every final name first, so a rename can never land on a name
	// that another entry of the directory still has.
	final := make([]string, len(ents))
	seen := make(map[string]bool, len(ents))
	for i, e := range ents {
		safe, err := fsutil.SafeName(e.Name())
		if err != nil {
			return fmt.Errorf("%w: unusable name %q in the extracted tree", ErrPathViolation, truncateForError(e.Name()))
		}
		key := strings.ToLower(safe)
		if seen[key] {
			return fmt.Errorf("%w: names collide in the extracted tree: %q", ErrPathViolation, truncateForError(path(rel, safe)))
		}
		seen[key] = true
		final[i] = safe
	}
	for i, e := range ents {
		w.count++
		if w.count > w.maxEntries*2+1 {
			return fmt.Errorf("%w: more than %d entries extracted", ErrTooManyEntries, w.maxEntries)
		}
		from := filepath.Join(abs, e.Name())
		fi, err := os.Lstat(from)
		if err != nil {
			return err
		}
		kind := modeKind(fi.Mode())
		if kind != kindFile && kind != kindDir {
			return fmt.Errorf("%w: the extracted tree has a %s %q", ErrPathViolation, kind, truncateForError(path(rel, e.Name())))
		}
		to := from
		if final[i] != e.Name() {
			to = filepath.Join(abs, final[i])
			if _, err := os.Lstat(to); err == nil {
				return fmt.Errorf("%w: names collide in the extracted tree: %q", ErrPathViolation, truncateForError(path(rel, final[i])))
			}
			if err := os.Rename(from, to); err != nil {
				return err
			}
		}
		if !within(abs, to) {
			return fmt.Errorf("%w: %q leaves the staging directory", ErrPathViolation, truncateForError(path(rel, final[i])))
		}
		childRel := path(rel, final[i])
		if kind == kindDir {
			if err := w.dir(ctx, to, childRel, depth+1); err != nil {
				return err
			}
			continue
		}
		if linkCount(fi) > 1 {
			return fmt.Errorf("%w: the extracted file %q is a hardlink", ErrPathViolation, truncateForError(childRel))
		}
		if err := os.Chmod(to, 0o644); err != nil {
			return fmt.Errorf("unpack: normalize file mode: %w", err)
		}
		w.files = append(w.files, childRel)
		w.bytes = addSat(w.bytes, fi.Size())
	}
	return nil
}

// within reports whether p is a direct child of dir, lexically.
func within(dir, p string) bool {
	return filepath.Dir(p) == dir && p != dir
}

func path(rel, name string) string {
	if rel == "" {
		return name
	}
	return rel + "/" + name
}

// modeKind classifies a file mode from Lstat.
func modeKind(m fs.FileMode) entryKind {
	switch {
	case m.IsRegular():
		return kindFile
	case m.IsDir():
		return kindDir
	case m&fs.ModeSymlink != 0:
		return kindSymlink
	case m&fs.ModeDevice != 0:
		return kindDevice
	case m&fs.ModeNamedPipe != 0:
		return kindFIFO
	}
	return kindOther
}
