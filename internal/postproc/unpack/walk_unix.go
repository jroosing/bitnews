//go:build unix

package unpack

import (
	"io/fs"
	"syscall"
)

// linkCount is the number of hard links to the file.
func linkCount(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:gosec // a link count is never negative
	}
	return 1
}
