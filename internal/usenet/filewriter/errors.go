package filewriter

import "errors"

var (
	// ErrSizeMismatch is returned when a part announces a file size that
	// differs from the one recorded for its key.
	ErrSizeMismatch = errors.New("filewriter: file size differs between parts")
	// ErrInvalidPart is returned for a part whose offsets, size or data
	// length are inconsistent.
	ErrInvalidPart = errors.New("filewriter: invalid part")
	// ErrTooLarge is returned for a part announcing an absurd file size.
	ErrTooLarge = errors.New("filewriter: file too large")
	// ErrUnknownFile is returned for a key that has no file yet.
	ErrUnknownFile = errors.New("filewriter: unknown file key")
	// ErrClosed is returned by operations on a closed Writer.
	ErrClosed = errors.New("filewriter: writer closed")
	// ErrInvalidSnapshot is returned by Restore for a malformed snapshot.
	ErrInvalidSnapshot = errors.New("filewriter: invalid snapshot")
	// ErrNotEmpty is returned by Restore when the Writer already has files.
	ErrNotEmpty = errors.New("filewriter: writer already has files")
)

// MaxFileSize is the largest file size a part may announce (1 TiB).
const MaxFileSize int64 = 1 << 40
