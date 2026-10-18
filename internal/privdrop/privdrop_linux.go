//go:build linux

package privdrop

import (
	"os"
	"syscall"
)

// OS returns the real system calls. On Linux, Go applies Setgroups, Setgid
// and Setuid to every thread of the process (since Go 1.16).
func OS() Syscalls { return linuxSyscalls{} }

type linuxSyscalls struct{}

func (linuxSyscalls) Geteuid() int               { return os.Geteuid() }
func (linuxSyscalls) Getegid() int               { return os.Getegid() }
func (linuxSyscalls) Umask(mask int) int         { return syscall.Umask(mask) }
func (linuxSyscalls) Setgroups(gids []int) error { return syscall.Setgroups(gids) }
func (linuxSyscalls) Setgid(gid int) error       { return syscall.Setgid(gid) }
func (linuxSyscalls) Setuid(uid int) error       { return syscall.Setuid(uid) }
