//go:build unix

package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// SameFilesystem reports whether a and b live on the same filesystem, by
// comparing the device numbers of their stat results. Symlinks are followed.
// A path that does not exist is represented by its deepest existing ancestor,
// so callers can ask about a destination they have not created yet.
func SameFilesystem(a, b string) (bool, error) {
	da, err := deviceOf(a)
	if err != nil {
		return false, err
	}
	db, err := deviceOf(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

func deviceOf(path string) (uint64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, fmt.Errorf("fsutil: %w", err)
	}
	for {
		fi, err := os.Stat(abs)
		if err == nil {
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, fmt.Errorf("fsutil: no device information for %q: %w", abs, ErrUnsupported)
			}
			return uint64(st.Dev), nil // Dev is int32 on darwin, uint64 on linux
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return 0, fmt.Errorf("fsutil: %w", err)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return 0, fmt.Errorf("fsutil: %w", err)
		}
		abs = parent
	}
}
