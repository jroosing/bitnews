package tidy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Rename is one entry DotUnderscores renamed.
type Rename struct{ From, To string }

// DotUnderscores replaces "_" with "." in the name of every entry (file or
// folder) directly inside dir, as replace_for.py does. It does not recurse
// and never overwrites: an entry whose new name is taken is left as it is.
// A link is renamed itself, never followed. It returns what it renamed; on a
// rename error it returns the renames done so far and the error.
func DotUnderscores(dir string) ([]Rename, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("tidy: read %s: %w", dir, err)
	}
	var done []Rename
	for _, e := range entries {
		from := e.Name()
		if !strings.Contains(from, "_") {
			continue
		}
		to := strings.ReplaceAll(from, "_", ".")
		src, dst := filepath.Join(dir, from), filepath.Join(dir, to)
		if _, err := os.Lstat(dst); err == nil {
			continue // taken: never overwrite
		}
		if err := os.Rename(src, dst); err != nil {
			return done, fmt.Errorf("tidy: rename %s: %w", from, err)
		}
		done = append(done, Rename{From: from, To: to})
	}
	return done, nil
}
