package adapt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/postproc/unpack"
)

// Unpacker implements core.Unpacker on top of the unpack package. The zero
// value is ready to use (no passwords, no free space reserve).
type Unpacker struct {
	// Passwords returns the archive password of a Download. Optional: nil
	// means no password. An empty Secret also means none. An error from it
	// is returned as it is.
	Passwords func(id core.ID) (core.Secret, error)
	// MinFree is the number of bytes to keep free on the disk; an unpack
	// whose declared output does not fit fails with core.ErrDiskFull.
	MinFree uint64
	// FreeSpace replaces the free disk space probe (statfs by default).
	FreeSpace func(path string) (uint64, error)
	// Options are passed to every unpack.Unpack call after the options
	// above, for example limits.
	Options []unpack.Option
}

var _ core.Unpacker = Unpacker{}

// Detect finds the archive sets in dir.
//
// A set that is not complete (a volume is missing, or a volume number is
// there twice) is a damaged download, whatever the cause: Detect fails with
// an error that wraps core.ErrArchiveCorrupt and the unpack package's
// error (unpack.ErrMissingVolume). The alternative, unpacking the sets that
// are complete and ignoring the broken one, would hand the user a partial
// result that looks like a success.
func (u Unpacker) Detect(ctx context.Context, dir string) ([]core.ArchiveSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sets, err := unpack.Detect(dir)
	if err != nil {
		return nil, fmt.Errorf("detect archives: %w", err)
	}
	out := make([]core.ArchiveSet, 0, len(sets))
	for _, s := range sets {
		if s.Err != nil {
			return nil, fmt.Errorf("%w: archive %q: %w", core.ErrArchiveCorrupt, s.Name, s.Err)
		}
		out = append(out, core.ArchiveSet{Name: s.Name, Opaque: s})
	}
	return out, nil
}

// Unpack extracts opts.Sets, in order, into dst. The archives are never
// modified.
//
// One set is unpacked straight into dst by unpack.Unpack, which stages it
// next to dst and renames it into place. Several sets are each unpacked
// into their own directory under dst+".sets" (cleared first), merged into
// one tree there and renamed to dst: where two sets have a directory of the
// same name the directories are merged, and where anything else collides
// the later entry gets a free name from fsutil.Unique ("a.mkv" becomes
// "a.1.mkv"), so nothing is overwritten. In both cases an existing dst is
// replaced, which is what makes a repeated run after a crash safe (with
// several sets the replacement is a removal followed by the rename).
//
// Errors are mapped to the core sentinels, see MapUnpackError.
func (u Unpacker) Unpack(ctx context.Context, _, dst string, opts core.UnpackOptions) (core.UnpackResult, error) {
	sets := make([]unpack.ArchiveSet, 0, len(opts.Sets))
	for _, s := range opts.Sets {
		us, ok := s.Opaque.(unpack.ArchiveSet)
		if !ok {
			return core.UnpackResult{}, fmt.Errorf("adapt: archive set %q has the Opaque payload %T, not one of this package's Detect", s.Name, s.Opaque)
		}
		sets = append(sets, us)
	}
	if len(sets) == 0 {
		return core.UnpackResult{}, errors.New("adapt: no archive sets to unpack")
	}
	uopts, err := u.options(opts.DownloadID)
	if err != nil {
		return core.UnpackResult{}, err
	}
	if len(sets) == 1 {
		res, err := unpack.Unpack(ctx, sets[0], dst, append(uopts, unpack.WithReplace())...)
		if err != nil {
			return core.UnpackResult{}, MapUnpackError(ctx, err)
		}
		return core.UnpackResult{Files: len(res.Files), Bytes: res.Bytes}, nil
	}
	return u.unpackMany(ctx, sets, dst, uopts)
}

// options builds the unpack options for a Download.
func (u Unpacker) options(id core.ID) ([]unpack.Option, error) {
	var o []unpack.Option
	if u.Passwords != nil {
		pw, err := u.Passwords(id)
		if err != nil {
			return nil, fmt.Errorf("archive password: %w", err)
		}
		if pw != "" {
			o = append(o, unpack.WithPassword(pw))
		}
	}
	if u.MinFree > 0 {
		o = append(o, unpack.WithMinFree(u.MinFree))
	}
	if u.FreeSpace != nil {
		o = append(o, unpack.WithFreeSpace(u.FreeSpace))
	}
	return append(o, u.Options...), nil
}

