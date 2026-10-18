package fsutil

import "errors"

var (
	// ErrInvalidName is returned by SafeName for names that cannot be made safe.
	ErrInvalidName = errors.New("fsutil: invalid name")
	// ErrUnsafePath is returned by SafeJoin for paths that escape the root.
	ErrUnsafePath = errors.New("fsutil: unsafe path")
	// ErrExists is returned by MoveDir when the destination already exists.
	ErrExists = errors.New("fsutil: destination exists")
	// ErrSymlink is returned by MoveDir when a cross-device copy meets a symlink.
	ErrSymlink = errors.New("fsutil: symlink refused")
	// ErrSpecialFile is returned by MoveDir when a cross-device copy meets
	// anything that is not a regular file, directory or symlink.
	ErrSpecialFile = errors.New("fsutil: special file refused")
	// ErrNested is returned by MoveDir when dst lies inside src.
	ErrNested = errors.New("fsutil: destination inside source")
	// ErrUnsupported is returned on platforms where an operation is not implemented.
	ErrUnsupported = errors.ErrUnsupported
)
