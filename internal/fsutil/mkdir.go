package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// MkdirAll is os.MkdirAll, except that a permission error names the folder
// bitnews could not write to and the user id it runs as. Directories are
// created at first use; whether a configured path is writable is not checked
// before.
func MkdirAll(path string, perm os.FileMode) error {
	return WriteError(os.MkdirAll(path, perm))
}

// WriteError relabels a permission error from a file operation: it names the
// folder that refused the write and the effective user id, so the user knows
// what to fix. The result still matches fs.ErrPermission and wraps err. Any
// other error, including nil, is returned unchanged.
func WriteError(err error) error {
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	var path string
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		path = pe.Path
	case errors.As(err, &le):
		path = le.New // a refused rename or link: the folder it was going into
	default:
		return err
	}
	return &writeError{folder: filepath.Dir(path), uid: os.Geteuid(), err: err}
}

type writeError struct {
	folder string
	uid    int
	err    error
}

func (e *writeError) Error() string {
	return fmt.Sprintf("cannot write to folder %q: permission denied for uid %d, give that user write access to the folder",
		e.folder, e.uid)
}

func (e *writeError) Unwrap() error { return e.err }

// FailureDetail is the text to show as the reason of a failed Download: it
// names only the folder and the user id, so it may keep its path.
func (e *writeError) FailureDetail() string { return e.Error() }
