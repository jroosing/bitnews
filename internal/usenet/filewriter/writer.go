package filewriter

import (
	"container/list"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/usenet/yenc"
)

// DefaultMaxOpenFiles is the default bound on simultaneously open files.
const DefaultMaxOpenFiles = 64

// maxCreateTries bounds the retries when a chosen name is taken by a racing
// creator.
const maxCreateTries = 100

// FileKey identifies one file of a Download, normally its index in the NZB.
type FileKey int

// Option configures a Writer.
type Option func(*config)

type config struct {
	maxOpen int
}

// WithMaxOpenFiles bounds how many files the Writer keeps open at once.
// Evicted files are reopened on demand. Values below 1 are ignored.
func WithMaxOpenFiles(n int) Option {
	return func(c *config) {
		if n >= 1 {
			c.maxOpen = n
		}
	}
}

// Writer writes decoded yEnc parts into the files of one Download
// directory. It is safe for concurrent use. Calls for different keys run in
// parallel; calls for one key are serialized, and no lock shared between
// keys is held during file I/O except while a new file is being named and
// created.
type Writer struct {
	dir  string
	pool *pool

	closed   atomic.Bool
	dirDirty atomic.Bool

	mu      sync.Mutex // guards entries
	entries map[FileKey]*entry

	nameMu sync.Mutex // serializes file creation; guards names
	names  map[string]bool
}

// entry is the state of one file. mu guards every field except elem, which
// is guarded by the pool's lock.
type entry struct {
	mu   sync.Mutex
	key  FileKey
	name string // final on-disk name; empty until the file exists
	size int64
	t    tracker

	f       *os.File
	dirty   bool  // written since the last fsync
	pending error // fsync/close error from an eviction, reported later
	elem    *list.Element
}

// New creates a Writer for dir, creating the directory (0o777 before the
// umask) if needed.
func New(dir string, opts ...Option) (*Writer, error) {
	cfg := config{maxOpen: DefaultMaxOpenFiles}
	for _, o := range opts {
		o(&cfg)
	}
	if dir == "" {
		return nil, errors.New("filewriter: empty directory")
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, fmt.Errorf("filewriter: create dir: %w", err)
	}
	return &Writer{
		dir:     dir,
		pool:    newPool(cfg.maxOpen),
		entries: make(map[FileKey]*entry),
		names:   make(map[string]bool),
	}, nil
}

func validatePart(p yenc.Part) error {
	switch {
	case p.Size < 1:
		return fmt.Errorf("%w: size %d", ErrInvalidPart, p.Size)
	case p.Size > MaxFileSize:
		return fmt.Errorf("%w: %d bytes", ErrTooLarge, p.Size)
	case p.Begin < 1 || p.End < p.Begin || p.End > p.Size:
		return fmt.Errorf("%w: range %d-%d of %d", ErrInvalidPart, p.Begin, p.End, p.Size)
	case int64(len(p.Data)) != p.End-p.Begin+1:
		return fmt.Errorf("%w: %d bytes for range %d-%d", ErrInvalidPart, len(p.Data), p.Begin, p.End)
	}
	return nil
}

// WritePart writes part into the file identified by key, creating and
// pre-allocating the file on the first part.
//
// A part whose announced CRC32 does not match is written, but only over
// bytes that do not already hold a good copy, and is recorded as damaged. A
// good part overwrites its range and clears damaged state there. Duplicate
// parts are therefore idempotent. On error the bookkeeping is unchanged.
func (w *Writer) WritePart(key FileKey, part yenc.Part) error {
	if err := validatePart(part); err != nil {
		return err
	}
	e := w.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	if w.closed.Load() {
		return ErrClosed
	}

	if e.name == "" {
		if err := w.create(e, part); err != nil {
			return err
		}
	} else if part.Size != e.size {
		return fmt.Errorf("%w: key %d has %d bytes, part says %d", ErrSizeMismatch, key, e.size, part.Size)
	}

	f, err := w.handle(e)
	if err != nil {
		return err
	}
	r := Range{Start: part.Begin - 1, End: part.End}
	if part.HasExpectedCRC && !part.CRCMatch() {
		pieces := e.t.damageable(r)
		for _, p := range pieces {
			if err := writeAt(f, e.name, part.Data[p.Start-r.Start:p.End-r.Start], p.Start); err != nil {
				return err
			}
			e.dirty = true
		}
		e.t.markDamaged(pieces)
		return nil
	}
	if err := writeAt(f, e.name, part.Data, r.Start); err != nil {
		return err
	}
	e.dirty = true
	e.t.markGood(r)
	return nil
}

