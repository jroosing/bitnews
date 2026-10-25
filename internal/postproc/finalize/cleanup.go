package finalize

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jroosing/bitnews/internal/fsutil"
)

// tmpDirName is the staging dir Unpack may leave behind inside the
// Download's dir.
const tmpDirName = "_unpack.tmp"

var (
	// .rar, old-style .r00/.s00 volumes and 7z (single or split) volumes.
	archiveRE = regexp.MustCompile(`(?i)\.(rar|[rs]\d{2,}|7z(\.\d{3,})?)$`)
	garbageRE = regexp.MustCompile(`(?i)\.(par2|nzb)$`)
)

// cleanUsenet deletes known garbage from dir and, if archives were
// unpacked, merges unpackedDir into dir's root. It only ever deletes
// regular files directly inside dir and the _unpack.tmp dir, never follows
// symlinks and is idempotent.
func cleanUsenet(ctx context.Context, dir, unpackedDir string) error {
	unpacked := unpackedDir != ""
	if unpacked {
		unpackedDir = filepath.Clean(unpackedDir)
		if err := checkUnpackedDir(dir, unpackedDir); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("finalize: read %q: %w", dir, err)
	}

	// A missing unpackedDir is only fine on a retry, when everything was
	// already merged and the archives are gone. Archives still being there
	// means nothing was unpacked: refuse to delete them.
	merged := false
	if unpacked {
		if _, err := os.Lstat(unpackedDir); errors.Is(err, fs.ErrNotExist) {
			merged = true
			for _, e := range entries {
				if e.Type().IsRegular() && archiveRE.MatchString(e.Name()) {
					return fmt.Errorf("finalize: unpacked dir %q is missing but archives remain in %q", unpackedDir, dir)
				}
			}
		}
	}

	for _, e := range entries {
		name := e.Name()
		switch {
		case e.Type().IsRegular() && (garbageRE.MatchString(name) || unpacked && archiveRE.MatchString(name)):
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("finalize: %w", err)
			}
		case unpacked && name == tmpDirName && e.Type().IsDir():
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := removeTree(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}

	if unpacked && !merged {
		return mergeUnpacked(ctx, dir, unpackedDir)
	}
	return nil
}

// checkUnpackedDir makes sure unpackedDir is a real dir that is either
// inside dir (but not in the _unpack.tmp leftover) or entirely separate.
func checkUnpackedDir(dir, unpackedDir string) error {
	switch {
	case unpackedDir == dir || fsutil.Inside(unpackedDir, dir):
		return fmt.Errorf("finalize: unpacked dir %q overlaps download dir %q", unpackedDir, dir)
	case fsutil.Inside(dir, unpackedDir):
		rel, _ := filepath.Rel(dir, unpackedDir)
		if first, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); first == tmpDirName {
			return fmt.Errorf("finalize: unpacked dir %q is inside %s", unpackedDir, tmpDirName)
		}
	}
	fi, err := os.Lstat(unpackedDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("finalize: %w", err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: unpacked dir %q", fsutil.ErrSymlink, unpackedDir)
	case !fi.IsDir():
		return fmt.Errorf("finalize: unpacked dir %q is not a directory", unpackedDir)
	}
	return nil
}

// mergeUnpacked moves the entries of unpackedDir to the root of dir. An
// entry whose name is taken keeps the existing file and gets a Unique name.
func mergeUnpacked(ctx context.Context, dir, unpackedDir string) error {
	entries, err := os.ReadDir(unpackedDir)
	if err != nil {
		return fmt.Errorf("finalize: read %q: %w", unpackedDir, err)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := filepath.Join(unpackedDir, e.Name())
		dst := fsutil.Unique(filepath.Join(dir, e.Name()))
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("finalize: merge unpacked output: %w", err)
		}
	}
	if err := os.Remove(unpackedDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("finalize: remove %q: %w", unpackedDir, err)
	}
	return nil
}

// removeTree removes a real directory and everything in it without
// following symlinks (os.RemoveAll unlinks them), making dirs writable first.
func removeTree(root string) error {
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	if !fi.IsDir() {
		return nil
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	return nil
}
