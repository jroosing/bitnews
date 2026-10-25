package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// SafeJoin joins an untrusted relative path onto root and guarantees the
// result stays inside root.
//
// rel is cleaned lexically; backslashes count as separators on every
// platform. Absolute paths, volume names, NUL bytes and any ".." that climbs
// above root are rejected with ErrUnsafePath. An empty rel or "." yields root.
//
// If root exists, the deepest existing ancestor of the result is also
// resolved with EvalSymlinks and must lie inside the resolved root; this
// catches symlinked parent directories that point outside. Dangling or
// looping symlinks on the way are rejected too, since writing through them
// could create files anywhere. Components that do not exist yet are allowed.
// If root does not exist, nothing below it can be a symlink and only the
// lexical check applies.
//
// The returned path is built from root as given (cleaned), not the resolved
// path. The check is not atomic: a concurrent process able to create symlinks
// under root can still win a race between SafeJoin and the use of the path.
func SafeJoin(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: empty root", ErrUnsafePath)
	}
	if strings.IndexByte(root, 0) >= 0 || strings.IndexByte(rel, 0) >= 0 {
		return "", fmt.Errorf("%w: NUL byte", ErrUnsafePath)
	}

	// Backslashes are separators regardless of platform: names from archives
	// and torrents may come from Windows tools.
	rel = filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/"))
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || strings.HasPrefix(rel, string(filepath.Separator)) {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafePath, rel)
	}
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the root", ErrUnsafePath, rel)
	}
	joined := filepath.Join(root, rel)

	realRoot, err := filepath.EvalSymlinks(root)
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing exists below a missing root, so there are no symlinks to follow.
		return joined, nil
	}
	if err != nil {
		return "", fmt.Errorf("fsutil: resolve root: %w", err)
	}

	anc, err := deepestExisting(joined)
	if err != nil {
		return "", err
	}
	realAnc, err := filepath.EvalSymlinks(anc)
	if err != nil {
		// A dangling or looping symlink: writing through it could land anywhere.
		return "", fmt.Errorf("%w: cannot resolve %q: %v", ErrUnsafePath, anc, err)
	}
	if !Within(realRoot, realAnc) {
		return "", fmt.Errorf("%w: %q resolves outside the root", ErrUnsafePath, anc)
	}
	return joined, nil
}

// deepestExisting returns the longest prefix of path that exists, judged with
// Lstat so that a dangling symlink counts as existing.
func deepestExisting(path string) (string, error) {
	for {
		_, err := os.Lstat(path)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", fmt.Errorf("fsutil: %w", err)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path, nil
		}
		path = parent
	}
}
