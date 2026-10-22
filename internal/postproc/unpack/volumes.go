package unpack

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// openVolume opens one volume of a set. Detect only lists regular files, but
// the directory may have changed since: a volume swapped for a symlink or a
// special file is refused instead of followed.
func openVolume(path string) (*os.File, int64, error) {
	li, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if !li.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%w: volume %q is not a regular file", ErrCorrupt, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		_ = f.Close()
		return nil, 0, fmt.Errorf("%w: volume %q changed while opening", ErrCorrupt, path)
	}
	return f, fi.Size(), nil
}

// volumeSize is the total size of the volumes on disk.
func volumeSize(paths []string) (int64, error) {
	var total int64
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return 0, err
		}
		total = addSat(total, fi.Size())
	}
	return total, nil
}

// multiReaderAt presents consecutive files as one io.ReaderAt.
type multiReaderAt struct {
	parts []volumePart
	size  int64
}

type volumePart struct {
	f    *os.File
	off  int64
	size int64
}

// openMulti opens all volumes. The caller closes the result.
func openMulti(paths []string) (*multiReaderAt, error) {
	m := &multiReaderAt{}
	for _, p := range paths {
		f, size, err := openVolume(p)
		if err != nil {
			_ = m.Close()
			return nil, err
		}
		m.parts = append(m.parts, volumePart{f: f, off: m.size, size: size})
		m.size += size
	}
	return m, nil
}

func (m *multiReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("unpack: negative offset")
	}
	n := 0
	for _, part := range m.parts {
		if len(p) == 0 {
			break
		}
		if off >= part.off+part.size {
			continue
		}
		k, err := part.f.ReadAt(p[:min(int64(len(p)), part.off+part.size-off)], off-part.off)
		n += k
		p = p[k:]
		off += int64(k)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, err
		}
		if k == 0 {
			break
		}
	}
	if len(p) > 0 {
		return n, io.EOF
	}
	return n, nil
}

func (m *multiReaderAt) Close() error {
	var errs []error
	for _, part := range m.parts {
		errs = append(errs, part.f.Close())
	}
	return errors.Join(errs...)
}