func writeAt(f *os.File, name string, data []byte, off int64) error {
	if _, err := f.WriteAt(data, off); err != nil {
		return fmt.Errorf("filewriter: write %s at %d: %w", name, off, err)
	}
	return nil
}

func (w *Writer) entry(key FileKey) *entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.entries[key]
	if e == nil {
		e = &entry{key: key}
		w.entries[key] = e
	}
	return e
}

func (w *Writer) lookup(key FileKey) *entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.entries[key]
}

// snapshotEntries returns the entries in key order.
func (w *Writer) snapshotEntries() []*entry {
	w.mu.Lock()
	es := make([]*entry, 0, len(w.entries))
	for _, e := range w.entries {
		es = append(es, e)
	}
	w.mu.Unlock()
	slices.SortFunc(es, func(a, b *entry) int { return int(a.key) - int(b.key) })
	return es
}

// create picks a safe, unique name for e, creates and pre-allocates the
// file and leaves it open. The caller holds e.mu.
func (w *Writer) create(e *entry, part yenc.Part) error {
	w.nameMu.Lock()
	name, f, err := w.createFile(e.key, part.Name)
	w.nameMu.Unlock()
	if err != nil {
		return err
	}
	// Sparse pre-allocation; see the package documentation.
	if err := f.Truncate(part.Size); err != nil {
		_ = f.Close()
		w.discard(name)
		return fmt.Errorf("filewriter: preallocate %s: %w", name, err)
	}
	w.dirDirty.Store(true)
	e.name, e.size, e.f, e.t = name, part.Size, f, tracker{}
	w.evict(w.pool.use(e))
	return nil
}

// discard removes a file this Writer just created, and frees its name.
func (w *Writer) discard(name string) {
	if p, err := fsutil.SafeJoin(w.dir, name); err == nil {
		_ = os.Remove(p)
	}
	w.nameMu.Lock()
	delete(w.names, name)
	w.nameMu.Unlock()
}

// createFile chooses the final name and creates the file with O_EXCL. The
// caller holds w.nameMu.
func (w *Writer) createFile(key FileKey, raw string) (string, *os.File, error) {
	base, err := fsutil.SafeName(raw)
	if err != nil {
		base = fmt.Sprintf("file-%d.bin", key)
	}
	for try := range maxCreateTries {
		// base comes from SafeName, so it has no separators. Unique looks
		// at the entry itself (Lstat), so a planted symlink counts as taken.
		name := filepath.Base(fsutil.Unique(filepath.Join(w.dir, base)))
		if w.names[name] {
			// Taken by a file that is not on disk right now.
			base = fmt.Sprintf("file-%d-%d.bin", key, try)
			continue
		}
		p, err := fsutil.SafeJoin(w.dir, name)
		if err != nil {
			return "", nil, fmt.Errorf("filewriter: %w", err)
		}
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL|oNoFollow, 0o666)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("filewriter: create %s: %w", name, err)
		}
		w.names[name] = true
		return name, f, nil
	}
	return "", nil, fmt.Errorf("filewriter: no free name for %q", raw)
}

// handle returns the open file of e, reopening it by its recorded name if it
// was evicted or finished. It never follows a symlink. The caller holds e.mu.
func (w *Writer) handle(e *entry) (*os.File, error) {
	if e.f == nil {
		f, err := w.openExisting(e.name)
		if err != nil {
			return nil, err
		}
		e.f = f
	}
	w.evict(w.pool.use(e))
	return e.f, nil
}

