//go:build !unix

package daemon

// statfs is unavailable here: the adapter then reports no disk space.
func statfs(string) (free, total int64, ok bool) { return 0, 0, false }
