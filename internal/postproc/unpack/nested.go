package unpack

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// unpackNested unpacks the archive sets found in the extracted output, one
// level deep: a RAR inside a RAR is unpacked, a RAR inside that is left alone.
// Each inner archive is extracted next to where it lay, under the same guards,
// and its volumes are deleted afterwards. Everything stays inside stage.
func unpackNested(ctx context.Context, stage string, cfg *config, res *Result) error {
	var dirs []string
	err := filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(dirs)
	n := 0
	for _, dir := range dirs {
		sets, err := Detect(dir)
		if err != nil {
			return err
		}
		for _, set := range sets {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := unpackInner(ctx, stage, dir, set, cfg, n, res); err != nil {
				return err
			}
			n++
		}
	}
	if n == 0 {
		return nil
	}
	res.Nested = true
	return recount(stage, res)
}

// unpackInner extracts one inner set into a scratch directory below stage and
// moves the result into dir. A name that is already taken is made unique, so
// the inner archive can never overwrite what the outer one produced.
func unpackInner(ctx context.Context, stage, dir string, set ArchiveSet, cfg *config, n int, res *Result) error {
	if set.Err != nil {
		return fmt.Errorf("nested archive %q: %w", set.Name, set.Err)
	}
	inner := *cfg
	if cfg.maxBytes > 0 {
		// The byte cap covers the outer and the inner archives together.
		var used int64
		_ = filepath.WalkDir(stage, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if fi, ierr := d.Info(); ierr == nil {
					used += fi.Size()
				}
			}
			return nil
		})
		inner.maxBytes = max(cfg.maxBytes-used, 1)
	}
	open, packed, err := openerFor(set, &inner)
	if err != nil {
		return err
	}
	scratch := filepath.Join(stage, fmt.Sprintf(".nested-%d", n))
	r, err := unpackLevel(ctx, scratch, open, packed, &inner, false)
	if err != nil {
		return fmt.Errorf("nested archive %q: %w", set.Name, err)
	}
	res.Skipped += r.Skipped
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return err
	}
	for _, e := range entries {
		target := fsutil.Unique(filepath.Join(dir, e.Name()))
		if err := os.Rename(filepath.Join(scratch, e.Name()), target); err != nil {
			return err
		}
	}
	if err := os.Remove(scratch); err != nil {
		return err
	}
	for _, v := range set.Volumes {
		if err := os.Remove(v); err != nil {
			return err
		}
	}
	return nil
}

// recount rebuilds the file list and byte total from what is in stage.
func recount(stage string, res *Result) error {
	res.Files = res.Files[:0]
	res.Bytes = 0
	err := filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return err
		}
		res.Files = append(res.Files, filepath.ToSlash(rel))
		res.Bytes += fi.Size()
		return nil
	})
	sort.Strings(res.Files)
	return err
}
