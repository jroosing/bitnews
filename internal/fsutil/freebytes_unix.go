//go:build unix

package fsutil

import (
	"fmt"
	"syscall"
)

// DiskSpace reports the bytes available to an unprivileged process and the
// total bytes of the filesystem holding path.
func DiskSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("fsutil: statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize)                                        //nolint:gosec // non-negative
	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil //nolint:gosec // non-negative
}
