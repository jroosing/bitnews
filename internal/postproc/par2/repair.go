// Repair is described here; the package overview is in doc.go.
//
// # Repair
//
// Repair rebuilds the damaged slices that Verify found, in place, from the
// recovery slices of the set, using the GF(2^16) Reed-Solomon decoder of
// package gf16.
//
// Input slices are numbered globally: file 0's slices first, then file 1's,
// in Set.FileIDs order, each file's last slice zero-padded to the slice size.
// With k damaged slices (a missing or unverifiable file counts all of its
// slices) Repair picks k recovery slices, lowest exponents first, builds the
// decoder and, if the matrix is singular, slides the window one exponent up
// (at most maxExponentAttempts times) before giving up with
// ErrSingularRecovery. Recovery slices that cannot be used (wrong length, or
// past the end of the file they live in) are dropped before counting.
//
// Nothing on disk changes until feasibility, the exponent choice and the
// memory budget are settled, so InsufficientError, ErrSingularRecovery,
// ErrTooLarge and the other planning errors leave the directory untouched.
//
// # Memory
//
// Work proceeds chunk by chunk: for each byte range [off, off+chunk) of the
// slice size it reads that range of every good input slice and of each chosen
// recovery slice, feeds them to the decoder and writes the k reconstructed
// ranges into the damaged slices. Memory is therefore (k+1) × chunk for the
// output and read buffers, plus about 6·k² bytes for the decoder matrix, and
// does not depend on file or slice size. The default chunk is 1 MiB; when the
// estimate exceeds WithMemoryLimit (256 MiB by default) the chunk is halved
// down to 4 KiB, then Repair fails with ErrTooLarge. Large k therefore means
// either small chunks (slower, more per-call decoder overhead) or ErrTooLarge:
// k = 100 needs about 100 MiB at 1 MiB chunks.
//
// # Safety
//
// Only the byte ranges of damaged slices are ever written, and only with the
// reconstructed data, so good slices are never overwritten and an interrupted
// or failed run leaves every file no worse than it was: damaged slices stay
// damaged. Files created for missing descriptions are removed again when the
// run fails. Wrong-length files are truncated or extended to the description's
// length only at the end, after every read. Each touched file is fsynced.
// Names of missing files go through fsutil.SafeName and fsutil.SafeJoin, files
// are opened without following symlinks and must be regular files, and a
// path in the Verify result that is not directly in dir is refused.
//
// After the write phase Repair runs Verify again; anything still damaged is a
// *RemainingDamageError.
//
// # Benchmarks
//
// BenchmarkRepair (repair_big_test.go), Apple M4 (10 cores), go1.27.2,
// darwin/arm64, shared machine: a 64 MiB set (4 files of 16 MiB, 256 KiB
// slices, 8 recovery slices on disk) with 3 of 256 slices damaged (1.2%):
//
//	BenchmarkRepair   108 ms/op  (620 MB/s of set)   of which the closing Verify ~78 ms
//
// so reconstruction itself (reading every good slice once, k = 3 multiply-
// accumulates per input byte, writing 3 slices) takes about 30 ms, 2 GB/s of
// set. The closing Verify (MD5 of every file) dominates at this damage level.
// TestRepair_PeakMemoryIsIndependentOfFileSize repairs a 512 MiB sparse set and
// measures a heap growth of well under 1 MiB (64 KiB chunks, k = 3).
package par2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