// unpackMany unpacks several sets and merges them into dst.
func (u Unpacker) unpackMany(ctx context.Context, sets []unpack.ArchiveSet, dst string, uopts []unpack.Option) (core.UnpackResult, error) {
	work := dst + ".sets"
	if err := os.RemoveAll(work); err != nil {
		return core.UnpackResult{}, fmt.Errorf("clear unpack work directory: %w", err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return core.UnpackResult{}, fmt.Errorf("create unpack work directory: %w", err)
	}
	defer os.RemoveAll(work) //nolint:errcheck // best effort; the next run clears it first

	var total core.UnpackResult
	parts := make([]string, len(sets))
	for i, s := range sets {
		parts[i] = filepath.Join(work, strconv.Itoa(i))
		res, err := unpack.Unpack(ctx, s, parts[i], uopts...)
		if err != nil {
			return core.UnpackResult{}, MapUnpackError(ctx, err)
		}
		total.Files += len(res.Files)
		total.Bytes += res.Bytes
	}

	merged := filepath.Join(work, "merged")
	if err := os.Mkdir(merged, 0o755); err != nil {
		return core.UnpackResult{}, fmt.Errorf("create merge directory: %w", err)
	}
	for _, p := range parts {
		if err := ctx.Err(); err != nil {
			return core.UnpackResult{}, err
		}
		if err := mergeInto(p, merged); err != nil {
			return core.UnpackResult{}, fmt.Errorf("merge unpacked archives: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return core.UnpackResult{}, err
	}
	if err := os.RemoveAll(dst); err != nil {
		return core.UnpackResult{}, fmt.Errorf("replace unpack destination: %w", err)
	}
	if err := os.Rename(merged, dst); err != nil {
		return core.UnpackResult{}, fmt.Errorf("move merged archives into place: %w", mapDiskErr(err))
	}
	return total, nil
}

// mergeInto moves the entries of the directory src into dst. Two directories
// of the same name are merged; any other collision gives the incoming entry
// a free name, so nothing is overwritten.
func mergeInto(src, dst string) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		existing, err := os.Lstat(to)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Free: move as it is.
		case err != nil:
			return err
		case existing.IsDir() && e.IsDir():
			if err := mergeInto(from, to); err != nil {
				return err
			}
			continue
		default:
			to = fsutil.Unique(to)
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	return nil
}

// MapUnpackError translates an error of the unpack package into the core
// sentinels, keeping the original in the chain for the detail:
//
//	unpack.ErrPassword                           core.ErrArchivePassword
//	unpack.ErrUnsupported                        core.ErrArchiveUnsupported
//	unpack.ErrCorrupt, unpack.ErrMissingVolume   core.ErrArchiveCorrupt
//	unpack.ErrPathViolation                      core.ErrPathViolation
//	unpack.ErrDiskFull, ENOSPC                   core.ErrDiskFull
//	unpack.ErrBomb, ErrTooLarge, ErrTooManyEntries
//	  (and any other unpack.ErrLimit)            core.ErrArchiveCorrupt
//
// A bomb or an oversized or over-full archive is treated as a damaged
// archive: the Download cannot be unpacked within the limits, and there is
// no sentinel for "too big". Context errors and every other error pass
// through (the latter wrapped, so the orchestrator takes it as internal).
func MapUnpackError(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case isContextErr(ctx, err):
		return err
	case errors.Is(err, unpack.ErrPassword):
		return fmt.Errorf("%w: %w", core.ErrArchivePassword, err)
	case errors.Is(err, unpack.ErrUnsupported):
		return fmt.Errorf("%w: %w", core.ErrArchiveUnsupported, err)
	case errors.Is(err, unpack.ErrPathViolation):
		return fmt.Errorf("%w: %w", core.ErrPathViolation, err)
	case errors.Is(err, unpack.ErrDiskFull), errors.Is(err, syscall.ENOSPC):
		return fmt.Errorf("%w: %w", core.ErrDiskFull, err)
	case errors.Is(err, unpack.ErrCorrupt), errors.Is(err, unpack.ErrMissingVolume), errors.Is(err, unpack.ErrLimit):
		return fmt.Errorf("%w: %w", core.ErrArchiveCorrupt, err)
	}
	return fmt.Errorf("unpack: %w", err)
}

// mapDiskErr turns a full disk into core.ErrDiskFull and leaves other errors
// alone.
func mapDiskErr(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%w: %w", core.ErrDiskFull, err)
	}
	return err
}
