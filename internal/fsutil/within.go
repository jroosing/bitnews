package fsutil

import (
	"path/filepath"
	"strings"
)

// Within reports whether p is root or lies below it, lexically. It does not
// touch the filesystem or resolve symlinks, so callers that need real
// containment resolve first (SafeJoin does). Both paths are cleaned; a
// relative path paired with an absolute one is never within. "/a/b" is
// within "/a", "/ab" is not.
func Within(root, p string) bool {
	_, ok := relBelow(root, p)
	return ok
}

// Inside reports whether p lies strictly below root, lexically: like Within,
// but root itself is not inside root.
func Inside(root, p string) bool {
	rel, ok := relBelow(root, p)
	return ok && rel != "."
}

// relBelow returns p relative to root and whether it stays at or below root.
func relBelow(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