var (
	// ErrInsufficientRecovery means the set has fewer usable recovery slices
	// than damaged slices. Repair returns an *InsufficientError wrapping it.
	ErrInsufficientRecovery = errors.New("par2: not enough recovery slices")
	// ErrSingularRecovery means no tried choice of recovery slices could
	// recover the damaged slices.
	ErrSingularRecovery = errors.New("par2: recovery matrix is singular for every tried choice of recovery slices")
	// ErrTooLarge means the repair does not fit the memory limit even with
	// the smallest chunk size.
	ErrTooLarge = errors.New("par2: repair does not fit the memory limit")
	// ErrRepairFailed means the files still do not match after repair.
	// Repair returns a *RemainingDamageError wrapping it.
	ErrRepairFailed = errors.New("par2: damage remains after repair")
	// ErrInvalidResult means the Verify result does not describe the set.
	ErrInvalidResult = errors.New("par2: verify result does not match the set")
	// ErrIncompleteSet means a file of the recovery set has no description,
	// so the slice numbering (and with it the repair) is unknown.
	ErrIncompleteSet = errors.New("par2: file of the recovery set has no description")
	// ErrTooManySlices means the set has more input slices than PAR2 allows.
	ErrTooManySlices = errors.New("par2: too many input slices")
)

// InsufficientError reports how many recovery slices a repair needs and how
// many the set has, so the caller can fetch deferred volumes.
type InsufficientError struct {
	Needed, Available int
}

func (e *InsufficientError) Error() string {
	return fmt.Sprintf("par2: repair needs %d recovery slices, %d available", e.Needed, e.Available)
}

// Is makes errors.Is(err, ErrInsufficientRecovery) true.
func (e *InsufficientError) Is(target error) bool { return target == ErrInsufficientRecovery }

// RemainingDamageError is returned when Verify after the repair still finds
// damage. Result is that second Verify result.
type RemainingDamageError struct {
	Result *Result
}

func (e *RemainingDamageError) Error() string {
	return fmt.Sprintf("par2: %d damaged slice(s) remain after repair", e.Result.DamagedSlices)
}

// Is makes errors.Is(err, ErrRepairFailed) true.
func (e *RemainingDamageError) Is(target error) bool { return target == ErrRepairFailed }

// RepairResult is the outcome of a successful Repair.
type RepairResult struct {
	// RepairedSlices is the number of slices reconstructed.
	RepairedSlices int
	// FilesTouched lists the paths written, created or resized, in set order.
	FilesTouched []string
	// FilesCreated lists files that did not exist and were created.
	FilesCreated []string
	// FilesTruncated lists files that were cut to their description's length.
	FilesTruncated []string
	// Verify is the Result of the verification after the repair (the input
	// Result when there was nothing to do).
	Verify *Result
}

// RepairOption configures Repair.
type RepairOption func(*repairConfig)

type repairConfig struct {
	chunk      int
	memLimit   int64
	afterChunk func(index int)
}

// Repair defaults and limits.
const (
	defaultRepairChunk = 1 << 20
	minRepairChunk     = 4 << 10
	// DefaultMemoryLimit is the memory budget of Repair unless overridden.
	DefaultMemoryLimit = 256 << 20
	// maxExponentAttempts bounds how many recovery-slice windows are tried
	// when the decoder matrix is singular.
	maxExponentAttempts = 16
	// maxOpenFiles bounds the file handles Repair keeps open.
	maxOpenFiles = 64
)

// WithMemoryLimit sets the memory budget in bytes (default DefaultMemoryLimit).
// Repair shrinks its chunk size to fit and fails with ErrTooLarge when even
// 4 KiB chunks do not fit. Values below 1 are ignored.
func WithMemoryLimit(bytes int64) RepairOption {
	return func(c *repairConfig) {
		if bytes > 0 {
			c.memLimit = bytes
		}
	}
}

// WithChunkSize sets the preferred chunk size in bytes (default 1 MiB). It is
// rounded down to a multiple of 4 and still shrinks to fit the memory limit.
// Values below 4 are ignored.
func WithChunkSize(bytes int) RepairOption {
	return func(c *repairConfig) {
		if bytes >= 4 {
			c.chunk = bytes
		}
	}
}

// repairFile is one file of the recovery set.
type repairFile struct {
	fd      *FileDesc
	fr      *FileResult
	first   int   // global index of the file's first slice
	slices  int   // number of slices
	all     bool  // every slice is damaged (missing or unverifiable file)
	damaged []int // local indices of damaged slices (not filled when all)
	path    string
	create  bool // the file does not exist and must be created
	resize  bool // length may differ from the description: fix at the end
	touched bool
}

