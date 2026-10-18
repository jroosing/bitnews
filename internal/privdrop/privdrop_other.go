//go:build !linux

package privdrop

import (
	"errors"
	"os"
	"syscall"
)

// errUnsupported is returned by every id-changing call: dropping privileges
// is only implemented for Linux.
var errUnsupported = errors.New("privilege drop is only supported on Linux")

// OS returns the real system calls. Here only Geteuid, Getegid and Umask
// work; changing ids fails, so PUID/PGID are an error unless they already
// match the current process.
func OS() Syscalls { return otherSyscalls{} }

type otherSyscalls struct{}

func (otherSyscalls) Geteuid() int          { return os.Geteuid() }
func (otherSyscalls) Getegid() int          { return os.Getegid() }
func (otherSyscalls) Umask(mask int) int    { return syscall.Umask(mask) }
func (otherSyscalls) Setgroups([]int) error { return errUnsupported }
func (otherSyscalls) Setgid(int) error      { return errUnsupported }
func (otherSyscalls) Setuid(int) error      { return errUnsupported }
