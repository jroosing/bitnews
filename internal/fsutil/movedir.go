package fsutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// rename is os.Rename; tests replace it to simulate EXDEV.
var rename = os.Rename

// MoveResult describes how MoveDir moved a directory.
type MoveResult struct {
	// CrossDevice is true when the source and destination are on different
	// filesystems, so the tree was copied instead of renamed. Hardlinks into
	// or out of the moved tree no longer share data with the new copy, so
	// callers should warn that hardlinking will not work for this layout.
	CrossDevice bool
}

// MoveDir moves the directory src to the new path dst and creates any missing
// parent directories of dst.
//
// dst must not exist (ErrExists) and must not lie inside src (ErrNested); src
// must be a real directory, not a symlink to one (ErrSymlink). The existence
// check is not atomic with the move.
//
// On the same filesystem MoveDir is a single rename, which keeps inodes (and
// so hardlinks) intact. When rename fails with EXDEV the tree is copied
// instead:
//
//   - Symlinks anywhere in src are never followed and are refused with
//     ErrSymlink. FIFOs, sockets and devices are refused with ErrSpecialFile.
//     Post-processing is expected to have removed them already.
//   - Regular files are copied with their permission bits (setuid, setgid and
//     sticky are dropped) and modification time. Hardlinked files inside src
//     are copied as independent files.
//   - Every file and directory is fsynced before src is removed. Directory
//     fsync failures that only mean "unsupported" (some network filesystems)
//     are ignored.
//   - If the copy fails for any reason, the partial dst is removed and src is
//     left untouched. Missing parents of dst created by MoveDir are kept.
//   - If the copy succeeds but src cannot be fully removed, dst is kept
//     (it is complete), src may be partially deleted, and both a result with
//     CrossDevice set and a non-nil error are returned.
//
// Symlinks inside src are only refused on the copy path; a same-filesystem
// rename moves them as they are.
func MoveDir(src, dst string) (MoveResult, error) {
	fi, err := os.Lstat(src)
	if err != nil {
		return MoveResult{}, fmt.Errorf("fsutil: move %q: %w", src, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return MoveResult{}, fmt.Errorf("%w: source %q is a symlink", ErrSymlink, src)
	}
	if !fi.IsDir() {
		return MoveResult{}, fmt.Errorf("fsutil: move %q: not a directory", src)
	}

	if _, err := os.Lstat(dst); err == nil {
		return MoveResult{}, fmt.Errorf("%w: %q", ErrExists, dst)
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return MoveResult{}, fmt.Errorf("fsutil: move to %q: %w", dst, err)
	}
	if nested, err := isNested(src, dst); err != nil {
		return MoveResult{}, err
	} else if nested {
		return MoveResult{}, fmt.Errorf("%w: %q in %q", ErrNested, dst, src)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return MoveResult{}, fmt.Errorf("fsutil: create parent of %q: %w", dst, err)
	}

	err = rename(src, dst)
	if err == nil {
		return MoveResult{}, nil
	}
	if !isCrossDevice(err) {
		return MoveResult{}, fmt.Errorf("fsutil: move %q to %q: %w", src, dst, err)
	}

	res := MoveResult{CrossDevice: true}
	if err := copyTree(src, dst, fi); err != nil {
		return res, err
	}
	if err := removeAllWritable(src); err != nil {
		return res, fmt.Errorf("fsutil: copied %q to %q but could not remove the source: %w", src, dst, err)
	}
	return res, nil
}

func isCrossDevice(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	// ERROR_NOT_SAME_DEVICE
	return runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(0x11))
}

// isNested reports whether dst resolves to a location inside src.
func isNested(src, dst string) (bool, error) {
	realSrc, err := resolveLoosely(src)
	if err != nil {
		return false, err
	}
	realDst, err := resolveLoosely(dst)
	if err != nil {
		return false, err
	}
	return Within(realSrc, realDst), nil
}

// resolveLoosely makes path absolute and resolves symlinks in its existing
// part, keeping a non-existing tail as is.
func resolveLoosely(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("fsutil: %w", err)
	}
	anc, err := deepestExisting(abs)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(anc)
	if err != nil {
		return "", fmt.Errorf("fsutil: resolve %q: %w", anc, err)
	}
	tail, err := filepath.Rel(anc, abs)
	if err != nil {
		return "", fmt.Errorf("fsutil: %w", err)
	}
	return filepath.Join(real, tail), nil
}

// copyTree copies the directory src (described by info) to the new path dst.
// On failure it removes whatever it created.
func copyTree(src, dst string, info fs.FileInfo) (err error) {
	if err := os.Mkdir(dst, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %q", ErrExists, dst)
		}
		return fmt.Errorf("fsutil: create %q: %w", dst, err)
	}
	defer func() {
		if err != nil {
			_ = removeAllWritable(dst)
		}
	}()
	if err := copyDirContents(src, dst); err != nil {
		return err
	}
	if err := finishDir(dst, info); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// copyDirContents copies the entries of src into the existing directory dst.
func copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("fsutil: read %q: %w", src, err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		info, err := os.Lstat(s)
		if err != nil {
			return fmt.Errorf("fsutil: %w", err)
		}
		switch mode := info.Mode(); {
		case mode&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %q", ErrSymlink, s)
		case mode.IsDir():
			if err := os.Mkdir(d, 0o700); err != nil {
				return fmt.Errorf("fsutil: create %q: %w", d, err)
			}
			if err := copyDirContents(s, d); err != nil {
				return err
			}
			if err := finishDir(d, info); err != nil {
				return err
			}
		case mode.IsRegular():
			if err := copyFile(s, d, info); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %q (%v)", ErrSpecialFile, s, mode.Type())
		}
	}
	return nil
}

// finishDir fsyncs a fully copied directory and then applies the source's
// permissions and modification time. Permissions come last because a
// read-only source directory could not be filled otherwise.
func finishDir(dst string, info fs.FileInfo) error {
	if err := syncDir(dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("fsutil: %w", err)
	}
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return fmt.Errorf("fsutil: %w", err)
	}
	return nil
}

// copyFile copies the regular file src (described by info from Lstat) to the
// new file dst and fsyncs it.
func copyFile(src, dst string, info fs.FileInfo) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("fsutil: %w", err)
	}
	defer in.Close()
	// Guard against the entry having been swapped for a symlink since Lstat.
	opened, err := in.Stat()
	if err != nil {
		return fmt.Errorf("fsutil: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("%w: %q changed while moving", ErrSymlink, src)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("fsutil: %w", err)
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("fsutil: close %q: %w", dst, cerr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("fsutil: copy %q: %w", src, err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("fsutil: fsync %q: %w", dst, err)
	}
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("fsutil: %w", err)
	}
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return fmt.Errorf("fsutil: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so that its entries are durable. It is a no-op
// on Windows and ignores filesystems that do not support directory fsync.
func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("fsutil: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("fsutil: fsync %q: %w", path, err)
	}
	return nil
}

// removeAllWritable is os.RemoveAll for trees that may contain directories
// the process cannot write to: it makes every directory owner-accessible
// first. Symlinks are never followed.
func removeAllWritable(root string) error {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}
