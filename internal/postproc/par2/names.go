package par2

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// ErrUnsafeSet is returned, before par2 is started, for a PAR2 set that
// cannot be handed to par2: a file name that could escape the Download's
// directory or is not a plain relative path, or a file that is built to make
// the name reader work without end. par2 writes files under the names the set
// gives it, and the set is attacker controlled.
var ErrUnsafeSet = errors.New("par2: unsafe PAR2 set")

// Limits of the name reader and of the name check.
const (
	packetHeaderLen = 64
	// fileDescFixed is the part of a File Description body before the name.
	fileDescFixed = 56
	// maxDescLen is the longest File Description packet the reader will hash.
	// A candidate packet that claims more is refused (ErrUnsafeSet) instead
	// of skipped: par2 might accept it.
	maxDescLen = 1 << 20
	// MaxNameLen is the longest file name (path) a set may carry.
	MaxNameLen = 1024
	// maxComponentLen is the longest single path component (a file system
	// limit on every platform we run on).
	maxComponentLen = 255
	// MaxNames bounds how many distinct names ReadNames collects.
	MaxNames = 100_000

	scanChunk = 256 << 10
)

var (
	packetMagic  = []byte("PAR2\x00PKT")
	typeFileDesc = "PAR 2.0\x00FileDesc"
)

// ReadNames returns the distinct file names that the File Description
// packets of one .par2 stream (r, size bytes) carry, trailing NULs removed.
//
// It is the minimal packet reader behind the name check, and it must accept at
// least every packet par2 would accept. So it does not follow packet
// boundaries: it looks for the "PAR2\0PKT" magic at every byte offset, inside
// other packets, recovery data and garbage included, and takes every
// candidate whose type is File Description and whose MD5 verifies. Anything
// par2 finds is a subset of that. Names are returned exactly as written
// (possibly hostile); run them through CheckNames.
//
// Memory is bounded (a chunk and one packet of at most 1 MiB), and so is the
// hashing done for candidates that fail verification: when it exceeds a small
// multiple of size the stream is refused with ErrUnsafeSet, as is a stream
// with a candidate packet longer than 1 MiB or with more than MaxNames names.
// An I/O error from r is returned as it is.
func ReadNames(r io.ReaderAt, size int64) ([]string, error) {
	rd := &nameReader{r: r, size: size, budget: 4*size + 1<<20, seen: map[string]struct{}{}}
	if err := rd.run(); err != nil {
		return nil, err
	}
	return rd.names, nil
}

type nameReader struct {
	r      io.ReaderAt
	size   int64
	budget int64 // bytes of failed hashing still allowed
	seen   map[string]struct{}
	names  []string
	pkt    []byte
}

func (n *nameReader) run() error {
	buf := make([]byte, scanChunk+len(packetMagic)-1)
	var pos int64
	for pos < n.size {
		want := int(min(int64(len(buf)), n.size-pos))
		if err := n.readFull(buf[:want], pos); err != nil {
			return err
		}
		window := buf[:want]
		base := 0
		for {
			i := bytes.Index(window[base:], packetMagic)
			if i < 0 {
				break
			}
			at := base + i
			// A magic in the last len(magic)-1 bytes of a full chunk may be
			// cut; the next chunk starts early enough to see it whole.
			if err := n.candidate(pos + int64(at)); err != nil {
				return err
			}
			base = at + 1
		}
		if pos+int64(want) >= n.size {
			break
		}
		pos += int64(want) - int64(len(packetMagic)) + 1
	}
	return nil
}

