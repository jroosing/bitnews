//go:build !unix

package fsutil

import "fmt"

// DiskSpace is not implemented on this platform: the space is unknown.
func DiskSpace(path string) (free, total uint64, err error) {
	return 0, 0, fmt.Errorf("fsutil: free space of %s: %w", path, ErrUnsupported)
}
