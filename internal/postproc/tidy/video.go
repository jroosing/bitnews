package tidy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jroosing/bitnews/internal/fsutil"
)

// videoExts are the extensions media managers import as video.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".ts": true, ".m2ts": true, ".mts": true,
	".mov": true, ".wmv": true, ".mpg": true, ".mpeg": true, ".webm": true, ".vob": true, ".divx": true,
	".flv": true, ".iso": true, ".img": true,
}

// NameVideo is SABnzbd's deobfuscation of final file names: when the largest
// file below dir has no video extension but its content is a video, it is
// renamed to name (the Download's name, made safe) plus the extension the
// content says. Fully obfuscated posts without a usable PAR2 set otherwise
// leave the movie under a random name that media managers do not import. It
// walks subfolders but never follows links, never overwrites (a taken name
// gets a counter) and leaves everything else alone. ok says whether it
// renamed; the Rename's paths are relative to dir.
func NameVideo(dir, name string) (r Rename, ok bool, err error) {
	var largest string
	var size int64 = -1
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
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
		if fi.Size() > size {
			largest, size = p, fi.Size()
		}
		return nil
	})
	if err != nil {
		return Rename{}, false, fmt.Errorf("tidy: scan %s: %w", dir, err)
	}
	if largest == "" || videoExts[strings.ToLower(filepath.Ext(largest))] {
		return Rename{}, false, nil
	}
	ext, err := sniffVideo(largest)
	if ext == "" || err != nil {
		return Rename{}, false, err
	}
	safe, err := fsutil.SafeName(name + ext)
	if err != nil {
		return Rename{}, false, nil
	}
	to := fsutil.Unique(filepath.Join(filepath.Dir(largest), safe))
	if err := os.Rename(largest, to); err != nil {
		return Rename{}, false, fmt.Errorf("tidy: rename %s: %w", filepath.Base(largest), err)
	}
	from, _ := filepath.Rel(dir, largest)
	rel, _ := filepath.Rel(dir, to)
	return Rename{From: filepath.ToSlash(from), To: filepath.ToSlash(rel)}, true, nil
}

// sniffVideo reads the start of the file at p and returns the extension of
// the video container it holds, or "".
func sniffVideo(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("tidy: %w", err)
	}
	defer f.Close()
	head := make([]byte, 400)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("tidy: read %s: %w", filepath.Base(p), err)
	}
	return VideoExt(head[:n]), nil
}

// VideoExt returns the extension of the video container the bytes start
// with: Matroska, MP4 or QuickTime, AVI, ASF (WMV), MPEG program stream,
// MPEG transport stream or M2TS; "" for anything else.
func VideoExt(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return ".mkv"
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		if string(head[8:12]) == "qt  " {
			return ".mov"
		}
		return ".mp4"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "AVI ":
		return ".avi"
	case bytes.HasPrefix(head, []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11}):
		return ".wmv"
	case bytes.HasPrefix(head, []byte{0x00, 0x00, 0x01, 0xba}):
		return ".mpg"
	case syncEvery(head, 0, 188):
		return ".ts"
	case syncEvery(head, 4, 192):
		return ".m2ts"
	}
	return ""
}

// syncEvery reports whether head has the transport stream sync byte at off
// and at the next two packets of size n.
func syncEvery(head []byte, off, n int) bool {
	for i := range 3 {
		if p := off + i*n; p >= len(head) || head[p] != 0x47 {
			return false
		}
	}
	return true
}
