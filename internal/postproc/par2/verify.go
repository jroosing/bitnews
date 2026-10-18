package par2

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// State is the verdict for one file of the set.
type State int

const (
	// StateOK means the file matches its description byte for byte.
	StateOK State = iota
	// StateDamaged means the file exists but differs from its description.
	// FileResult.DamagedSlices lists the aligned slices that do not match.
	StateDamaged
	// StateMissing means no file was found, by name or by content.
	StateMissing
	// StateUnverifiable means the file differs from its description but the
	// set has no slice checksums for it, so the damage cannot be located.
	StateUnverifiable
)

func (s State) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StateDamaged:
		return "damaged"
	case StateMissing:
		return "missing"
	case StateUnverifiable:
		return "unverifiable"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// Rename records a rename of an obfuscated file to its real name. Both are
// plain file names inside the verified directory.
type Rename struct {
	From, To string
}

// FileResult is the verdict for one file of the recovery set.
type FileResult struct {
	// Name is the file name inside the directory: the description's name
	// passed through fsutil.SafeName, or the name the file was renamed to
	// (which may carry a counter when the safe name was taken by another
	// file). A description whose name cannot be made safe is called
	// "unnamed-<first 8 hex digits of its file ID>".
	Name   string
	FileID ID
	State  State
	// Path is where the file was verified: below the directory, at its
	// old name when Renamed is set on a dry run. Empty for a missing file.
	Path string
	// Size is the length of the file on disk (0 when missing).
	Size int64
	// TotalSlices is the number of slices the file has per its description.
	TotalSlices uint64
	// DamagedCount is the number of slices that must be recovered. A missing
	// file and an unverifiable file count all of their slices.
	DamagedCount uint64
	// DamagedSlices lists the indices of the damaged slices, ascending. It is
	// filled only for StateDamaged: for a missing or unverifiable file every
	// slice is damaged and the (possibly astronomically long) list is not
	// built. A damaged file with an empty list has intact slices but the
	// wrong length (for example trailing bytes), which repair fixes by
	// truncating.
	DamagedSlices []int
	// Renamed is set when the file was matched by content under another name.
	Renamed *Rename
}

// Result is the outcome of Verify.
type Result struct {
	Files []FileResult
	// DamagedSlices is the total of FileResult.DamagedCount, saturating.
	DamagedSlices uint64
	// RecoverySlicesOnDisk is the number of distinct recovery slices in the Set.
	RecoverySlicesOnDisk int
	// RecoverySlicesDeferred is what the caller said it could still fetch
	// (WithDeferredRecoveryBlocks).
	RecoverySlicesDeferred int
	// Warnings are non-fatal problems, quoted and ASCII-only.
	Warnings []string
}

// NeedsRepair reports whether any file is not OK.
func (r *Result) NeedsRepair() bool {
	for _, f := range r.Files {
		if f.State != StateOK {
			return true
		}
	}
	return false
}

// RecoverySlicesNeeded is the number of recovery slices a repair consumes: one
// per damaged slice. It saturates at the largest int.
func (r *Result) RecoverySlicesNeeded() int {
	if r.DamagedSlices > math.MaxInt {
		return math.MaxInt
	}
	return int(r.DamagedSlices)
}

// Repairable reports whether the recovery slices on disk plus the deferred
// ones cover the damage. It says nothing about the matrix being solvable.
func (r *Result) Repairable() bool {
	return r.DamagedSlices <= uint64(r.RecoverySlicesOnDisk)+uint64(r.RecoverySlicesDeferred)
}

// Option configures Verify.
type Option func(*config)

type config struct {
	dryRun   bool
	deferred int
}

// WithDryRun makes Verify report the renames it would do without doing them.
// Verify then writes nothing at all.
func WithDryRun() Option { return func(c *config) { c.dryRun = true } }

// WithDeferredRecoveryBlocks tells Verify how many recovery slices the caller
// can still fetch (volumes not downloaded yet). Negative values count as 0.
func WithDeferredRecoveryBlocks(n int) Option {
	return func(c *config) { c.deferred = max(n, 0) }
}

// Limits that keep hostile sets and directories from costing unbounded work.
const (
	verifyChunk = 1 << 20 // read buffer size
	head16k     = 16 * 1024
	// MaxSliceSize is the largest slice size Verify accepts: a recovery packet
	// is at most 1 GiB, so larger slices could never be repaired anyway, and
	// the last slice of a file is hashed zero-padded to this size.
	MaxSliceSize = 1 << 30
	// maxCandidateHashes bounds how many unmatched files in a directory are
	// read for the 16 KiB hash.
	maxCandidateHashes = 10000
)

// ErrSliceSizeTooLarge means the set's slice size exceeds MaxSliceSize.
var ErrSliceSizeTooLarge = errors.New("par2: slice size too large")

