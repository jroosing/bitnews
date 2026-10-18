package nzb

import "errors"

// Errors returned by Parse. Callers match them with errors.Is.
var (
	// ErrTooLarge means the input is longer than Limits.MaxBytes.
	ErrTooLarge = errors.New("nzb: input too large")

	// ErrTooManyFiles means the NZB has more <file> elements than
	// Limits.MaxFiles.
	ErrTooManyFiles = errors.New("nzb: too many files")

	// ErrTooManySegments means the NZB has more <segment> elements in total
	// than Limits.MaxSegments.
	ErrTooManySegments = errors.New("nzb: too many segments")

	// ErrNoFiles means the NZB contains no file with at least one valid
	// segment.
	ErrNoFiles = errors.New("nzb: no usable files")

	// ErrMalformed means the input is not a well-formed NZB document: bad
	// XML, an unsupported encoding, or a root element other than <nzb>. It
	// wraps the underlying encoding/xml error when there is one.
	ErrMalformed = errors.New("nzb: malformed document")
)
