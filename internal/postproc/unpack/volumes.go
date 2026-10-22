package unpack

import (
	"fmt"
	"os"
)

// volumeSize is the total size of the volumes on disk. Detect only lists
// regular files, but the directory may have changed since: a volume swapped
// for a symlink or a special file is refused, because 7zz would follow it.
func volumeSize(paths []string) (int64, error) {
	var total int64
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return 0, err
		}
		if !fi.Mode().IsRegular() {
			return 0, fmt.Errorf("%w: volume %q is not a regular file", ErrCorrupt, p)
		}
		total = addSat(total, fi.Size())
	}
	return total, nil
}

func addSat(a, b int64) int64 {
	if a > 1<<62-b {
		return 1 << 62
	}
	return a + b
}
