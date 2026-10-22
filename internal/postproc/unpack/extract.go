package unpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// Result describes a finished Unpack.
type Result struct {
	// Files are the extracted files, relative to dst with '/' separators, sorted.
	Files []string
	// Bytes is the total size of the extracted files.
	Bytes int64
	// Nested is true when archives found in the output were unpacked too.
	Nested bool
	// Skipped counts entries that were not extracted: symlinks, hardlinks,
	// devices, FIFOs and other special entries.
	Skipped int
}

// unpackWith is Unpack without the archive format: it extracts the entries
// that open yields into a staging directory next to dst and renames the
// staging directory to dst on success. packed is the size of the archive on
// disk, for the compression ratio guard.
func unpackWith(ctx context.Context, dst string, open openFunc, packed int64, cfg *config) (Result, error) {
	return unpackLevel(ctx, dst, open, packed, cfg, true)
}

// unpackLevel is unpackWith; nested says whether archives found in the output
// are unpacked too (one level only).
func unpackLevel(ctx context.Context, dst string, open openFunc, packed int64, cfg *config, nested bool) (Result, error) {
	dst = filepath.Clean(dst)
	if _, err := os.Lstat(dst); err == nil && !cfg.replace {
		return Result{}, fmt.Errorf("%w: %s", ErrExists, dst)
	}
	stage := dst + ".tmp"
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return Result{}, err
	}
	// A leftover from a crashed run is stale by definition.
	if err := os.RemoveAll(stage); err != nil {
		return Result{}, err
	}
	if err := os.Mkdir(stage, 0o777); err != nil {
		return Result{}, err
	}
	res, err := extractAll(ctx, stage, open, packed, cfg)
	if err == nil && nested {
		err = unpackNested(ctx, stage, cfg, &res)
	}
	if err == nil {
		err = publish(stage, dst, cfg.replace)
	}
	if err != nil {
		_ = os.RemoveAll(stage)
		return Result{}, err
	}
	return res, nil
}

// publish moves the finished staging directory into place.
func publish(stage, dst string, replace bool) error {
	if _, err := os.Lstat(dst); err == nil {
		if !replace {
			return fmt.Errorf("%w: %s", ErrExists, dst)
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	return os.Rename(stage, dst)
}

// guard applies the per-entry rules. One guard sees one pass over an archive.
type guard struct {
	cfg      *config
	count    int
	declared int64
	seen     map[string]bool // lower-cased path -> is a directory
}

func newGuard(cfg *config) *guard {
	return &guard{cfg: cfg, seen: map[string]bool{}}
}

// admit checks an entry. ok is false for entries that are skipped (special
// types and the archive root); rel is the sanitized relative path otherwise.
func (g *guard) admit(e *entry) (rel string, ok bool, err error) {
	g.count++
	if g.count > g.cfg.maxEntries {
		return "", false, fmt.Errorf("%w: more than %d", ErrTooManyEntries, g.cfg.maxEntries)
	}
	rel, err = cleanName(e.name)
	if err != nil {
		return "", false, err
	}
	switch e.kind {
	case kindFile, kindDir:
	default:
		return "", false, nil
	}
	if rel == "" {
		if e.kind == kindDir {
			return "", false, nil
		}
		return "", false, fmt.Errorf("%w: file named after the root %q", ErrPathViolation, truncateForError(e.name))
	}
	if err := g.claim(rel, e.kind == kindDir); err != nil {
		return "", false, err
	}
	if e.kind == kindFile && e.size > 0 {
		g.declared = addSat(g.declared, e.size)
	}
	return rel, true, nil
}

// claim records rel and its parents, refusing a name that collides with an
// earlier one once case is ignored.
func (g *guard) claim(rel string, isDir bool) error {
	for i := 0; i < len(rel); i++ {
		if rel[i] != '/' {
			continue
		}
		parent := strings.ToLower(rel[:i])
		if dir, seen := g.seen[parent]; seen && !dir {
			return fmt.Errorf("%w: %q is both a file and a directory", ErrPathViolation, truncateForError(rel))
		}
		g.seen[parent] = true
	}
	key := strings.ToLower(rel)
	dir, seen := g.seen[key]
	switch {
	case !seen:
		g.seen[key] = isDir
	case dir && isDir:
	default:
		return fmt.Errorf("%w: duplicate entry %q", ErrPathViolation, truncateForError(rel))
	}
	return nil
}

func addSat(a, b int64) int64 {
	if a > 1<<62-b {
		return 1 << 62
	}
	return a + b
}

type extractor struct {
	cfg     *config
	stage   string
	packed  int64
	buf     []byte
	written int64 // across all files, skipped drains included
	res     Result
}

// extractAll runs the two passes: validate everything, then write.
func extractAll(ctx context.Context, stage string, open openFunc, packed int64, cfg *config) (Result, error) {
	if err := scan(ctx, stage, open, packed, cfg); err != nil {
		return Result{}, err
	}
	x := &extractor{cfg: cfg, stage: stage, packed: packed, buf: make([]byte, copyChunk)}
	if err := x.run(ctx, open); err != nil {
		return Result{}, err
	}
	sort.Strings(x.res.Files)
	return x.res, nil
}

// scan is the first pass. It applies every guard to every entry and checks the
// declared sizes, so a hostile archive fails before a single byte is written.
func scan(ctx context.Context, stage string, open openFunc, packed int64, cfg *config) error {
	src, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = src.close() }()
	g := newGuard(cfg)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, err := src.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if _, _, err := g.admit(e); err != nil {
			return err
		}
	}
	if cfg.maxBytes > 0 && g.declared > cfg.maxBytes {
		return fmt.Errorf("%w: archive declares %d bytes, cap is %d", ErrTooLarge, g.declared, cfg.maxBytes)
	}
	if g.declared > cfg.bombMin && g.declared/max(packed, 1) > cfg.bombRatio {
		return fmt.Errorf("%w: archive of %d bytes declares %d", ErrBomb, packed, g.declared)
	}
	return checkDisk(stage, g.declared, cfg)
}

