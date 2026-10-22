package unpack

import (
	"context"
	"fmt"
)

// Unpack extracts the archive set into dst. The files are first written to a
// staging directory next to dst (dst + ".tmp", cleared first, so an
// interrupted run is simply repeated) and the directory is renamed to dst once
// everything passed the guards. Any failure removes the staging directory and
// leaves dst as it was. It fails with ErrExists when dst exists, unless
// WithReplace is given.
//
// Errors are classified: ErrPassword, ErrUnsupported, ErrCorrupt,
// ErrPathViolation, and the resource limits under ErrLimit (ErrDiskFull,
// ErrBomb, ErrTooLarge, ErrTooManyEntries). Test with errors.Is.
func Unpack(ctx context.Context, set ArchiveSet, dst string, opts ...Option) (Result, error) {
	cfg := newConfig(opts)
	if set.Err != nil {
		return Result{}, set.Err
	}
	open, packed, err := openerFor(set, cfg)
	if err != nil {
		return Result{}, err
	}
	return unpackWith(ctx, dst, open, packed, cfg)
}

// openerFor picks the format reader for a set.
func openerFor(set ArchiveSet, cfg *config) (openFunc, int64, error) {
	if len(set.Volumes) == 0 {
		return nil, 0, fmt.Errorf("%w: archive set %q has no volumes", ErrCorrupt, set.Name)
	}
	packed, err := volumeSize(set.Volumes)
	if err != nil {
		return nil, 0, err
	}
	switch set.Format {
	case FormatRAR:
		return openRAR(set, cfg), packed, nil
	case Format7z:
		return open7z(set, cfg), packed, nil
	}
	return nil, 0, fmt.Errorf("%w: archive format %v", ErrUnsupported, set.Format)
}

// guarded runs a call into a decoder library and turns a panic into
// ErrCorrupt. The archives are hostile, and a decoder bug on a malformed
// archive (sevenzip v1.6.5 dereferences a nil on some headers) must fail the
// Download, not crash the process.
func guarded(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: decoder panic: %v", ErrCorrupt, r)
		}
	}()
	return fn()
}
