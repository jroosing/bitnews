//go:build unix

package runner

import (
	"fmt"
	"syscall"
)

// defaultFreeSpace reports the bytes available to an unprivileged process on
// the filesystem holding path.
func defaultFreeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("runner: statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec // both are non-negative
}
