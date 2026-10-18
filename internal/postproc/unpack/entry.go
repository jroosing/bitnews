package unpack

import "io"

// entryKind is the type of an archive entry. Only files and directories are
// ever extracted.
type entryKind int

const (
	kindFile entryKind = iota
	kindDir
	kindSymlink
	kindHardlink
	kindDevice
	kindFIFO
	kindOther
)

func (k entryKind) String() string {
	switch k {
	case kindFile:
		return "file"
	case kindDir:
		return "directory"
	case kindSymlink:
		return "symlink"
	case kindHardlink:
		return "hardlink"
	case kindDevice:
		return "device"
	case kindFIFO:
		return "fifo"
	}
	return "other"
}

// entry is one archive member as the extractor sees it. Everything about it is
// hostile until the guards have looked at it.
type entry struct {
	name string
	kind entryKind
	// size is the declared uncompressed size, negative when unknown.
	size int64
	// open returns the content. It is only called for files (and for skipped
	// entries whose content must still be read, as in solid archives), and the
	// reader is only valid until the next call to source.next.
	open func() (io.Reader, error)
}

// source iterates over the entries of one archive.
type source interface {
	// next returns the next entry, or io.EOF after the last one. The errors it
	// returns are already classified (ErrCorrupt, ErrPassword, ...).
	next() (*entry, error)
	close() error
}

// openFunc opens the archive for one pass over its entries.
type openFunc func() (source, error)
