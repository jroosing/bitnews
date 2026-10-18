package filewriter

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"slices"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// Snapshot wire format (version 1), all integers are unsigned varints
// unless noted:
//
//	"GNFW" version(1 byte)
//	nfiles
//	nfiles x { key, len(name), name, size, ranges(done), ranges(damaged), ranges(lost) }
//	crc32 (IEEE, 4 bytes little endian) over everything before it
//
// ranges is: n, then n x { start - previousEnd, end - start }, so a file
// with long runs costs a few bytes per run regardless of its size.
const (
	snapMagic   = "GNFW"
	snapVersion = 1
)

type fileRecord struct {
	key                 FileKey
	name                string
	size                int64
	done, damaged, lost rangeSet
}

func appendRanges(b []byte, rs rangeSet) []byte {
	b = binary.AppendUvarint(b, uint64(len(rs)))
	var prev int64
	for _, r := range rs {
		b = binary.AppendUvarint(b, uint64(r.Start-prev))
		b = binary.AppendUvarint(b, uint64(r.Len()))
		prev = r.End
	}
	return b
}

func encodeSnapshot(recs []fileRecord) []byte {
	b := append([]byte(snapMagic), snapVersion)
	b = binary.AppendUvarint(b, uint64(len(recs)))
	for _, r := range recs {
		b = binary.AppendUvarint(b, uint64(r.key))
		b = binary.AppendUvarint(b, uint64(len(r.name)))
		b = append(b, r.name...)
		b = binary.AppendUvarint(b, uint64(r.size))
		b = appendRanges(b, r.done)
		b = appendRanges(b, r.damaged)
		b = appendRanges(b, r.lost)
	}
	return binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

// snapReader reads varints from a bounded buffer; the first problem sticks.
type snapReader struct {
	b   []byte
	err error
}

func (r *snapReader) fail(msg string) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: %s", ErrInvalidSnapshot, msg)
	}
}

func (r *snapReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.fail("bad varint")
		return 0
	}
	r.b = r.b[n:]
	return v
}

// count reads an element count and bounds it by the bytes left, given the
// minimum encoded size of one element.
func (r *snapReader) count(minElem int) int {
	n := r.uvarint()
	if r.err == nil && n > uint64(len(r.b)/minElem) {
		r.fail("count exceeds data")
		return 0
	}
	return int(n)
}

func (r *snapReader) ranges(size int64) rangeSet {
	n := r.count(2)
	var rs rangeSet
	var prev int64
	for i := 0; i < n && r.err == nil; i++ {
		gap, length := r.uvarint(), r.uvarint()
		if r.err != nil {
			break
		}
		if gap > uint64(size) || length == 0 || length > uint64(size) || (i > 0 && gap == 0) {
			r.fail("bad range")
			break
		}
		start := prev + int64(gap)
		end := start + int64(length)
		if end > size {
			r.fail("range beyond file size")
			break
		}
		rs = append(rs, Range{start, end})
		prev = end
	}
	return rs
}

func decodeSnapshot(data []byte) ([]fileRecord, error) {
	hdr := len(snapMagic) + 1
	if len(data) < hdr+1+4 || string(data[:len(snapMagic)]) != snapMagic {
		return nil, fmt.Errorf("%w: bad header", ErrInvalidSnapshot)
	}
	if data[len(snapMagic)] != snapVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidSnapshot, data[len(snapMagic)])
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrInvalidSnapshot)
	}

	r := &snapReader{b: body[hdr:]}
	n := r.count(5)
	recs := make([]fileRecord, 0, n)
	keys := make(map[FileKey]bool, n)
	names := make(map[string]bool, n)
	for i := 0; i < n && r.err == nil; i++ {
		var rec fileRecord
		key := r.uvarint()
		nameLen := r.count(1)
		if r.err != nil {
			break
		}
		rec.name = string(r.b[:nameLen])
		r.b = r.b[nameLen:]
		size := r.uvarint()
		if r.err == nil && (size < 1 || size > uint64(MaxFileSize)) {
			r.fail("bad file size")
		}
		if r.err != nil {
			break
		}
		rec.size = int64(size)
		if key > math.MaxInt32 || keys[FileKey(key)] {
			r.fail("bad or duplicate key")
			break
		}
		rec.key = FileKey(key)
		if safe, err := fsutil.SafeName(rec.name); err != nil || safe != rec.name || names[rec.name] {
			r.fail("unsafe or duplicate name")
			break
		}
		rec.done = r.ranges(rec.size)
		rec.damaged = r.ranges(rec.size)
		rec.lost = r.ranges(rec.size)
		if r.err == nil && !disjoint(rec.done, rec.damaged, rec.lost) {
			r.fail("overlapping range sets")
		}
		keys[rec.key], names[rec.name] = true, true
		recs = append(recs, rec)
	}
	if r.err == nil && len(r.b) != 0 {
		r.fail("trailing bytes")
	}
	if r.err != nil {
		return nil, r.err
	}
	return recs, nil
}

