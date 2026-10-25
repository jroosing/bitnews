//go:build !unix

package fsutil

// writable cannot be asked without access(2): assume yes and let the write
// itself report a problem.
func writable(string) bool { return true }
