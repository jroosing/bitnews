//go:build unix

package filewriter

import "syscall"

// oNoFollow makes open fail on a symlink in the final path component.
const oNoFollow = syscall.O_NOFOLLOW