// Verify checks the files in dir against set.
//
// Files are matched to descriptions first by exact name (the description's
// name passed through fsutil.SafeName), then, for descriptions still
// unmatched, by length plus MD5 of the first 16 KiB against every other
// regular file in dir; this finds obfuscated posts, and the match is renamed
// to the description's name. A file whose first 16 KiB are damaged cannot be
// found this way and counts as missing. Only the first maxCandidateHashes
// files whose length equals a wanted length are read, 16 KiB each. When the
// key is ambiguous the full MD5 decides, and if no candidate has the right
// full MD5 the first description in set order takes the first candidate in
// name order and the others stay unmatched (Missing).
//
// Only regular files are considered: symlinks, directories and special files
// are never opened, matched or renamed. Files are streamed, never read whole.
// Only files of Set.FileIDs with a description are reported; recovery-set
// files without a FileDesc cannot be verified and are skipped.
//
// The only change Verify ever makes to the filesystem is renaming files that
// it matched by content to the name in their description (never with
// WithDryRun).
func Verify(ctx context.Context, dir string, set *Set, opts ...Option) (*Result, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	if set == nil {
		return nil, ErrNoMainPacket
	}
	if !validSliceSize(set.SliceSize) {
		return nil, ErrInvalidSliceSize
	}
	if set.SliceSize > MaxSliceSize {
		return nil, ErrSliceSizeTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v := &verifier{ctx: ctx, dir: dir, set: set, cfg: cfg, buf: make([]byte, verifyChunk), zeros: make([]byte, verifyChunk)}
	return v.run()
}

type verifier struct {
	ctx   context.Context
	dir   string
	set   *Set
	cfg   config
	buf   []byte
	zeros []byte
	res   Result
	// taken holds the names that renames of this run produced; it matters for
	// dry runs, where the disk does not show them.
	taken map[string]bool
}

// target is a file description with the name it should have on disk.
type target struct {
	fd   *FileDesc
	name string
}

func (v *verifier) run() (*Result, error) {
	var targets []target
	for _, id := range v.set.FileIDs {
		fd := v.set.FileDescs[id]
		if fd == nil {
			continue
		}
		targets = append(targets, target{fd, safeTargetName(fd)})
	}

	cands, err := v.listCandidates()
	if err != nil {
		return nil, err
	}
	matched, err := v.match(targets, cands)
	if err != nil {
		return nil, err
	}

	v.taken = map[string]bool{}
	v.res.RecoverySlicesOnDisk = len(v.set.Recovery)
	v.res.RecoverySlicesDeferred = v.cfg.deferred
	for i, t := range targets {
		fr := FileResult{Name: t.name, FileID: t.fd.FileID, TotalSlices: v.set.SliceCount(t.fd.Length)}
		if c := matched[i]; c == nil {
			markAll(&fr, StateMissing)
		} else {
			diskName := c.name
			if c.name != t.name {
				diskName = v.rename(c.name, t.name, &fr)
			}
			if err := v.check(t, diskName, &fr); err != nil {
				return nil, err
			}
		}
		v.res.Files = append(v.res.Files, fr)
		v.res.DamagedSlices = satAdd(v.res.DamagedSlices, fr.DamagedCount)
	}
	return &v.res, nil
}

type contentKey struct {
	length uint64
	h16    [16]byte
}

// candidate is a regular file found in the directory.
type candidate struct {
	name    string
	size    uint64
	claimed bool
	hasHead bool
	h16     [16]byte
	full    [16]byte
	hasFull bool
}

// listCandidates returns the regular files of the directory, sorted by name.
// Symlinks, directories and special files are not candidates.
func (v *verifier) listCandidates() ([]*candidate, error) {
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		return nil, err
	}
	var out []*candidate
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, &candidate{name: e.Name(), size: uint64(fi.Size())})
	}
	return out, nil
}

