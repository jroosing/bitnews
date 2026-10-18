package fsutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// maxUniqueTries bounds the counter search; after that a random suffix is used.
const maxUniqueTries = 10000

// Unique returns path if nothing exists there, otherwise the first free
// variant with a counter: "file.mkv" becomes "file.1.mkv", "file.2.mkv", ...
// The counter goes before the last extension of a file and at the end for
// directories ("dir" becomes "dir.1") and for names without an extension
// (including dotfiles such as ".hidden").
//
// Existence is checked with Lstat, so a dangling symlink counts as taken.
// Any Lstat error other than "not exist" also counts as taken. The result is
// shortened on rune boundaries so the final name stays within 255 bytes.
//
// The check is not atomic: a caller that creates the result must still open
// it exclusively (O_EXCL) or use a non-clobbering rename.
func Unique(path string) string {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path
	}
	dir, name := filepath.Split(path)
	base, ext := name, ""
	if err != nil || !fi.IsDir() {
		base, ext = splitExt(name)
	}
	for n := 1; n <= maxUniqueTries; n++ {
		if c := dir + withSuffix(base, "."+strconv.Itoa(n), ext); !exists(c) {
			return c
		}
	}
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return dir + withSuffix(base, "."+hex.EncodeToString(buf[:]), ext)
}

// withSuffix builds base+suffix+ext, shrinking base to respect the byte cap.
func withSuffix(base, suffix, ext string) string {
	room := maxNameBytes - len(suffix) - len(ext)
	return truncateRunes(base, room) + suffix + ext
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
