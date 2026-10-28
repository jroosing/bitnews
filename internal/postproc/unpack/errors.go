package unpack

import (
	"errors"
	"fmt"

	"github.com/jroosing/bitnews/internal/core"
)

// Errors returned by Detect and Unpack. Test with errors.Is; the concrete
// error usually carries more detail. Each wraps the core sentinel the
// orchestrator maps to a Download failure reason, so they are returned to it
// as they are: ErrPassword is a core.ErrArchivePassword, ErrUnsupported a
// core.ErrArchiveUnsupported, ErrCorrupt and ErrMissingVolume a
// core.ErrArchiveCorrupt, ErrPathViolation a core.ErrPathViolation and
// ErrDiskFull a core.ErrDiskFull.
var (
	// ErrMissingVolume marks a set whose volumes are not all present.
	ErrMissingVolume = fmt.Errorf("unpack: missing archive volume: %w", core.ErrArchiveCorrupt)
	// ErrPassword means the archive is encrypted and the password is missing or
	// wrong.
	ErrPassword = fmt.Errorf("unpack: %w", core.ErrArchivePassword)
	// ErrUnsupported means the archive uses a compression method or feature 7zz does not support.
	ErrUnsupported = fmt.Errorf("unpack: unsupported archive feature: %w", core.ErrArchiveUnsupported)
	// ErrCorrupt means the archive is damaged, truncated or lies about itself.
	ErrCorrupt = fmt.Errorf("unpack: %w", core.ErrArchiveCorrupt)
	// ErrPathViolation means an entry name tried to escape the output
	// directory or is otherwise unusable.
	ErrPathViolation = fmt.Errorf("unpack: unsafe path in archive: %w", core.ErrPathViolation)

	// ErrLimit is the parent of the resource limits below, for callers that
	// treat them alike. A bomb or an oversized or over-full archive cannot be
	// unpacked within the limits, which the orchestrator treats as a damaged
	// archive; a full disk is core.ErrDiskFull.
	ErrLimit = errors.New("unpack: resource limit exceeded")
	// ErrDiskFull means the output does not fit in the free disk space.
	ErrDiskFull = fmt.Errorf("%w: not enough free disk space: %w", ErrLimit, core.ErrDiskFull)
	// ErrBomb means the archive expands far beyond what its size justifies.
	ErrBomb = fmt.Errorf("%w: compression ratio too high: %w", ErrLimit, core.ErrArchiveCorrupt)
	// ErrTooLarge means the output exceeds the configured byte cap.
	ErrTooLarge = fmt.Errorf("%w: output too large: %w", ErrLimit, core.ErrArchiveCorrupt)
	// ErrTooManyEntries means the archive has more entries than allowed.
	ErrTooManyEntries = fmt.Errorf("%w: too many entries: %w", ErrLimit, core.ErrArchiveCorrupt)

	// ErrExists means the destination already exists and Replace was not set.
	ErrExists = errors.New("unpack: destination exists")
)