// match pairs targets with candidates: by exact name first, then by content
// (length plus MD5 of the first 16 KiB) for the targets still unmatched. The
// result is indexed like targets; nil means no file was found.
//
// When the content key is ambiguous (several targets share a candidate, or a
// target has several candidates), a candidate whose full MD5 equals the
// target's wins; failing that the first target in set order takes the first
// candidate in name order, and the others stay unmatched.
func (v *verifier) match(targets []target, cands []*candidate) ([]*candidate, error) {
	matched := make([]*candidate, len(targets))
	byName := make(map[string]*candidate, len(cands))
	for _, c := range cands {
		byName[c.name] = c
	}
	var unmatched []int
	for i, t := range targets {
		if c := byName[t.name]; c != nil && !c.claimed {
			c.claimed = true
			matched[i] = c
		} else {
			unmatched = append(unmatched, i)
		}
	}
	if len(unmatched) == 0 {
		return matched, nil
	}

	wantLen := map[uint64]bool{}
	for _, i := range unmatched {
		wantLen[targets[i].fd.Length] = true
	}
	hashed := 0
	for _, c := range cands {
		if c.claimed || !wantLen[c.size] {
			continue
		}
		if hashed++; hashed > maxCandidateHashes {
			break
		}
		if err := v.hashHead(c); err != nil {
			return nil, err
		}
	}

	byKey := map[contentKey][]*candidate{}
	for _, c := range cands {
		if c.hasHead && !c.claimed {
			k := contentKey{c.size, c.h16}
			byKey[k] = append(byKey[k], c)
		}
	}
	perTarget := map[int][]*candidate{}
	uses := map[*candidate]int{}
	for _, i := range unmatched {
		fd := targets[i].fd
		for _, c := range byKey[contentKey{fd.Length, fd.MD516k}] {
			perTarget[i] = append(perTarget[i], c)
			uses[c]++
		}
	}
	// Ambiguous keys: let the full MD5 decide.
	for _, i := range unmatched {
		free := unclaimed(perTarget[i])
		if len(free) == 0 || len(free) == 1 && uses[free[0]] == 1 {
			continue
		}
		for _, c := range free {
			if err := v.hashFull(c); err != nil {
				return nil, err
			}
			if c.full == targets[i].fd.MD5 {
				c.claimed = true
				matched[i] = c
				break
			}
		}
	}
	for _, i := range unmatched {
		if matched[i] != nil {
			continue
		}
		if free := unclaimed(perTarget[i]); len(free) > 0 {
			free[0].claimed = true
			matched[i] = free[0]
		}
	}
	return matched, nil
}

func unclaimed(cs []*candidate) []*candidate {
	var out []*candidate
	for _, c := range cs {
		if !c.claimed {
			out = append(out, c)
		}
	}
	return out
}

func satAdd(a, b uint64) uint64 {
	if a+b < a {
		return math.MaxUint64
	}
	return a + b
}

func markAll(fr *FileResult, s State) {
	fr.State = s
	fr.DamagedCount = fr.TotalSlices
}

// safeTargetName is the on-disk name for a description.
func safeTargetName(fd *FileDesc) string {
	if n, err := fsutil.SafeName(fd.Name); err == nil {
		return n
	}
	return fmt.Sprintf("unnamed-%x", fd.FileID[:4])
}

// openRegular opens name inside the directory, refusing anything that is not
// a regular file (symlinks included) at both Lstat and open time.
func (v *verifier) openRegular(name string) (*os.File, os.FileInfo, error) {
	path, err := fsutil.SafeJoin(v.dir, name)
	if err != nil {
		return nil, nil, err
	}
	li, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, nil, errNotRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		f.Close()
		return nil, nil, errNotRegular
	}
	return f, fi, nil
}

var errNotRegular = errors.New("par2: not a regular file")

// check verifies the file called diskName in the directory against t.
func (v *verifier) check(t target, diskName string, fr *FileResult) error {
	f, fi, err := v.openRegular(diskName)
	if err != nil {
		if errors.Is(err, errNotRegular) || errors.Is(err, os.ErrNotExist) || errors.Is(err, fsutil.ErrUnsafePath) {
			markAll(fr, StateMissing)
			return nil
		}
		return err
	}
	defer f.Close()
	fr.Path, fr.Size = f.Name(), fi.Size()

	if uint64(fi.Size()) == t.fd.Length {
		sum, err := v.fullMD5(f, t.fd.Length)
		if err != nil {
			return err
		}
		if sum == t.fd.MD5 {
			fr.State = StateOK
			return nil
		}
	}
	return v.checkSlices(f, t.fd, fr)
}

// checkSlices compares every aligned slice with its IFSC pair.
func (v *verifier) checkSlices(f *os.File, fd *FileDesc, fr *FileResult) error {
	ifsc := v.set.IFSCs[fd.FileID]
	if ifsc == nil || uint64(len(ifsc.Slices)) != fr.TotalSlices {
		if fr.TotalSlices == 0 {
			fr.State = StateDamaged // an empty file that is not empty
		} else {
			markAll(fr, StateUnverifiable)
		}
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	fr.State = StateDamaged
	size := uint64(fr.Size)
	sliceSize := v.set.SliceSize
	for i := range ifsc.Slices {
		start := uint64(i) * sliceSize
		end := min(start+sliceSize, fd.Length)
		if end > size { // truncated: this slice and all after it are gone
			for j := i; j < len(ifsc.Slices); j++ {
				fr.DamagedSlices = append(fr.DamagedSlices, j)
			}
			break
		}
		ok, err := v.sliceMatches(f, end-start, ifsc.Slices[i])
		if err != nil {
			return err
		}
		if !ok {
			fr.DamagedSlices = append(fr.DamagedSlices, i)
		}
	}
	fr.DamagedCount = uint64(len(fr.DamagedSlices))
	return nil
}

// sliceMatches hashes the next dataLen bytes of f, zero-padded to the slice
// size, and compares them with want.
func (v *verifier) sliceMatches(f *os.File, dataLen uint64, want SliceChecksum) (bool, error) {
	h := md5.New()
	crc := uint32(0)
	feed := func(p []byte) {
		h.Write(p)
		crc = crc32.Update(crc, crc32.IEEETable, p)
	}
	for left := dataLen; left > 0; {
		if err := v.ctx.Err(); err != nil {
			return false, err
		}
		n, err := io.ReadFull(f, v.buf[:min(uint64(len(v.buf)), left)])
		feed(v.buf[:n])
		left -= uint64(n)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, nil // the file shrank under us
			}
			return false, err
		}
	}
	for pad := v.set.SliceSize - dataLen; pad > 0; {
		if err := v.ctx.Err(); err != nil {
			return false, err
		}
		n := min(uint64(len(v.zeros)), pad)
		feed(v.zeros[:n])
		pad -= n
	}
	var sum [16]byte
	h.Sum(sum[:0])
	return sum == want.MD5 && crc == want.CRC32, nil
}

