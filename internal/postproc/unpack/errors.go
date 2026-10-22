package unpack

import (
	"errors"
	"fmt"
)

// Errors returned by Detect and Unpack. Test with errors.Is; the concrete
// error usually carries more detail. The orchestrator maps them to Download
// failure reasons: ErrPassword to archive_password, ErrUnsupported to
// archive_unsupported, ErrCorrupt to archive_corrupt, ErrPathViolation to
// path_violation.
var (
	// ErrMissingVolume marks a set whose volumes are not all present.
	ErrMissingVolume = errors.New("unpack: missing archive volume")
	// ErrPassword means the archive is encrypted and the password is missing or
	// wrong.
	ErrPassword = errors.New("unpack: password missing or wrong")
	// ErrUnsupported means the archive uses a feature the pure-Go decoders lack.
	ErrUnsupported = errors.New("unpack: unsupported archive feature")
	// ErrCorrupt means the archive is damaged, truncated or lies about itself.
	ErrCorrupt = errors.New("unpack: archive corrupt")
	// ErrPathViolation means an entry name tried to escape the output
	// directory or is otherwise unusable.
	ErrPathViolation = errors.New("unpack: unsafe path in archive")

	// ErrLimit is the parent of the resource limits below, for callers that
	// treat them alike.
	ErrLimit = errors.New("unpack: resource limit exceeded")
	// ErrDiskFull means the output does not fit in the free disk space.
	ErrDiskFull = fmt.Errorf("%w: not enough free disk space", ErrLimit)
	// ErrBomb means the archive expands far beyond what its size justifies.
	ErrBomb = fmt.Errorf("%w: compression ratio too high", ErrLimit)
	// ErrTooLarge means the output exceeds the configured byte cap.
	ErrTooLarge = fmt.Errorf("%w: output too large", ErrLimit)
	// ErrTooManyEntries means the archive has more entries than allowed.
	ErrTooManyEntries = fmt.Errorf("%w: too many entries", ErrLimit)

	// ErrExists means the destination already exists and Replace was not set.
	ErrExists = errors.New("unpack: destination exists")
)
