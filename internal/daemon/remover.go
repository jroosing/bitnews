package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// fetchHolder stops the fetch of a Download and keeps it stopped.
type fetchHolder interface {
	Hold(ctx context.Context, id core.ID) error
	Release(id core.ID)
}

// dataRemover implements app.DataRemover.
type dataRemover struct {
	settings   func() core.Settings
	categories core.CategoryRepo
	holder     fetchHolder
	dirFor     func(core.Download) string
	log        *slog.Logger
}

// RemoveData deletes the working directory and the output of d. Only NZB
// Downloads have data to remove yet; torrents come with M5. The running
// fetch is stopped and held first, so it cannot write into a directory that
// is being deleted. When the removal fails the hold is released again: the
// Download stays and goes on.
func (r *dataRemover) RemoveData(ctx context.Context, d core.Download) error {
	if d.SourceKind != core.SourceNZB {
		return nil
	}
	if err := r.holder.Hold(ctx, d.ID); err != nil {
		return err
	}
	roots, err := r.roots(ctx)
	// The working directory is trusted when it is <parent>/<id>: it was made
	// for this Download at add time, even if the incomplete root has been
	// changed since.
	work := r.dirFor(d)
	if err == nil && filepath.Base(work) == string(d.ID) {
		roots = append(roots, filepath.Dir(work))
	}
	if err == nil {
		err = errors.Join(
			r.removeInside(roots, work, d.ID),
			r.removeInside(roots, d.OutputPath, d.ID),
		)
	}
	if err != nil {
		r.holder.Release(d.ID)
		return err
	}
	return nil
}

// roots returns the directories data may be deleted inside: the Usenet
// incomplete and complete roots, and the directory override of every
// Category (Ready output of such a Category lands there).
func (r *dataRemover) roots(ctx context.Context) ([]string, error) {
	s := r.settings()
	roots := []string{s.Paths.Usenet.Incomplete, s.Paths.Usenet.Complete}
	cats, err := r.categories.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("daemon: list categories: %w", err)
	}
	for _, c := range cats {
		if c.DirOverride != "" {
			roots = append(roots, c.DirOverride)
		}
	}
	return roots, nil
}

// removeInside deletes target if it lies strictly inside one of the roots.
// A target outside every root, or a root itself, is left alone with a
// warning: the setting it came from may have changed since. A target whose
// path climbs out of its root through a symlink is an error. Symlinks are
// never followed: a symlink target is unlinked, and deleting a directory
// removes symlinks inside it without entering them. A target that is gone
// already is fine.
func (r *dataRemover) removeInside(roots []string, target string, id core.ID) error {
	if target == "" {
		return nil
	}
	target = filepath.Clean(target)
	if !filepath.IsAbs(target) {
		r.log.Warn("not deleting a relative path", "id", id)
		return nil
	}
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		// Check the parent chain only: the last element may itself be a
		// symlink, which is unlinked, not followed.
		parent, err := fsutil.SafeJoin(root, filepath.Dir(rel))
		if err != nil {
			return fmt.Errorf("daemon: refusing to delete %q: %w", target, err)
		}
		return removePath(filepath.Join(parent, filepath.Base(rel)))
	}
	r.log.Warn("not deleting data outside the configured directories", "id", id)
	return nil
}

// removePath deletes path: a symlink is unlinked, a directory is removed with
// its content (os.RemoveAll does not follow links), a missing path is fine.
func removePath(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("daemon: delete data: %w", err)
	case fi.Mode()&fs.ModeSymlink != 0:
		err = os.Remove(path)
	default:
		err = os.RemoveAll(path)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon: delete data: %w", err)
	}
	return nil
}