// openExisting opens an existing regular file inside the directory for
// reading and writing, refusing symlinks and special files.
func (w *Writer) openExisting(name string) (*os.File, error) {
	p, err := fsutil.SafeJoin(w.dir, name)
	if err != nil {
		return nil, fmt.Errorf("filewriter: %w", err)
	}
	f, err := os.OpenFile(p, os.O_RDWR|oNoFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("filewriter: open %s: %w", name, err)
	}
	lst, lerr := os.Lstat(p)
	st, serr := f.Stat()
	if lerr != nil || serr != nil || !lst.Mode().IsRegular() || !os.SameFile(lst, st) {
		_ = f.Close()
		return nil, fmt.Errorf("filewriter: %s is not the regular file this writer created: %w", name, fsutil.ErrSymlink)
	}
	return f, nil
}

// evict closes least recently used files, skipping any that are busy, until
// the pool is back within its limit.
func (w *Writer) evict(cands []*entry) {
	for _, c := range cands {
		if !w.pool.over() {
			return
		}
		if !c.mu.TryLock() {
			continue
		}
		if c.f != nil {
			if err := w.closeEntry(c); err != nil {
				c.pending = errors.Join(c.pending, err)
			}
		}
		c.mu.Unlock()
	}
}

// closeEntry fsyncs (when dirty) and closes the handle of e. The caller
// holds e.mu.
func (w *Writer) closeEntry(e *entry) error {
	var err error
	if e.f != nil {
		if e.dirty {
			if serr := e.f.Sync(); serr != nil {
				err = fmt.Errorf("filewriter: fsync %s: %w", e.name, serr)
			}
		}
		if cerr := e.f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("filewriter: close %s: %w", e.name, cerr))
		}
		if err == nil {
			e.dirty = false
		}
		e.f = nil
	}
	w.pool.drop(e)
	err = errors.Join(e.pending, err)
	e.pending = nil
	return err
}

// syncEntry fsyncs e if it has unsynced writes. The caller holds e.mu.
func (w *Writer) syncEntry(e *entry) error {
	err := e.pending
	e.pending = nil
	if e.f != nil && e.dirty {
		if serr := e.f.Sync(); serr != nil {
			return errors.Join(err, fmt.Errorf("filewriter: fsync %s: %w", e.name, serr))
		}
		e.dirty = false
	}
	return err
}

func (w *Writer) syncDir() error {
	if runtime.GOOS == "windows" || !w.dirDirty.Load() {
		return nil
	}
	d, err := os.Open(w.dir)
	if err != nil {
		return fmt.Errorf("filewriter: open dir for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("filewriter: fsync dir: %w", err)
	}
	w.dirDirty.Store(false)
	return nil
}

// Finish fsyncs the file for key and closes its handle. The bookkeeping is
// kept, and a later WritePart reopens the file.
func (w *Writer) Finish(key FileKey) error {
	e := w.lookup(key)
	if e == nil {
		return fmt.Errorf("%w: %d", ErrUnknownFile, key)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.name == "" {
		return fmt.Errorf("%w: %d", ErrUnknownFile, key)
	}
	return errors.Join(w.closeEntry(e), w.syncDir())
}

// Sync fsyncs every file with unsynced writes (and the directory, if files
// were created). Call it before persisting state.
func (w *Writer) Sync() error {
	var errs []error
	for _, e := range w.snapshotEntries() {
		e.mu.Lock()
		errs = append(errs, w.syncEntry(e))
		e.mu.Unlock()
	}
	errs = append(errs, w.syncDir())
	return errors.Join(errs...)
}

// Close fsyncs and closes every file. Further writes fail with ErrClosed.
// Close is idempotent.
func (w *Writer) Close() error {
	if w.closed.Swap(true) {
		return nil
	}
	var errs []error
	for _, e := range w.snapshotEntries() {
		e.mu.Lock()
		errs = append(errs, w.closeEntry(e))
		e.mu.Unlock()
	}
	errs = append(errs, w.syncDir())
	return errors.Join(errs...)
}