// disjoint reports whether no byte is in more than one of the sets.
func disjoint(sets ...rangeSet) bool {
	var all rangeSet
	for _, s := range sets {
		for _, r := range s {
			if g := all.gaps(r); len(g) != 1 || g[0] != r {
				return false
			}
			all.add(r)
		}
	}
	return true
}

// Snapshot returns the per-file bookkeeping (name, size, done, damaged and
// lost ranges) in a compact, versioned binary form for persistence. Each
// file with unsynced writes is fsynced first, under that file's lock, so a
// snapshot never claims bytes that are not durable.
func (w *Writer) Snapshot() ([]byte, error) {
	var recs []fileRecord
	for _, e := range w.snapshotEntries() {
		e.mu.Lock()
		if e.name == "" {
			e.mu.Unlock()
			continue
		}
		if err := w.syncEntry(e); err != nil {
			e.mu.Unlock()
			return nil, err
		}
		recs = append(recs, fileRecord{
			key: e.key, name: e.name, size: e.size,
			done: slices.Clone(e.t.done), damaged: slices.Clone(e.t.damaged), lost: slices.Clone(e.t.lost),
		})
		e.mu.Unlock()
	}
	if err := w.syncDir(); err != nil {
		return nil, err
	}
	return encodeSnapshot(recs), nil
}

// Restore loads bookkeeping produced by Snapshot into an empty Writer whose
// directory still holds the files, and validates it against the disk. A
// file that is missing or not a regular file is forgotten; one whose size
// differs from the recorded size is re-allocated and its state reset. The
// keys that were reset or forgotten are returned in ascending order, so the
// caller knows their Articles must be fetched again.
func (w *Writer) Restore(snapshot []byte) ([]FileKey, error) {
	recs, err := decodeSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	empty := len(w.entries) == 0
	w.mu.Unlock()
	if !empty {
		return nil, ErrNotEmpty
	}

	var reset []FileKey
	var keep []*entry
	for _, rec := range recs {
		e, status := w.checkOnDisk(rec)
		switch status {
		case onDiskOK:
			keep = append(keep, e)
		case onDiskReset:
			reset = append(reset, rec.key)
			keep = append(keep, e)
		case onDiskGone:
			reset = append(reset, rec.key)
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.entries) != 0 {
		return nil, ErrNotEmpty
	}
	w.nameMu.Lock()
	defer w.nameMu.Unlock()
	for _, e := range keep {
		w.entries[e.key] = e
		w.names[e.name] = true
	}
	slices.Sort(reset)
	return reset, nil
}

type onDiskStatus int

const (
	onDiskOK    onDiskStatus = iota // file matches the record
	onDiskReset                     // file re-allocated, state cleared
	onDiskGone                      // file missing or unusable, forgotten
)

func (w *Writer) checkOnDisk(rec fileRecord) (*entry, onDiskStatus) {
	p, err := fsutil.SafeJoin(w.dir, rec.name)
	if err != nil {
		return nil, onDiskGone
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, onDiskGone
	}
	e := &entry{key: rec.key, name: rec.name, size: rec.size}
	if fi.Size() == rec.size {
		e.t = tracker{done: rec.done, damaged: rec.damaged, lost: rec.lost}
		return e, onDiskOK
	}
	f, err := w.openExisting(rec.name)
	if err != nil {
		return nil, onDiskGone
	}
	defer f.Close()
	if err := f.Truncate(rec.size); err != nil {
		return nil, onDiskGone
	}
	return e, onDiskReset
}
