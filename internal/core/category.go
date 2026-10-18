package core

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Category is an optional label on a Download that decides where its Ready
// output lands and which Media manager owns it. Categories are shared across
// Usenet and torrents.
type Category struct {
	ID ID
	// Name is unique and matches [a-z0-9._-]{1,64}, except "." and "..".
	Name string
	// DirOverride, when set, is the absolute directory Ready output of this
	// Category lands in. Empty means <complete root>/<Name>.
	DirOverride string
}

// ErrInvalidCategoryName is returned for a Category name outside
// [a-z0-9._-]{1,64}, or one that is "." or "..".
var ErrInvalidCategoryName = errors.New("category name must match [a-z0-9._-]{1,64}")

// Validate reports whether the Category may be stored.
func (c Category) Validate() error {
	if !validCategoryName(c.Name) {
		return fmt.Errorf("core: %q: %w", c.Name, ErrInvalidCategoryName)
	}
	if c.DirOverride != "" && !filepath.IsAbs(c.DirOverride) {
		return fmt.Errorf("core: category %q: dir override %q is not an absolute path", c.Name, c.DirOverride)
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
