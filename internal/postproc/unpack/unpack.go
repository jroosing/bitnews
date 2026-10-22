package unpack

import (
	"context"
	"fmt"
)

// Unpack extracts the archive set into dst with 7zz. The archive is listed
// first and checked against the limits, then extracted into a staging
// directory next to dst (dst + ".tmp", cleared first, so an interrupted run is
// simply repeated), the result is checked again, and the directory is renamed
// to dst once everything passed. Any failure removes the staging directory and
// leaves dst as it was. It fails with ErrExists when dst exists, unless
// WithReplace is given.
//
// Errors are classified: ErrPassword, ErrUnsupported, ErrCorrupt,
// ErrMissingVolume, ErrPathViolation, and the resource limits under ErrLimit
// (ErrDiskFull, ErrBomb, ErrTooLarge, ErrTooManyEntries). Test with errors.Is.
// A missing 7zz program is an error wrapping tool.ErrNotFound. A cancelled
// context kills 7zz and returns the context's error.
func Unpack(ctx context.Context, set ArchiveSet, dst string, opts ...Option) (Result, error) {
	cfg := newConfig(opts)
	if set.Err != nil {
		return Result{}, set.Err
	}
	if len(set.Volumes) == 0 {
		return Result{}, fmt.Errorf("%w: archive set %q has no volumes", ErrCorrupt, set.Name)
	}
	z, err := locate(cfg)
	if err != nil {
		return Result{}, err
	}
	return unpackLevel(ctx, z, set, dst, cfg, true)
}
