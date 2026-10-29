package core

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Category is an optional label on a Download that decides where its Ready
// output lands and which Media manager owns it. Categories are shared across
// Usenet and torrents; each protocol has its own folder override, as each
// has its own complete root.
type Category struct {
	ID ID
	// Name is unique and matches [a-z0-9._-]{1,64}, except "." and "..".
	Name string
	// UsenetDir and TorrentDir, when set, are the absolute directories the
	// Ready output of this Category's Usenet and torrent Downloads lands in.
	// Empty means <complete root of the protocol>/<Name>.
	UsenetDir  string
	TorrentDir string
}

// DirOverride returns the folder override for Downloads of the source kind,
// or "" for the default folder.
func (c Category) DirOverride(kind SourceKind) string {
	if kind == SourceTorrent {
		return c.TorrentDir
	}
	return c.UsenetDir
}

// ErrInvalidCategoryName is returned for a Category name outside
// [a-z0-9._-]{1,64}, or one that is "." or "..".
var ErrInvalidCategoryName = errors.New("category name must match [a-z0-9._-]{1,64}")

// Validate reports whether the Category may be stored.
func (c Category) Validate() error {
	if !validCategoryName(c.Name) {
		return fmt.Errorf("core: %q: %w", c.Name, ErrInvalidCategoryName)
	}
	for _, d := range []struct{ kind, dir string }{{"usenet", c.UsenetDir}, {"torrent", c.TorrentDir}} {
		if d.dir == "" {
			continue
		}
		if !filepath.IsAbs(d.dir) {
			return fmt.Errorf("core: category %q: %s dir override %q is not an absolute path", c.Name, d.kind, d.dir)
		}
		if filepath.Clean(d.dir) != d.dir {
			return fmt.Errorf("core: category %q: %s dir override %q is not a clean path (no \"..\", \".\" or doubled and trailing separators)", c.Name, d.kind, d.dir)
		}
	}
	return nil
}

func validCategoryName(name string) bool {
	// "." and ".." match the pattern but would name a directory outside
	// <complete root>/<Name>.
	if len(name) < 1 || len(name) > 64 || name == "." || name == ".." {
		return false
	}
	for i := range len(name) {
		switch b := name[i]; {
		case 'a' <= b && b <= 'z', '0' <= b && b <= '9', b == '.', b == '_', b == '-':
		default:
			return false
		}
	}
	return true
}
