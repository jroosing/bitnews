package par2

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	// ErrNoMainPacket means no usable Main packet was found.
	ErrNoMainPacket = errors.New("par2: no usable main packet")
	// ErrInvalidSliceSize means every Main packet had a slice size that is
	// zero or not a multiple of 4.
	ErrInvalidSliceSize = errors.New("par2: invalid slice size")
)

const (
	maxWarnings   = 100 // further warnings are counted, not kept
	maxWarningLen = 512
	maxQuotedName = 64
)

// RecoverySlice locates the data of one recovery slice on disk.
type RecoverySlice struct {
	Path   string
	Offset int64
	Length int64
}

// Set is the merged content of every .par2 file of one recovery set.
//
// Names inside FileDescs are exactly what the uploader wrote (trailing NULs
// trimmed) and must be sanitized before they are used as paths. Warnings
// never contain raw names: they are quoted, ASCII-only and truncated.
type Set struct {
	ID             ID
	SliceSize      uint64
	FileIDs        []ID // recovery-set files, in Main packet order
	NonRecoveryIDs []ID
	FileDescs      map[ID]*FileDesc // validated: the file ID matches its content
	IFSCs          map[ID]*IFSC     // validated: one pair per slice of the file
	Recovery       map[uint32]RecoverySlice
	Creator        string
	Warnings       []string // non-fatal problems
}

// SliceCount returns the number of slices of a file of the given length.
func (s *Set) SliceCount(length uint64) uint64 {
	n := length / s.SliceSize
	if length%s.SliceSize != 0 {
		n++
	}
	return n
}

