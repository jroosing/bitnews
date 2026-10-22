package unpack

import (
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"strings"

	"github.com/bodgit/sevenzip"
)

// open7z returns the opener for a 7z set (single file or .7z.001 volumes).
func open7z(set ArchiveSet, cfg *config) openFunc {
	return func() (source, error) {
		m, err := openMulti(set.Volumes)
		if err != nil {
			return nil, err
		}
		var r *sevenzip.Reader
		err = guarded(func() (err error) {
			r, err = sevenzip.NewReaderWithPassword(m, m.size, cfg.password)
			return err
		})
		if err != nil {
			_ = m.Close()
			return nil, map7zError(err, cfg)
		}
		return &sevenSource{m: m, r: r, cfg: cfg}, nil
	}
}

type sevenSource struct {
	m   *multiReaderAt
	r   *sevenzip.Reader
	cfg *config
	i   int
}

func (s *sevenSource) close() error { return s.m.Close() }

func (s *sevenSource) next() (*entry, error) {
	if s.i >= len(s.r.File) {
		return nil, io.EOF
	}
	f := s.r.File[s.i]
	s.i++
	e := &entry{name: f.Name, kind: kind7z(f.Mode()), size: -1}
	if f.UncompressedSize > 1<<62 {
		return nil, fmt.Errorf("%w: entry %q declares an absurd size", ErrCorrupt, truncateForError(f.Name))
	}
	if e.kind == kindFile {
		e.size = int64(f.UncompressedSize)
		e.open = func() (io.Reader, error) {
			var rc io.ReadCloser
			err := guarded(func() (err error) {
				rc, err = f.Open()
				return err
			})
			if err != nil {
				return nil, map7zError(err, s.cfg)
			}
			cr := &crcReader{rc: rc, f: f, h: crc32.NewIEEE(), cfg: s.cfg}
			return cr, nil
		}
	}
	return e, nil
}

func kind7z(m fs.FileMode) entryKind {
	switch {
	case m&fs.ModeSymlink != 0:
		return kindSymlink
	case m&fs.ModeDevice != 0:
		return kindDevice
	case m&fs.ModeNamedPipe != 0:
		return kindFIFO
	case m&(fs.ModeSocket|fs.ModeIrregular) != 0:
		return kindOther
	case m.IsDir():
		return kindDir
	}
	return kindFile
}

// crcReader checks the CRC32 of the 7z entry, which the library leaves to the
// caller, and maps its errors. It also closes the entry at the end.
type crcReader struct {
	rc   io.ReadCloser
	f    *sevenzip.File
	h    hash.Hash32
	cfg  *config
	done bool
}

func (c *crcReader) Read(p []byte) (int, error) {
	var n int
	err := guarded(func() (err error) {
		n, err = c.rc.Read(p)
		return err
	})
	_, _ = c.h.Write(p[:n])
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, io.EOF) {
		return n, map7zError(err, c.cfg)
	}
	if !c.done {
		c.done = true
		_ = c.rc.Close()
		if c.f.CRC32 != 0 && c.h.Sum32() != c.f.CRC32 {
			// An encrypted entry read with the wrong or no password decrypts
			// to garbage, which only shows as a checksum mismatch, and 7z
			// does not say whether an archive is encrypted. Data PAR2 has
			// verified makes a wrong password the likelier cause.
			return n, fmt.Errorf("%w: checksum mismatch in %q (wrong password, or corrupt data)", ErrPassword, truncateForError(c.f.Name))
		}
	}
	return n, io.EOF
}

// map7zError classifies an error from the sevenzip library.
func map7zError(err error, cfg *config) error {
	var re *sevenzip.ReadError
	switch {
	case errors.Is(err, ErrPassword), errors.Is(err, ErrCorrupt), errors.Is(err, ErrUnsupported):
		return err
	case errors.As(err, &re) && re.Encrypted:
		if cfg.password == "" {
			return fmt.Errorf("%w: archive is encrypted: %v", ErrPassword, err)
		}
		return fmt.Errorf("%w: %v", ErrPassword, err)
	case strings.Contains(err.Error(), "unsupported"):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return fmt.Errorf("%w: %v", ErrCorrupt, err)
}
