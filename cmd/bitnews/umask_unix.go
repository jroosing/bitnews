//go:build unix

package main

import "syscall"

// setUmask sets the process-wide file mode creation mask.
func setUmask(mask int) { syscall.Umask(mask) }
