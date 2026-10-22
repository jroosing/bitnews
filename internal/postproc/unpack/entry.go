package unpack

// entryKind is the type of an archive entry. Only files and directories are
// ever extracted; every other kind fails the Unpack.
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
	return "special entry"
}

// entry is one archive member as the 7-Zip listing describes it. Everything
// about it is hostile until the guards have looked at it.
type entry struct {
	name string
	kind entryKind
	// size is the declared uncompressed size, zero for directories.
	size int64
	// encrypted says the content (or the name) is encrypted.
	encrypted bool
}