// fullMD5 hashes at most limit bytes of f.
func (v *verifier) fullMD5(f *os.File, limit uint64) ([16]byte, error) {
	h := md5.New()
	var sum [16]byte
	for read := uint64(0); read < limit; {
		if err := v.ctx.Err(); err != nil {
			return sum, err
		}
		want := min(uint64(len(v.buf)), limit-read)
		n, err := io.ReadFull(f, v.buf[:want])
		h.Write(v.buf[:n])
		read += uint64(n)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return sum, err
		}
	}
	h.Sum(sum[:0])
	return sum, nil
}

// hashHead computes the MD5 of the first 16 KiB of c.
func (v *verifier) hashHead(c *candidate) error {
	if err := v.ctx.Err(); err != nil {
		return err
	}
	f, _, err := v.openRegular(c.name)
	if err != nil {
		return nil // vanished or swapped: not a candidate any more
	}
	defer f.Close()
	n, err := io.ReadFull(f, v.buf[:head16k])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	c.h16 = md5.Sum(v.buf[:n])
	c.hasHead = true
	return nil
}

// hashFull computes the MD5 of the whole candidate once.
func (v *verifier) hashFull(c *candidate) error {
	if c.hasFull {
		return nil
	}
	f, _, err := v.openRegular(c.name)
	if err != nil {
		return nil
	}
	defer f.Close()
	sum, err := v.fullMD5(f, c.size)
	if err != nil {
		return err
	}
	c.full, c.hasFull = sum, true
	return nil
}

// rename moves the file called from to the name want, never overwriting
// anything, and returns the name the file has on disk afterwards (still from
// on a dry run, which only records the intended rename). A failed rename is a warning: the file is then verified under
// its old name.
func (v *verifier) rename(from, want string, fr *FileResult) string {
	to, err := v.freeName(from, want)
	if err == nil && !v.cfg.dryRun {
		var src, dst string
		if src, err = fsutil.SafeJoin(v.dir, from); err == nil {
			if dst, err = fsutil.SafeJoin(v.dir, to); err == nil {
				err = os.Rename(src, dst)
			}
		}
	}
	if err != nil {
		v.res.Warnings = append(v.res.Warnings, fmt.Sprintf("cannot rename %s to %s: %s",
			strconv.QuoteToASCII(from), strconv.QuoteToASCII(want), strconv.QuoteToASCII(err.Error())))
		return from
	}
	v.taken[to] = true
	fr.Name = to
	fr.Renamed = &Rename{From: from, To: to}
	if v.cfg.dryRun {
		return from
	}
	return to
}

// freeName picks the final name for a file called from: want itself when it
// is free (or is the same file with different letter case), else the first
// free variant from fsutil.Unique.
func (v *verifier) freeName(from, want string) (string, error) {
	path, err := fsutil.SafeJoin(v.dir, want)
	if err != nil {
		return "", err
	}
	if fi, err := os.Lstat(path); err == nil && strings.EqualFold(from, want) {
		src, err := fsutil.SafeJoin(v.dir, from)
		if err != nil {
			return "", err
		}
		if si, err := os.Lstat(src); err == nil && os.SameFile(si, fi) {
			return want, nil
		}
	}
	cand := fsutil.Unique(path)
	for i := 1; v.taken[filepath.Base(cand)]; i++ {
		alt, err := fsutil.SafeName(fmt.Sprintf("%d_%s", i, want))
		if err != nil {
			return "", err
		}
		p, err := fsutil.SafeJoin(v.dir, alt)
		if err != nil {
			return "", err
		}
		cand = fsutil.Unique(p)
	}
	return filepath.Base(cand), nil
}