func checkDisk(stage string, need int64, cfg *config) error {
	free, err := cfg.freeSpace(filepath.Dir(stage))
	if err != nil {
		cfg.logger.Warn("unpack: cannot determine free disk space, skipping the disk check", "err", err)
		return nil
	}
	if free < cfg.minFree || uint64(need) > free-cfg.minFree { //nolint:gosec // need is non-negative
		return fmt.Errorf("%w: need %d bytes, %d free, %d reserved", ErrDiskFull, need, free, cfg.minFree)
	}
	return nil
}

func (x *extractor) run(ctx context.Context, open openFunc) error {
	src, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = src.close() }()
	g := newGuard(x.cfg)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, err := src.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, ok, err := g.admit(e)
		if err != nil {
			return err
		}
		if !ok {
			if err := x.skip(ctx, e); err != nil {
				return err
			}
			continue
		}
		if err := x.write(ctx, e, rel); err != nil {
			return err
		}
	}
}

// skip handles an entry that is not extracted. Its content is still read when
// the archive format needs that to stay in step, as in solid RAR archives.
func (x *extractor) skip(ctx context.Context, e *entry) error {
	if e.kind != kindDir {
		x.res.Skipped++
		x.cfg.logger.Info("unpack: skipped archive entry", "kind", e.kind.String(), "name", truncateForError(e.name))
	}
	if e.open == nil {
		return nil
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	_, err = x.copy(ctx, io.Discard, r, e)
	return err
}

func (x *extractor) write(ctx context.Context, e *entry, rel string) error {
	target, err := fsutil.SafeJoin(x.stage, rel)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPathViolation, err)
	}
	if e.kind == kindDir {
		return diskErr(os.MkdirAll(target, 0o777))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		return diskErr(err)
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return diskErr(err)
	}
	n, err := x.copy(ctx, f, r, e)
	if cerr := f.Close(); err == nil {
		err = diskErr(cerr)
	}
	if err != nil {
		return err
	}
	x.res.Bytes += n
	x.res.Files = append(x.res.Files, rel)
	return nil
}

// diskErr maps "no space left" to ErrDiskFull.
func diskErr(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%w: %v", ErrDiskFull, err)
	}
	return err
}

// copy streams r to w in chunks, enforcing the per-file, total and ratio
// limits and watching the context.
func (x *extractor) copy(ctx context.Context, w io.Writer, r io.Reader, e *entry) (int64, error) {
	var n int64
	allow := int64(-1)
	if e.size >= 0 {
		allow = e.size + e.size/100 + 1024
	}
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		k, rerr := r.Read(x.buf)
		if k > 0 {
			if allow >= 0 && n+int64(k) > allow {
				return n, fmt.Errorf("%w: entry %q is larger than its declared %d bytes", ErrCorrupt, truncateForError(e.name), e.size)
			}
			x.written += int64(k)
			if x.cfg.maxBytes > 0 && x.written > x.cfg.maxBytes {
				return n, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, x.cfg.maxBytes)
			}
			if x.written > x.cfg.bombMin && x.written/max(x.packed, 1) > x.cfg.bombRatio {
				return n, fmt.Errorf("%w: %d bytes from an archive of %d", ErrBomb, x.written, x.packed)
			}
			if _, err := w.Write(x.buf[:k]); err != nil {
				return n, diskErr(err)
			}
			n += int64(k)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return n, classifyRead(rerr)
		}
	}
	if e.size >= 0 && n != e.size {
		return n, fmt.Errorf("%w: entry %q is %d bytes, header says %d", ErrCorrupt, truncateForError(e.name), n, e.size)
	}
	return n, nil
}

// classifyRead keeps errors that are already classified and turns anything else
// a decoder reports into ErrCorrupt.
func classifyRead(err error) error {
	for _, known := range []error{ErrCorrupt, ErrPassword, ErrUnsupported, ErrLimit, ErrPathViolation, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%w: %v", ErrCorrupt, err)
}
