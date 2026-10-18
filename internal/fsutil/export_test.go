package fsutil

import "syscall"

// ForceCrossDevice makes MoveDir's rename fail with EXDEV, so the copy
// fallback runs on hosts that have only one filesystem. It returns a function
// restoring the real rename. Tests using it must not run in parallel.
func ForceCrossDevice() (restore func()) {
	return SetRename(func(_, _ string) error { return &linkError{err: syscall.EXDEV} })
}

// SetRename replaces the rename function used by MoveDir.
func SetRename(f func(oldpath, newpath string) error) (restore func()) {
	orig := rename
	rename = f
	return func() { rename = orig }
}

type linkError struct{ err error }

func (e *linkError) Error() string { return "rename: " + e.err.Error() }
func (e *linkError) Unwrap() error { return e.err }