func (n *nameReader) readFull(p []byte, off int64) error {
	got, err := n.r.ReadAt(p, off)
	if got == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// candidate looks at the bytes at off, where the magic was found.
func (n *nameReader) candidate(off int64) error {
	if n.size-off < packetHeaderLen {
		return nil
	}
	var hdr [packetHeaderLen]byte
	if err := n.readFull(hdr[:], off); err != nil {
		return err
	}
	if string(hdr[48:64]) != typeFileDesc {
		return nil
	}
	length := binary.LittleEndian.Uint64(hdr[8:16])
	if length%4 != 0 || length < packetHeaderLen+fileDescFixed || length > uint64(n.size-off) {
		return nil // par2 refuses a packet like this too
	}
	if length > maxDescLen {
		return fmt.Errorf("%w: a file description packet claims %d bytes", ErrUnsafeSet, length)
	}
	if cap(n.pkt) < int(length) {
		n.pkt = make([]byte, length)
	}
	pkt := n.pkt[:length]
	if err := n.readFull(pkt, off); err != nil {
		return err
	}
	if sum := md5.Sum(pkt[32:]); !bytes.Equal(sum[:], pkt[16:32]) {
		n.budget -= int64(length) + 4096
		if n.budget < 0 {
			return fmt.Errorf("%w: too many damaged file description packets", ErrUnsafeSet)
		}
		return nil
	}
	name := string(bytes.TrimRight(pkt[packetHeaderLen+fileDescFixed:], "\x00"))
	if _, dup := n.seen[name]; dup {
		return nil
	}
	if len(n.names) >= MaxNames {
		return fmt.Errorf("%w: more than %d file names", ErrUnsafeSet, MaxNames)
	}
	n.seen[name] = struct{}{}
	n.names = append(n.names, name)
	return nil
}

// ReadNamesFile is ReadNames for a file on disk.
func ReadNamesFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return ReadNames(f, fi.Size())
}

// CheckName reports whether name, taken from a PAR2 file, is a plain relative
// path that is safe to give to par2: slash separated, no empty, "." or ".."
// component, not absolute, no drive letter, no backslash, no NUL or other
// control character, not a PAR2 file itself, and within the length limits.
// Control characters are refused because par2 prints names, and a name with a
// line break could fake lines of the output we parse.
func CheckName(name string) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: file name %q: %s", ErrUnsafeSet, truncate(name, 80), why)
	}
	switch {
	case name == "":
		return bad("empty")
	case len(name) > MaxNameLen:
		return bad("too long")
	case strings.HasPrefix(name, "/"):
		return bad("absolute path")
	case len(name) >= 2 && name[1] == ':' && isLetter(name[0]):
		return bad("drive letter")
	}
	for i := range len(name) {
		switch c := name[i]; {
		case c == 0:
			return bad("NUL byte")
		case c == '\\':
			return bad("backslash")
		case c < 0x20 || c == 0x7f:
			return bad("control character")
		}
	}
	for comp := range strings.SplitSeq(name, "/") {
		switch {
		case comp == "":
			return bad("empty path component")
		case comp == "." || comp == "..":
			return bad("relative path component")
		case len(comp) > maxComponentLen:
			return bad("path component too long")
		}
	}
	if strings.HasSuffix(strings.ToLower(name), ".par2") {
		return bad("a PAR2 file cannot be a target")
	}
	return nil
}

// CheckNames runs CheckName on every name and then fsutil.SafeJoin on dir
// and the name, which also refuses a name whose parent directory in dir is a
// symlink that leaves dir. A target that already exists in dir as a symlink
// or as anything but a regular file is refused too: par2 would read it or
// replace it.
func CheckNames(dir string, names []string) error {
	for _, name := range names {
		if err := CheckName(name); err != nil {
			return err
		}
		p, err := fsutil.SafeJoin(dir, name)
		if err != nil {
			return fmt.Errorf("%w: file name %q: %w", ErrUnsafeSet, truncate(name, 80), err)
		}
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return fmt.Errorf("par2: %w", err)
		case !fi.Mode().IsRegular():
			return fmt.Errorf("%w: target %q exists and is not a regular file", ErrUnsafeSet, truncate(name, 80))
		}
	}
	return nil
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// truncate shortens s to at most n bytes for an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
