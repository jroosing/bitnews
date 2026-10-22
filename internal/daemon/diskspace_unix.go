//go:build unix

package daemon

import "syscall"

// statfs reports the free and total bytes of the filesystem holding path.
func statfs(path string) (free, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), true //nolint:gosec // block counts fit
}