// LoadDir merges the packets of every *.par2 file (any letter case) directly
// in dir; subdirectories, symlinks and other files are ignored.
func LoadDir(dir string) (*Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries { // sorted by name
		if e.Type().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return LoadFiles(paths)
}

type located struct {
	Packet
	path string
}

// warnings collects bounded, sanitized messages.
type warnings struct {
	list    []string
	dropped int
}

func (w *warnings) addf(format string, args ...any) {
	if len(w.list) >= maxWarnings {
		w.dropped++
		return
	}
	msg := fmt.Sprintf(format, args...)
	if len(msg) > maxWarningLen {
		msg = msg[:maxWarningLen]
	}
	w.list = append(w.list, msg)
}

func (w *warnings) result() []string {
	if w.dropped > 0 {
		return append(w.list, fmt.Sprintf("%d further warnings suppressed", w.dropped))
	}
	return w.list
}

// quote renders untrusted text as short, printable ASCII.
func quote(s string) string {
	if len(s) > maxQuotedName {
		s = s[:maxQuotedName] + "..."
	}
	return strconv.QuoteToASCII(s)
}

// LoadFiles merges the packets of the given .par2 files. Files are read in
// the given order, which decides which of two duplicate recovery slices is
// kept. Unreadable files only cause warnings.
func LoadFiles(paths []string) (*Set, error) {
	var w warnings
	var pkts []located
	for _, path := range paths {
		pkts = append(pkts, readPackets(path, &w)...)
	}

	s, err := chooseSet(pkts, &w)
	if err != nil {
		return nil, err
	}
	s.merge(pkts, &w)
	s.Warnings = w.result()
	return s, nil
}

func readPackets(path string, w *warnings) []located {
	name := quote(filepath.Base(path))
	f, err := os.Open(path)
	if err != nil {
		w.addf("%s: cannot open: %s", name, quote(err.Error()))
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		w.addf("%s: not a regular file", name)
		return nil
	}
	var out []located
	skipped := 0
	for p, err := range scan(f, fi.Size(), func(int64, string) { skipped++ }) {
		if err != nil {
			w.addf("%s: read error: %s", name, quote(err.Error()))
			break
		}
		out = append(out, located{p, path})
	}
	if skipped > 0 {
		w.addf("%s: skipped %d damaged or unusable region(s)", name, skipped)
	}
	return out
}

func validSliceSize(n uint64) bool { return n != 0 && n%4 == 0 }

// chooseSet picks the recovery set the directory is about: among sets with a
// self-consistent Main packet, the one with the most packets, smallest ID
// winning ties.
func chooseSet(pkts []located, w *warnings) (*Set, error) {
	refs := map[ID]int{}
	for _, p := range pkts {
		refs[p.SetID]++
	}
	mains := map[ID]*Main{}
	badSize, forged := false, 0
	for _, p := range pkts {
		m, ok := p.Body.(*Main)
		if !ok {
			continue
		}
		switch {
		case m.DerivedSetID != p.SetID:
			forged++
		case !validSliceSize(m.SliceSize):
			badSize = true
		case mains[p.SetID] == nil:
			mains[p.SetID] = m
		}
	}
	if forged > 0 {
		w.addf("%d main packet(s) whose recovery set ID is not the MD5 of their body ignored", forged)
	}
	if len(mains) == 0 {
		if badSize {
			return nil, ErrInvalidSliceSize
		}
		return nil, ErrNoMainPacket
	}
	var best ID
	first := true
	for id := range mains {
		if first || refs[id] > refs[best] || refs[id] == refs[best] && bytes.Compare(id[:], best[:]) < 0 {
			best, first = id, false
		}
	}
	m := mains[best]
	s := &Set{
		ID:        best,
		SliceSize: m.SliceSize,
		FileDescs: map[ID]*FileDesc{},
		IFSCs:     map[ID]*IFSC{},
		Recovery:  map[uint32]RecoverySlice{},
	}
	seen := map[ID]bool{}
	for _, id := range m.RecoveryIDs {
		if !seen[id] {
			seen[id] = true
			s.FileIDs = append(s.FileIDs, id)
		}
	}
	for _, id := range m.NonRecoveryIDs {
		if !seen[id] {
			seen[id] = true
			s.NonRecoveryIDs = append(s.NonRecoveryIDs, id)
		}
	}
	return s, nil
}

// fileID is the PAR2 file ID: MD5(MD5 of the first 16 KiB, length, name).
func fileID(fd *FileDesc) ID {
	h := md5.New()
	h.Write(fd.MD516k[:])
	h.Write(binary.LittleEndian.AppendUint64(nil, fd.Length))
	h.Write([]byte(fd.Name))
	var id ID
	h.Sum(id[:0])
	return id
}

// merge adds the validated, deduplicated packets of the chosen set.
func (s *Set) merge(pkts []located, w *warnings) {
	listed := map[ID]bool{}
	for _, id := range s.FileIDs {
		listed[id] = true
	}
	for _, id := range s.NonRecoveryIDs {
		listed[id] = true
	}

	var ifscs []*IFSC
	other, badID, unlisted, badSlice := 0, 0, 0, 0
	for _, p := range pkts {
		if p.SetID != s.ID {
			other++
			continue
		}
		switch b := p.Body.(type) {
		case *FileDesc:
			switch old := s.FileDescs[b.FileID]; {
			case fileID(b) != b.FileID:
				badID++
				w.addf("file description %s: file ID does not match its content, ignored", quote(b.Name))
			case !listed[b.FileID]:
				unlisted++
			case old == nil:
				s.FileDescs[b.FileID] = b
			case old.MD5 != b.MD5:
				w.addf("conflicting file descriptions for file %s, keeping the first", b.FileID)
			}
		case *IFSC:
			ifscs = append(ifscs, b)
		case *RecvSlic:
			if uint64(b.DataLength) != s.SliceSize {
				badSlice++
			} else if _, dup := s.Recovery[b.Exponent]; !dup {
				s.Recovery[b.Exponent] = RecoverySlice{Path: p.path, Offset: b.DataOffset, Length: b.DataLength}
			}
		case *Creator:
			if s.Creator == "" {
				s.Creator = b.Text
			}
		}
	}
	if other > 0 {
		w.addf("%d packet(s) of other recovery set(s) ignored", other)
	}
	if unlisted > 0 {
		w.addf("%d file description(s) for files not listed in the main packet ignored", unlisted)
	}
	if badSlice > 0 {
		w.addf("%d recovery slice(s) whose size is not the slice size ignored", badSlice)
	}

	wrongCount := map[ID]int{}
	orphan := 0
	for _, b := range ifscs {
		fd := s.FileDescs[b.FileID]
		if fd == nil {
			orphan++
			continue
		}
		if _, ok := s.IFSCs[b.FileID]; ok {
			continue
		}
		if uint64(len(b.Slices)) != s.SliceCount(fd.Length) {
			wrongCount[b.FileID] = len(b.Slices)
			continue
		}
		s.IFSCs[b.FileID] = b
	}
	for _, id := range s.allIDs() { // deterministic order
		if n, bad := wrongCount[id]; bad && s.IFSCs[id] == nil {
			w.addf("IFSC for file %s has %d slice checksums, expected %d, ignored", id, n, s.SliceCount(s.FileDescs[id].Length))
		}
	}
	if orphan > 0 {
		w.addf("%d IFSC packet(s) without a valid file description ignored", orphan)
	}

	noDesc, noIFSC := 0, 0
	for _, id := range s.allIDs() {
		fd := s.FileDescs[id]
		switch {
		case fd == nil:
			noDesc++
		case s.IFSCs[id] == nil && fd.Length > 0:
			noIFSC++
		}
	}
	if noDesc > 0 {
		w.addf("%d file(s) listed in the main packet have no description", noDesc)
	}
	if noIFSC > 0 {
		w.addf("%d file(s) have no valid slice checksums", noIFSC)
	}
}

func (s *Set) allIDs() []ID {
	return append(append([]ID(nil), s.FileIDs...), s.NonRecoveryIDs...)
}
