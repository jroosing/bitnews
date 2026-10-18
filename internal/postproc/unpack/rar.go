package unpack

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/nwaples/rardecode/v2"
)

// openRAR returns the opener for a RAR set. The volumes are opened through a
// file system that only knows the set's own volumes, so the decoder cannot
// wander off to a file Detect did not classify (or to a symlink).
func openRAR(set ArchiveSet, cfg *config) openFunc {
	return func() (source, error) {
		allowed := make(map[string]bool, len(set.Volumes))
		for _, v := range set.Volumes {
			allowed[filepath.Clean(v)] = true
		}
		opts := []rardecode.Option{
			rardecode.FileSystem(volumeFS{allowed: allowed}),
			rardecode.MaxDictionarySize(cfg.maxDict),
		}
		if cfg.password != "" {
			opts = append(opts, rardecode.Password(cfg.password))
		}
		var rc *rardecode.ReadCloser
		err := guarded(func() (err error) {
			rc, err = rardecode.OpenReader(set.Volumes[0], opts...)
			return err
		})
		if err != nil {
			return nil, mapRARError(err, nil, cfg)
		}
		return &rarSource{rc: rc, cfg: cfg}, nil
	}
}

// volumeFS serves the volumes of one set and nothing else.
type volumeFS struct{ allowed map[string]bool }

func (v volumeFS) Open(name string) (fs.File, error) {
	if !v.allowed[filepath.Clean(name)] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	f, _, err := openVolume(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

type rarSource struct {
	rc  *rardecode.ReadCloser
	cfg *config
}

func (s *rarSource) close() error { return s.rc.Close() }

func (s *rarSource) next() (*entry, error) {
	var h *rardecode.FileHeader
	err := guarded(func() (err error) {
		h, err = s.rc.Next()
		return err
	})
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, mapRARError(err, nil, s.cfg)
	}
	e := &entry{name: h.Name, kind: kindRAR(h), size: h.UnPackedSize}
	if h.UnKnownSize || h.UnPackedSize < 0 {
		e.size = -1
	}
	if e.kind != kindDir {
		e.open = func() (io.Reader, error) { return &rarReader{r: s.rc, h: h, cfg: s.cfg}, nil }
	}
	return e, nil
}

// kindRAR classifies a RAR entry from its link records and Unix mode bits.
func kindRAR(h *rardecode.FileHeader) entryKind {
	switch h.LinkType {
	case rardecode.LinkTypeNone:
	case rardecode.LinkTypeHardLink, rardecode.LinkTypeFileCopy:
		return kindHardlink
	default: // Unix and Windows symlinks, junctions, and anything newer
		return kindSymlink
	}
	if h.HostOS == rardecode.HostOSUnix {
		switch h.Attributes & 0xF000 {
		case 0xA000:
			return kindSymlink
		case 0x1000:
			return kindFIFO
		case 0x2000, 0x6000:
			return kindDevice
		case 0xC000:
			return kindOther
		}
	}
	if h.Mode()&fs.ModeSymlink != 0 {
		return kindSymlink
	}
	if h.IsDir {
		return kindDir
	}
	return kindFile
}

type rarReader struct {
	r   *rardecode.ReadCloser
	h   *rardecode.FileHeader
	cfg *config
}

func (r *rarReader) Read(p []byte) (int, error) {
	var n int
	err := guarded(func() (err error) {
		n, err = r.r.Read(p)
		return err
	})
	if err != nil && !errors.Is(err, io.EOF) {
		err = mapRARError(err, r.h, r.cfg)
	}
	return n, err
}

// Errors that say the data decoded to garbage. For an encrypted file that is
// what a wrong password looks like, as old RAR versions have no password check.
var rarDecodeErrors = []error{
	rardecode.ErrBadFileChecksum, rardecode.ErrShortFile, rardecode.ErrDecoderOutOfData,
	rardecode.ErrHuffDecodeFailed, rardecode.ErrInvalidLengthTable, rardecode.ErrCorruptDecodeHeader,
	rardecode.ErrCorruptPPM, rardecode.ErrInvalidFilter, rardecode.ErrTooManyFilters,
	rardecode.ErrInvalidVMInstruction,
}

var rarPasswordErrors = []error{
	rardecode.ErrArchiveEncrypted, rardecode.ErrArchivedFileEncrypted, rardecode.ErrBadPassword,
}

var rarUnsupportedErrors = []error{
	rardecode.ErrUnknownEncryptMethod, rardecode.ErrUnknownVersion, rardecode.ErrUnsupportedDecoder,
	rardecode.ErrUnknownDecoder, rardecode.ErrMultipleDecoders, rardecode.ErrUnknownFilter,
	rardecode.ErrDictionaryTooLarge, rardecode.ErrSolidOpen, rardecode.ErrPlatformIntSize,
}

func isAny(err error, set []error) bool {
	for _, e := range set {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// mapRARError classifies an error from rardecode. h is the file being read, if
// any.
func mapRARError(err error, h *rardecode.FileHeader, cfg *config) error {
	switch {
	case errors.Is(err, ErrCorrupt), errors.Is(err, ErrPassword), errors.Is(err, ErrUnsupported):
		return err
	case isAny(err, rarPasswordErrors):
		return fmt.Errorf("%w: %v", ErrPassword, err)
	case isAny(err, rarUnsupportedErrors):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	case h != nil && h.Encrypted && cfg.password != "" && isAny(err, rarDecodeErrors):
		return fmt.Errorf("%w: %v", ErrPassword, err)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %w: %v", ErrCorrupt, ErrMissingVolume, err)
	}
	return fmt.Errorf("%w: %v", ErrCorrupt, err)
}