func (rf *repairFile) damagedCount() uint64 {
	if rf.all {
		return rf.fr.TotalSlices
	}
	return uint64(len(rf.damaged))
}

// Repair reconstructs the damaged slices that vr (the Result of Verify for
// dir and set) lists, then verifies again.
//
// It returns an *InsufficientError (ErrInsufficientRecovery) without touching
// anything when the set has fewer usable recovery slices than damaged slices.
// See the package notes above for memory, safety and ordering.
func Repair(ctx context.Context, dir string, set *Set, vr *Result, opts ...RepairOption) (*RepairResult, error) {
	cfg := repairConfig{chunk: defaultRepairChunk, memLimit: DefaultMemoryLimit}
	for _, o := range opts {
		o(&cfg)
	}
	if set == nil || vr == nil {
		return nil, ErrInvalidResult
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

	files, needed, incomplete, err := planFiles(set, vr)
	if err != nil {
		return nil, err
	}
	usable := usableRecovery(set)
	if needed > uint64(len(usable)) {
		return nil, &InsufficientError{Needed: satInt(needed), Available: len(usable)}
	}
	k := int(needed)

	r := &repairer{ctx: ctx, dir: dir, set: set, cfg: cfg, files: files, k: k, cache: &fileCache{}}
	defer r.cache.close() //nolint:errcheck // the success path closes and checks it
	if k > 0 {
		if incomplete {
			return nil, ErrIncompleteSet
		}
		if err := r.expand(); err != nil {
			return nil, err
		}
		if err := r.plan(usable); err != nil {
			return nil, err
		}
	}
	if err := r.resolvePaths(); err != nil {
		return nil, err
	}
	if !r.anyWork() {
		return &RepairResult{Verify: vr}, nil
	}
	if err := r.run(); err != nil {
		r.removeCreated()
		return nil, err
	}

	res, err := Verify(ctx, dir, set)
	if err != nil {
		return nil, err
	}
	if res.NeedsRepair() {
		return nil, &RemainingDamageError{Result: res}
	}
	out := r.result
	out.RepairedSlices = k
	out.Verify = res
	return &out, nil
}

func satInt(n uint64) int {
	if n > math.MaxInt {
		return math.MaxInt
	}
	return int(n)
}

// planFiles checks vr against the set and works out, without allocating per
// slice, what is damaged. needed is the total damaged slice count
// (saturating); incomplete says a file of the set has no description.
func planFiles(set *Set, vr *Result) (files []*repairFile, needed uint64, incomplete bool, err error) {
	byID := make(map[ID]*FileResult, len(vr.Files))
	for i := range vr.Files {
		fr := &vr.Files[i]
		if byID[fr.FileID] != nil {
			return nil, 0, false, fmt.Errorf("%w: file %s listed twice", ErrInvalidResult, fr.FileID)
		}
		byID[fr.FileID] = fr
	}
	for _, id := range set.FileIDs {
		fd := set.FileDescs[id]
		if fd == nil {
			incomplete = true
			continue
		}
		fr := byID[id]
		if fr == nil {
			return nil, 0, false, fmt.Errorf("%w: file %s has no result", ErrInvalidResult, id)
		}
		total := set.SliceCount(fd.Length)
		if fr.TotalSlices != total {
			return nil, 0, false, fmt.Errorf("%w: file %s has %d slices in the result, %d in the set", ErrInvalidResult, id, fr.TotalSlices, total)
		}
		rf := &repairFile{fd: fd, fr: fr}
		switch fr.State {
		case StateOK:
		case StateMissing, StateUnverifiable:
			rf.all = true
			rf.create = fr.State == StateMissing
			rf.resize = true
		case StateDamaged:
			rf.resize = true
			prev := -1
			for _, j := range fr.DamagedSlices {
				if j <= prev || uint64(j) >= total {
					return nil, 0, false, fmt.Errorf("%w: file %s: bad damaged slice list", ErrInvalidResult, id)
				}
				prev = j
			}
			if uint64(len(fr.DamagedSlices)) != fr.DamagedCount {
				return nil, 0, false, fmt.Errorf("%w: file %s: damaged count does not match the list", ErrInvalidResult, id)
			}
			rf.damaged = fr.DamagedSlices
		default:
			return nil, 0, false, fmt.Errorf("%w: file %s: unknown state", ErrInvalidResult, id)
		}
		needed = satAdd(needed, rf.damagedCount())
		files = append(files, rf)
	}
	return files, needed, incomplete, nil
}

// usableRecovery returns the exponents of the recovery slices that can really
// be read, ascending.
func usableRecovery(set *Set) []uint32 {
	sizes := map[string]int64{}
	var out []uint32
	for e, rs := range set.Recovery {
		size, seen := sizes[rs.Path]
		if !seen {
			size = -1
			if fi, err := os.Stat(rs.Path); err == nil && fi.Mode().IsRegular() {
				size = fi.Size()
			}
			sizes[rs.Path] = size
		}
		if rs.Length == int64(set.SliceSize) && rs.Offset >= 0 && rs.Offset <= size && size-rs.Offset >= rs.Length {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	return out
}

type repairer struct {
	ctx   context.Context
	dir   string
	set   *Set
	cfg   repairConfig
	files []*repairFile
	k     int
	cache *fileCache

	// Filled by expand.
	inputCount int
	owner      []int32 // per global slice, the index into files
	isDamaged  []bool
	damaged    []int // global indices, ascending

	// Filled by plan.
	chunk     int64
	exponents []uint32
	dec       *gf16.Decoder

	created []string
	result  RepairResult
}

// expand numbers the slices globally. It runs only once the damage is known
// to be coverable, so the sizes involved are bounded by the recovery slices
// actually on disk.
func (r *repairer) expand() error {
	var total uint64
	for _, rf := range r.files {
		total = satAdd(total, rf.fr.TotalSlices)
	}
	if total > gf16.MaxInputSlices {
		return fmt.Errorf("%w: %d", ErrTooManySlices, total)
	}
	r.inputCount = int(total)
	r.owner = make([]int32, 0, r.inputCount)
	r.isDamaged = make([]bool, r.inputCount)
	for i, rf := range r.files {
		rf.first = len(r.owner)
		rf.slices = int(rf.fr.TotalSlices)
		for range rf.slices {
			r.owner = append(r.owner, int32(i))
		}
		if rf.all {
			for j := range rf.slices {
				r.damaged = append(r.damaged, rf.first+j)
			}
		} else {
			for _, j := range rf.damaged {
				r.damaged = append(r.damaged, rf.first+j)
			}
		}
	}
	for _, g := range r.damaged {
		r.isDamaged[g] = true
	}
	return nil
}

// plan fixes the chunk size and builds the decoder, trying other recovery
// slices when the matrix is singular.
func (r *repairer) plan(usable []uint32) error {
	sliceSize := int64(r.set.SliceSize)
	k := int64(r.k)
	fixed := 6 * k * k
	chunk := min(int64(r.cfg.chunk), sliceSize) &^ 3
	floor := min(int64(minRepairChunk), sliceSize)
	for fixed+(k+1)*chunk > r.cfg.memLimit && chunk > floor {
		chunk = max(chunk/2&^3, floor)
	}
	if fixed+(k+1)*chunk > r.cfg.memLimit {
		return fmt.Errorf("%w: %d damaged slices need about %d bytes, limit %d", ErrTooLarge, k, fixed+(k+1)*chunk, r.cfg.memLimit)
	}
	r.chunk = chunk

	for s := 0; s < maxExponentAttempts && s+r.k <= len(usable); s++ {
		exps := usable[s : s+r.k]
		dec, err := gf16.NewDecoder(r.inputCount, r.damaged, exps)
		if errors.Is(err, gf16.ErrSingular) {
			continue
		}
		if err != nil {
			return err
		}
		r.exponents, r.dec = exps, dec
		return nil
	}
	return ErrSingularRecovery
}

// resolvePaths decides where every file is, refusing anything outside dir.
func (r *repairer) resolvePaths() error {
	clean := filepath.Clean(r.dir)
	for _, rf := range r.files {
		if rf.fr.State == StateOK && r.k == 0 {
			continue // not needed
		}
		if rf.fd.Length > math.MaxInt64 {
			return fmt.Errorf("%w: file %s is too long", ErrInvalidResult, rf.fr.FileID)
		}
		if rf.create {
			p, err := fsutil.SafeJoin(r.dir, safeTargetName(rf.fd))
			if err != nil {
				return err
			}
			rf.path = p
			continue
		}
		if rf.fr.Path == "" {
			if rf.fr.State == StateOK {
				return fmt.Errorf("%w: file %s has no path", ErrInvalidResult, rf.fr.FileID)
			}
			continue
		}
		if filepath.Dir(filepath.Clean(rf.fr.Path)) != clean {
			return fmt.Errorf("%w: %q is not directly in the directory", fsutil.ErrUnsafePath, filepath.Base(rf.fr.Path))
		}
		p, err := fsutil.SafeJoin(r.dir, filepath.Base(rf.fr.Path))
		if err != nil {
			return err
		}
		rf.path = p
	}
	return nil
}

// anyWork reports whether anything has to be written.
func (r *repairer) anyWork() bool {
	for _, rf := range r.files {
		if rf.fr.State != StateOK {
			return true
		}
	}
	return false
}

func (r *repairer) run() error {
	for _, rf := range r.files {
		if rf.create {
			if err := r.createFile(rf); err != nil {
				return err
			}
		}
	}
	if r.k > 0 {
		if err := r.reconstruct(); err != nil {
			return err
		}
	}
	for _, rf := range r.files {
		if rf.resize {
			if err := r.fixLength(rf); err != nil {
				return err
			}
		}
	}
	if err := r.cache.close(); err != nil {
		return err
	}
	if len(r.created) > 0 {
		syncDir(r.dir)
	}
	for _, rf := range r.files {
		if rf.touched {
			r.result.FilesTouched = append(r.result.FilesTouched, rf.path)
		}
	}
	return nil
}

// createFile makes an empty file of the description's length (sparse).
func (r *repairer) createFile(rf *repairFile) error {
	f, err := os.OpenFile(rf.path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("par2: create %q: %w", filepath.Base(rf.path), err)
	}
	r.created = append(r.created, rf.path)
	rf.touched = true
	r.result.FilesCreated = append(r.result.FilesCreated, rf.path)
	if err := f.Truncate(int64(rf.fd.Length)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// removeCreated undoes createFile after a failure.
func (r *repairer) removeCreated() {
	r.cache.close() //nolint:errcheck // already failing
	for _, p := range r.created {
		os.Remove(p)
	}
}

// fixLength truncates or extends the file to the description's length.
func (r *repairer) fixLength(rf *repairFile) error {
	if rf.path == "" {
		return nil
	}
	f, err := r.cache.get(rf.path, true)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	want := int64(rf.fd.Length)
	if fi.Size() == want {
		return nil
	}
	if err := f.Truncate(want); err != nil {
		return err
	}
	rf.touched = true
	if fi.Size() > want {
		r.result.FilesTruncated = append(r.result.FilesTruncated, rf.path)
	}
	return nil
}

// reconstruct computes every damaged slice chunk by chunk and writes it.
func (r *repairer) reconstruct() error {
	sliceSize := int64(r.set.SliceSize)
	bufs := make([][]byte, r.k)
	out := make([][]byte, r.k)
	for m := range bufs {
		bufs[m] = make([]byte, r.chunk)
	}
	rbuf := make([]byte, r.chunk)

	index := 0
	for off := int64(0); off < sliceSize; off += r.chunk {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		n := min(r.chunk, sliceSize-off)
		for m := range out {
			out[m] = bufs[m][:n]
			clear(out[m])
		}
		for i, e := range r.exponents {
			rs := r.set.Recovery[e]
			f, err := r.cache.get(rs.Path, false)
			if err != nil {
				return err
			}
			if _, err := f.ReadAt(rbuf[:n], rs.Offset+off); err != nil {
				return fmt.Errorf("par2: read recovery slice %d: %w", e, err)
			}
			if err := r.dec.AddRecovery(out, i, rbuf[:n]); err != nil {
				return err
			}
		}
		for g := range r.inputCount {
			if r.isDamaged[g] {
				continue
			}
			if g%256 == 0 {
				if err := r.ctx.Err(); err != nil {
					return err
				}
			}
			rf := r.files[r.owner[g]]
			pos := int64(g-rf.first)*sliceSize + off
			if pos >= int64(rf.fd.Length) {
				continue // this chunk lies in the zero padding
			}
			cnt := min(n, int64(rf.fd.Length)-pos)
			f, err := r.cache.get(rf.path, false)
			if err != nil {
				return err
			}
			// A short read is zero padding: Verify vouched for this slice.
			if _, err := f.ReadAt(rbuf[:cnt], pos); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if err := r.dec.AddInput(out, g, rbuf[:cnt]); err != nil {
				return err
			}
		}
		if err := r.ctx.Err(); err != nil {
			return err
		}
		for m, g := range r.damaged {
			rf := r.files[r.owner[g]]
			pos := int64(g-rf.first)*sliceSize + off
			if pos >= int64(rf.fd.Length) {
				continue
			}
			cnt := min(n, int64(rf.fd.Length)-pos)
			f, err := r.cache.get(rf.path, true)
			if err != nil {
				return err
			}
			if _, err := f.WriteAt(out[m][:cnt], pos); err != nil {
				return err
			}
			rf.touched = true
		}
		if r.cfg.afterChunk != nil {
			r.cfg.afterChunk(index)
		}
		index++
	}
	return nil
}

// fileCache keeps a bounded number of files open. Files opened for writing
// are synced before they are closed. Files must be regular and are opened
// without following a symlink in the last component.
type fileCache struct {
	m     map[string]*cachedFile
	order []string
}

type cachedFile struct {
	f     *os.File
	rw    bool
	dirty bool
}

func (c *fileCache) get(path string, write bool) (*os.File, error) {
	if c.m == nil {
		c.m = map[string]*cachedFile{}
	}
	if e := c.m[path]; e != nil && (e.rw || !write) {
		e.dirty = e.dirty || write
		return e.f, nil
	} else if e != nil {
		if err := c.drop(path); err != nil {
			return nil, err
		}
	}
	for len(c.order) >= maxOpenFiles {
		if err := c.drop(c.order[0]); err != nil {
			return nil, err
		}
	}
	li, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, fmt.Errorf("par2: %q is not a regular file", filepath.Base(path))
	}
	flag := os.O_RDONLY
	if write {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !os.SameFile(li, fi) {
		f.Close()
		return nil, fmt.Errorf("par2: %q changed while opening", filepath.Base(path))
	}
	c.m[path] = &cachedFile{f: f, rw: write, dirty: write}
	c.order = append(c.order, path)
	return f, nil
}

// drop syncs (when written to) and closes one file.
func (c *fileCache) drop(path string) error {
	e := c.m[path]
	if e == nil {
		return nil
	}
	delete(c.m, path)
	c.order = slices.DeleteFunc(c.order, func(p string) bool { return p == path })
	var err error
	if e.dirty {
		err = e.f.Sync()
	}
	if cerr := e.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// close drops every file, returning the first error.
func (c *fileCache) close() error {
	var first error
	for len(c.order) > 0 {
		if err := c.drop(c.order[0]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// syncDir flushes a directory entry change; best effort, as not every
// platform can fsync a directory.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync() //nolint:errcheck // best effort
		d.Close()
	}
}
