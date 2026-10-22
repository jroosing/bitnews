package unpack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// Result describes a finished Unpack.
type Result struct {
	// Files are the extracted files, relative to dst with '/' separators, sorted.
	Files []string
	// Bytes is the total size of the extracted files.
	Bytes int64
	// Nested is true when archives found in the output were unpacked too.
	Nested bool
}

// unpackLevel unpacks one set; nested says whether archives found in the
// output are unpacked too (one level only).
func unpackLevel(ctx context.Context, z *sevenZip, set ArchiveSet, dst string, cfg *config, nested bool) (Result, error) {
	dst, err := filepath.Abs(dst)
	if err != nil {
		return Result{}, err
	}
	if _, err := os.Lstat(dst); err == nil && !cfg.replace {
		return Result{}, fmt.Errorf("%w: %s", ErrExists, dst)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	packed, err := volumeSize(set.Volumes)
	if err != nil {
		return Result{}, err
	}
	first, err := filepath.Abs(set.Volumes[0])
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return Result{}, diskErr(err)
	}

	// Pass 1: list the archive and apply every guard before a byte is written.
	entries, err := z.list(ctx, filepath.Dir(first), first)
	if err != nil {
		return Result{}, err
	}
	g := newGuard(cfg)
	for _, e := range entries {
		if err := g.admit(e); err != nil {
			return Result{}, err
		}
		if e.encrypted && cfg.password == "" {
			return Result{}, fmt.Errorf("%w: the archive is encrypted and no password is set", ErrPassword)
		}
	}
	if cfg.maxBytes > 0 && g.declared > cfg.maxBytes {
		return Result{}, fmt.Errorf("%w: archive declares %d bytes, cap is %d", ErrTooLarge, g.declared, cfg.maxBytes)
	}
	if g.declared > cfg.bombMin && g.declared/max(packed, 1) > cfg.bombRatio {
		return Result{}, fmt.Errorf("%w: archive of %d bytes declares %d", ErrBomb, packed, g.declared)
	}
	stage := dst + ".tmp"
	if err := checkDisk(stage, g.declared, cfg); err != nil {
		return Result{}, err
	}

	// Pass 2: extract into the staging directory and check what came out.
	// A leftover from a crashed run is stale by definition.
	if err := removeTree(stage); err != nil {
		return Result{}, err
	}
	if err := os.Mkdir(stage, 0o777); err != nil {
		return Result{}, diskErr(err)
	}
	res, err := extractStage(ctx, z, first, stage, g, cfg)
	if err == nil && nested {
		err = unpackNested(ctx, z, stage, cfg, &res)
	}
	if err == nil {
		err = publish(stage, dst, cfg.replace)
	}
	if err != nil {
		_ = removeTree(stage)
		return Result{}, err
	}
	return res, nil
}

// extractStage runs 7zz into stage and walks the result.
func extractStage(ctx context.Context, z *sevenZip, first, stage string, g *guard, cfg *config) (Result, error) {
	if err := z.extract(ctx, stage, first); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	w := &walker{maxEntries: cfg.maxEntries}
	if err := w.run(ctx, stage); err != nil {
		return Result{}, err
	}
	if len(w.files) > g.files || w.bytes > g.declared {
		return Result{}, fmt.Errorf("%w: 7zz wrote %d files and %d bytes, the listing declared %d and %d",
			ErrCorrupt, len(w.files), w.bytes, g.files, g.declared)
	}
	if cfg.maxBytes > 0 && w.bytes > cfg.maxBytes {
		return Result{}, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, cfg.maxBytes)
	}
	sort.Strings(w.files)
	return Result{Files: w.files, Bytes: w.bytes}, nil
}

// publish moves the finished staging directory into place.
func publish(stage, dst string, replace bool) error {
	if _, err := os.Lstat(dst); err == nil {
		if !replace {
			return fmt.Errorf("%w: %s", ErrExists, dst)
		}
		if err := removeTree(dst); err != nil {
			return err
		}
	}
	return diskErr(os.Rename(stage, dst))
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

// diskErr maps "no space left" to ErrDiskFull.
func diskErr(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%w: %v", ErrDiskFull, err)
	}
	return err
}

// removeTree is os.RemoveAll that also copes with what an archive can leave
// behind: directories without write or search permission.
func removeTree(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}
