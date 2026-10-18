package filewriter

import (
	"fmt"
	"slices"
)

// FileState is a point-in-time copy of the bookkeeping of one file. All
// range lists are sorted, disjoint and non-adjacent, and are half-open and
// zero-based.
type FileState struct {
	Key FileKey
	// Name is the final on-disk name inside the Writer's directory.
	Name string
	Size int64
	// Done holds ranges written from a part whose CRC32 matched (or was not
	// announced).
	Done []Range
	// Damaged holds ranges written from a part whose CRC32 did not match.
	Damaged []Range
	// Missing holds every byte that is neither Done nor Damaged.
	Missing []Range
	// Lost is the subset of Missing that MarkMissing declared permanently
	// unavailable.
	Lost []Range
	// Pending is Missing minus Lost: bytes that may still arrive.
	Pending []Range
}

// Complete reports whether every byte is Done: nothing is missing or
// damaged.
func (s FileState) Complete() bool { return len(s.Missing) == 0 && len(s.Damaged) == 0 }

// DoneBytes returns the number of bytes in Done.
func (s FileState) DoneBytes() int64 { return Sum(s.Done) }

// DamagedBytes returns the number of bytes in Damaged.
func (s FileState) DamagedBytes() int64 { return Sum(s.Damaged) }

// MissingBytes returns the number of bytes in Missing.
func (s FileState) MissingBytes() int64 { return Sum(s.Missing) }

// state builds the exported view of e. The caller holds e.mu.
func (e *entry) state() FileState {
	missing := e.t.missing(e.size)
	return FileState{
		Key:     e.key,
		Name:    e.name,
		Size:    e.size,
		Done:    slices.Clone(e.t.done),
		Damaged: slices.Clone(e.t.damaged),
		Missing: slices.Clone(missing),
		Lost:    slices.Clone(e.t.lost),
		Pending: missing.minus(e.t.lost),
	}
}

// Ranges returns the bookkeeping of the file for key. ok is false if no
// part has been written for key yet.
func (w *Writer) Ranges(key FileKey) (FileState, bool) {
	e := w.lookup(key)
	if e == nil {
		return FileState{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.name == "" {
		return FileState{}, false
	}
	return e.state(), true
}

// MarkMissing declares r (half-open, zero-based) permanently unavailable,
// for example because the Article carrying it is a Missing Article. Bytes
// that are already Done or Damaged are unaffected, and a later part
// covering r clears the mark. The file must already exist: its size is
// unknown before the first part.
func (w *Writer) MarkMissing(key FileKey, r Range) error {
	e := w.lookup(key)
	if e == nil {
		return fmt.Errorf("%w: %d", ErrUnknownFile, key)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.name == "" {
		return fmt.Errorf("%w: %d", ErrUnknownFile, key)
	}
	if r.Start < 0 || r.End < r.Start || r.End > e.size {
		return fmt.Errorf("%w: missing range [%d,%d) of %d", ErrInvalidPart, r.Start, r.End, e.size)
	}
	e.t.markLost(r)
	return nil
}
