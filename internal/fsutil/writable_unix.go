//go:build unix

package fsutil

import "syscall"

// writable asks the kernel whether the process may write into dir and enter
// it (access(2) with W_OK|X_OK).
func writable(dir string) bool {
	const wOK, xOK = 0x2, 0x1
	return syscall.Access(dir, wOK|xOK) == nil
}
