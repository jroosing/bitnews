package unpack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/fsutil"
)

// Unpacker implements core.Unpacker on top of Detect and Unpack, which run
// the 7zz program (ADR-0007). This package's errors wrap the core sentinels
// already; a full disk met by plain I/O becomes core.ErrDiskFull here. The
// zero value is ready to use (7zz is looked up on PATH, no passwords, no free
// space reserve).
type Unpacker struct {
	// SevenZip is the path of the 7zz program, as found by tool.Locate at
	// startup. Optional: empty means look 7zz up on PATH for each unpack. A
	// missing program is an ordinary error (not an archive error), wrapping
	// tool.ErrNotFound.
	SevenZip string
	// Passwords returns the archive password of a Download. Optional: nil
	// means no password. An empty Secret also means none. An error from it
	// is returned as it is.
	Passwords func(id core.ID) (core.Secret, error)
	// MinFree is the number of bytes to keep free on the disk; an unpack
	// whose declared output does not fit fails with core.ErrDiskFull.
	MinFree uint64
	// MinFreeFunc, when set, replaces MinFree: it is called for each unpack,
	// so the reserve can change while the Unpacker lives.
	MinFreeFunc func() uint64
	// FreeSpace replaces the free disk space probe (statfs by default).
	FreeSpace func(path string) (uint64, error)
	// Options are passed to every unpack.Unpack call after the options
	// above, for example limits.
	Options []Option
}

var _ core.Unpacker = Unpacker{}

// Detect reports whether dir holds archive sets.
//
// A set that is not complete (a volume is missing, or a volume number is
// there twice) is a damaged download, whatever the cause: Detect fails with
// an error that wraps core.ErrArchiveCorrupt and ErrMissingVolume. The
// alternative, unpacking the sets that are complete and ignoring the broken
// one, would hand the user a partial result that looks like a success.
func (u Unpacker) Detect(ctx context.Context, dir string) (bool, error) {
	sets, err := detectComplete(ctx, dir)
	return len(sets) > 0, err
}

// detectComplete finds the sets of dir and refuses an incomplete one.
func detectComplete(ctx context.Context, dir string) ([]ArchiveSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sets, err := Detect(dir)
	if err != nil {
		return nil, fmt.Errorf("detect archives: %w", err)
	}
	for _, s := range sets {
		if s.Err != nil {
			return nil, fmt.Errorf("%w: archive %q: %w", core.ErrArchiveCorrupt, s.Name, s.Err)
		}
	}
	return sets, nil
}

// Unpack extracts every archive set of dir, in order, into dst. The archives
// are never modified.
//
// One set is unpacked straight into dst by Unpack, which stages it next to
// dst and renames it into place. Several sets are each unpacked into their
// own directory under dst+".sets" (cleared first), merged into one tree there
// and renamed to dst: where two sets have a directory of the same name the
// directories are merged, and where anything else collides the later entry
// gets a free name from fsutil.Unique ("a.mkv" becomes "a.1.mkv"), so nothing
// is overwritten. In both cases an existing dst is replaced, which is what
// makes a repeated run after a crash safe (with several sets the replacement
// is a removal followed by the rename).
func (u Unpacker) Unpack(ctx context.Context, dir, dst string, opts core.UnpackOptions) (core.UnpackResult, error) {
	sets, err := detectComplete(ctx, dir)
	if err != nil {
		return core.UnpackResult{}, err
	}
	if len(sets) == 0 {
		return core.UnpackResult{}, fmt.Errorf("%w: no archive is left in the download", core.ErrArchiveCorrupt)
	}
	uopts, err := u.options(opts.DownloadID)
	if err != nil {
		return core.UnpackResult{}, err
	}
	if len(sets) == 1 {
		res, err := Unpack(ctx, sets[0], dst, append(uopts, WithReplace())...)
		if err != nil {
			return core.UnpackResult{}, mapDiskErr(err)
		}
		return core.UnpackResult{Files: len(res.Files), Bytes: res.Bytes}, nil
	}
	return u.unpackMany(ctx, sets, dst, uopts)
}

// options builds the unpack options for a Download.
func (u Unpacker) options(id core.ID) ([]Option, error) {
	var o []Option
	if u.SevenZip != "" {
		o = append(o, WithSevenZip(u.SevenZip))
	}
	if u.Passwords != nil {
		pw, err := u.Passwords(id)
		if err != nil {
			return nil, fmt.Errorf("archive password: %w", err)
		}
		if pw != "" {
			o = append(o, WithPassword(pw))
		}
	}
	minFree := u.MinFree
	if u.MinFreeFunc != nil {
		minFree = u.MinFreeFunc()
	}
	if minFree > 0 {
		o = append(o, WithMinFree(minFree))
	}
	if u.FreeSpace != nil {
		o = append(o, WithFreeSpace(u.FreeSpace))
	}
	return append(o, u.Options...), nil
}

// unpackMany unpacks several sets and merges them into dst.
func (u Unpacker) unpackMany(ctx context.Context, sets []ArchiveSet, dst string, uopts []Option) (core.UnpackResult, error) {
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
		res, err := Unpack(ctx, s, parts[i], uopts...)
		if err != nil {
			return core.UnpackResult{}, mapDiskErr(err)
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

// mapDiskErr turns a full disk met by plain I/O into core.ErrDiskFull and
// leaves other errors alone.
func mapDiskErr(err error) error {
	if errors.Is(err, syscall.ENOSPC) && !errors.Is(err, core.ErrDiskFull) {
		return fmt.Errorf("%w: %w", core.ErrDiskFull, err)
	}
	return err
}
