package fsutil

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// Dir is one folder ListDirs found.
type Dir struct {
	// Name is the folder's own name and Path its full path.
	Name, Path string
	// Writable says this process may create files in it.
	Writable bool
}

// ListDirs returns the folders directly inside dir, for the folder picker of
// the settings pages, sorted by name ignoring case. It lists folders only:
// files, symlinks (never followed, even to a folder) and hidden entries
// (a leading dot) are left out. At most limit folders are returned;
// truncated says there were more.
func ListDirs(dir string, limit int) (dirs []Dir, truncated bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		dirs = append(dirs, Dir{Name: e.Name(), Path: p, Writable: writable(p)})
	}
	slices.SortFunc(dirs, func(a, b Dir) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	if limit > 0 && len(dirs) > limit {
		return dirs[:limit], true, nil
	}
	return dirs, false, nil
}

// Writable reports whether this process may create files in dir.
func Writable(dir string) bool { return writable(dir) }

// NearestDir is the folder the picker opens at for a typed path: the path
// itself when it is an existing folder, else its nearest existing parent. A
// path that is empty, relative or holds control characters gives "/".
func NearestDir(p string) string {
	if p == "" || !filepath.IsAbs(p) || strings.ContainsFunc(p, unicode.IsControl) {
		return string(filepath.Separator)
	}
	for p = filepath.Clean(p); ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
		if parent := filepath.Dir(p); parent == p {
			return p
		}
	}
}
