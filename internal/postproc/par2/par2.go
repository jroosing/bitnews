package par2

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/jroosing/bitnews/internal/fsutil"
	"github.com/jroosing/bitnews/internal/postproc/tool"
)

// Defaults for Tool.
const (
	// DefaultMemoryMB is what par2 may use for repair buffers. par2's own
	// default is half of the physical memory, which is wrong in a container
	// whose limit it cannot see. More memory makes a repair of a big set
	// need fewer passes over the data; par2 works with as little as 1 MB.
	DefaultMemoryMB = 512
	// maxThreads caps the default thread count.
	maxThreads = 16
	// outputTail is how much of par2's output an error message may carry.
	outputTail = 400
)

// par2 exit codes (par2cmdline-turbo 1.5.0; `par2 -h` does not list them).
const (
	exitOK                = 0
	exitRepairRequired    = 1 // verify: damaged, and repairable
	exitRepairImpossible  = 2 // not enough recovery blocks
	exitInvalidArgs       = 3
	exitInsufficientCrit  = 4 // no usable main packet / critical packets
	exitRepairFailed      = 5
	exitFileIO            = 6
	exitLogic             = 7
	exitMemory            = 8
	exitSignal            = -1 // killed (the runner reports -1)
	maxBackupSuffixDigits = 4
)

// ErrRepairFailed: par2 ran the repair and could not produce correct files.
var ErrRepairFailed = errors.New("par2: repair failed")

// ErrNoSet is returned by Repair when the directory holds no usable PAR2 set.
var ErrNoSet = errors.New("par2: no usable PAR2 set")

// InsufficientError is returned by Repair when the recovery blocks on disk do
// not cover the damaged data blocks.
type InsufficientError struct {
	// Needed is the number of recovery blocks the damage requires.
	Needed int
	// Available is the number of recovery blocks on disk.
	Available int
}

func (e *InsufficientError) Error() string {
	return fmt.Sprintf("par2: need %d recovery blocks, have %d", e.Needed, e.Available)
}

// Report is what Verify found.
type Report struct {
	// HasSet says dir holds a usable PAR2 set. When false nothing else is
	// meaningful.
	HasSet bool
	// Clean says every file matches the set (after any renames).
	Clean bool
	// NeedsRepair says data blocks are damaged or missing.
	NeedsRepair bool
	// TotalBlocks is the number of data blocks of the set.
	TotalBlocks int
	// DamagedBlocks is the number of data blocks that are missing or damaged
	// (and not found in another file): the recovery blocks a repair needs.
	DamagedBlocks int
	// RecoveryBlocks is the number of recovery blocks par2 found on disk.
	RecoveryBlocks int
	// Repairable is par2's verdict: RecoveryBlocks cover DamagedBlocks.
	Repairable bool
	// Renamed is how many files Verify renamed to their PAR2 names.
	Renamed int
}

// RepairResult is what Repair did.
type RepairResult struct {
	// RepairedBlocks is the number of data blocks rebuilt.
	RepairedBlocks int
}

// Tool runs the par2 program (par2cmdline-turbo).
//
// The zero value works: par2 is looked up on PATH at each call.
type Tool struct {
	// Path is the par2 program; empty means tool.Locate("par2").
	Path string
	// Threads is par2's -t. Zero means GOMAXPROCS (container aware), at
	// most 16.
	Threads int
	// MemoryMB is par2's -m. Zero means DefaultMemoryMB.
	MemoryMB int
}

func (t Tool) path() (string, error) {
	if t.Path != "" {
		return t.Path, nil
	}
	return tool.Locate("par2")
}

func (t Tool) threads() int {
	if t.Threads > 0 {
		return t.Threads
	}
	return min(max(runtime.GOMAXPROCS(0), 1), maxThreads)
}

func (t Tool) memoryMB() int {
	if t.MemoryMB > 0 {
		return t.MemoryMB
	}
	return DefaultMemoryMB
}

// setInfo is what we learn about a directory before par2 is started.
type setInfo struct {
	dir   string   // absolute
	main  string   // the .par2 file given to par2
	extra []string // everything else par2 may read: other .par2 files and non-target files
	names []string // target names from the File Description packets
}

// inspect lists dir (not its subdirectories), reads the file names out of
// every .par2 file and checks them. It returns nil when there is no set.
func inspect(dir string) (*setInfo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	type pf struct {
		name string
		size int64
	}
	var pars []pf
	var others []string
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue // symlinks, directories and special files are never given to par2
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
			fi, err := e.Info()
			if err != nil {
				return nil, err
			}
			pars = append(pars, pf{e.Name(), fi.Size()})
		} else {
			others = append(others, e.Name())
		}
	}
	if len(pars) == 0 {
		return nil, nil
	}

	targets := map[string]bool{}
	var names []string
	for _, p := range pars {
		ns, err := ReadNamesFile(filepath.Join(abs, p.name))
		if err != nil {
			return nil, fmt.Errorf("par2: read %q: %w", p.name, err)
		}
		for _, n := range ns {
			if !targets[n] {
				targets[n] = true
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	if err := CheckNames(abs, names); err != nil {
		return nil, err
	}

	// The main file is the smallest one: the index without recovery data.
	sort.Slice(pars, func(i, j int) bool {
		if pars[i].size != pars[j].size {
			return pars[i].size < pars[j].size
		}
		return pars[i].name < pars[j].name
	})
	si := &setInfo{dir: abs, main: filepath.Join(abs, pars[0].name), names: names}
	for _, p := range pars[1:] {
		si.extra = append(si.extra, filepath.Join(abs, p.name))
	}
	for _, n := range others {
		// A file that already has a PAR2 name is checked as that target.
		// The rest may be an obfuscated copy of one: par2 finds it by hash.
		// The list is read line by line and trimmed, so names it cannot
		// carry are left out.
		if !targets[n] && listable(n) {
			si.extra = append(si.extra, filepath.Join(abs, n))
		}
	}
	return si, nil
}

// listable reports whether a name survives a line-based, trimmed file list.
func listable(name string) bool {
	if name == "" || name != strings.TrimSpace(name) {
		return false
	}
	for i := range len(name) {
		if name[i] < 0x20 || name[i] == 0x7f {
			return false
		}
	}
	return true
}

// run starts par2 with the given mode ("v" or "r") and options.
func (t Tool) run(ctx context.Context, si *setInfo, mode string, renameOnly bool) (tool.Result, error) {
	p, err := t.path()
	if err != nil {
		return tool.Result{}, err
	}
	// -B keeps par2 to dir: it ignores files outside the basepath. Verify and
	// repair never use -N (block skipping), -p (purge: it would delete the
	// .par2 files) or -S.
	args := []string{mode, "-B" + si.dir, "-t" + strconv.Itoa(t.threads()), "-m" + strconv.Itoa(t.memoryMB())}
	if renameOnly {
		args = append(args, "-O")
	}
	// The extra files go through stdin (`@`), so a directory with many files
	// cannot overflow the argument list.
	args = append(args, "--", si.main, "@")
	return tool.Run(ctx, tool.Spec{
		Path:  p,
		Args:  args,
		Dir:   si.dir,
		Stdin: strings.NewReader(strings.Join(si.extra, "\n") + "\n"),
	})
}

// Verify checks the files in dir (top level only) against its PAR2 set.
//
// A directory without a .par2 file, or whose .par2 files have no usable main
// packet, yields Report{HasSet: false} and no error. A set with an unsafe
// file name is refused before par2 runs, with ErrUnsafeSet.
//
// Files that are not called what the set says but match one by content
// (obfuscated names) are found by par2 through its extra-file list and
// renamed to their set names with `par2 r -O`; Report.Renamed counts them. A
// file renamed over a damaged one leaves that one as name.1, which is removed
// again. Verify then runs again, so the report describes the directory after
// the renames. Leftover name.N backups of an interrupted repair are removed
// when the directory is clean.
func (t Tool) Verify(ctx context.Context, dir string) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	si, err := inspect(dir)
	if err != nil {
		return nil, err
	}
	if si == nil {
		return &Report{}, nil
	}
	rep, out, err := t.verifyOnce(ctx, si)
	if err != nil || !rep.HasSet {
		return rep, err
	}
	if out.renames > 0 {
		res, err := t.run(ctx, si, "r", true)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != exitOK {
			return nil, exitError("rename", res)
		}
		if err := removeBackups(si); err != nil {
			return nil, err
		}
		// Renamed files are now targets and no longer extras; list again.
		if si, err = inspect(dir); err != nil || si == nil {
			return nil, errors.Join(err, ErrNoSet)
		}
		renamed := out.renames
		if rep, _, err = t.verifyOnce(ctx, si); err != nil {
			return nil, err
		}
		rep.Renamed = renamed
	}
	if rep.Clean {
		if err := removeBackups(si); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// verifyOnce runs `par2 v` and turns its exit code and text into a Report.
func (t Tool) verifyOnce(ctx context.Context, si *setInfo) (*Report, output, error) {
	res, err := t.run(ctx, si, "v", false)
	if err != nil {
		return nil, output{}, err
	}
	out := parseOutput(res.Stdout)
	rep := &Report{HasSet: true, TotalBlocks: out.total, RecoveryBlocks: out.recoveryBlocks()}
	switch res.ExitCode {
	case exitOK:
		rep.Clean = true
		rep.Repairable = true
	case exitRepairRequired, exitRepairImpossible:
		if !out.haveAvail && out.renames == 0 {
			return nil, out, exitError("verify (unreadable report)", res)
		}
		rep.DamagedBlocks = out.damaged()
		rep.NeedsRepair = true
		// "Repair is not possible" comes with exit code 2. par2 says
		// "possible" for exit code 1.
		rep.Repairable = res.ExitCode == exitRepairRequired && !out.impossible
		if rep.DamagedBlocks == 0 && out.renames > 0 {
			// Only misnamed files: the rename pass fixes them.
			rep.Repairable = true
		}
	case exitInsufficientCrit:
		return &Report{}, out, nil // "Main packet not found": no usable set
	default:
		return nil, out, exitError("verify", res)
	}
	return rep, out, nil
}

// Repair repairs the files in dir with par2 (`par2 r`), which verifies first,
// renames matching files, rebuilds damaged and missing files from the
// recovery blocks and checks the result. The damaged originals that par2
// keeps as name.1, name.2, ... are removed afterwards.
//
// Errors: *InsufficientError when the recovery blocks on disk do not cover the
// damage (par2 changes nothing then); ErrRepairFailed when par2 failed to
// produce correct files; ErrUnsafeSet for a hostile set (refused before par2
// runs); ErrNoSet; the context's error when it is cancelled (the process
// group is killed). A cancelled repair may leave name.N files and half-written
// targets; calling Verify and Repair again picks them up as data sources.
func (t Tool) Repair(ctx context.Context, dir string) (RepairResult, error) {
	if err := ctx.Err(); err != nil {
		return RepairResult{}, err
	}
	si, err := inspect(dir)
	if err != nil {
		return RepairResult{}, err
	}
	if si == nil {
		return RepairResult{}, ErrNoSet
	}
	res, err := t.run(ctx, si, "r", false)
	if err != nil {
		return RepairResult{}, err
	}
	out := parseOutput(res.Stdout)
	switch res.ExitCode {
	case exitOK:
		if err := removeBackups(si); err != nil {
			return RepairResult{}, err
		}
		if err := removePartials(si, out.partials); err != nil {
			return RepairResult{}, err
		}
		return RepairResult{RepairedBlocks: out.willUse}, nil
	case exitRepairImpossible:
		short := &InsufficientError{Needed: out.damaged(), Available: out.recoveryBlocks()}
		if !out.haveAvail {
			short.Needed = out.recoveryBlocks() + out.needMore
		}
		return RepairResult{}, short
	case exitRepairFailed:
		return RepairResult{}, fmt.Errorf("%w: %w", ErrRepairFailed, exitError("repair", res))
	case exitInsufficientCrit:
		return RepairResult{}, fmt.Errorf("%w: %w", ErrNoSet, exitError("repair", res))
	}
	return RepairResult{}, exitError("repair", res)
}

// exitError describes an unexpected par2 exit with the tail of its output.
func exitError(what string, res tool.Result) error {
	tail := res.Stdout
	if len(res.Stderr) > 0 {
		tail = append(append([]byte(nil), tail...), res.Stderr...)
	}
	if len(tail) > outputTail {
		tail = tail[len(tail)-outputTail:]
	}
	return fmt.Errorf("par2: %s: exit status %d (%s): %s", what, res.ExitCode, exitName(res.ExitCode), printable(tail))
}

func exitName(code int) string {
	switch code {
	case exitRepairRequired:
		return "repair required"
	case exitRepairImpossible:
		return "repair not possible"
	case exitInvalidArgs:
		return "invalid arguments"
	case exitInsufficientCrit:
		return "insufficient critical data"
	case exitRepairFailed:
		return "repair failed"
	case exitFileIO:
		return "file I/O error"
	case exitLogic:
		return "logic error"
	case exitMemory:
		return "out of memory"
	case exitSignal:
		return "killed"
	}
	return "unknown"
}

// printable keeps b as one line of text: progress lines (\r) and line breaks
// become " | ", other control bytes are dropped.
func printable(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c == '\r' || c == '\n':
			sb.WriteString(" | ")
		case c < 0x20 || c == 0x7f:
		default:
			sb.WriteByte(c)
		}
	}
	return strings.TrimSpace(sb.String())
}

var backupRE = regexp.MustCompile(`^(.+)\.(\d{1,` + strconv.Itoa(maxBackupSuffixDigits) + `})$`)

// removePartials deletes the damaged copies of repaired targets that par2
// found under other names (an obfuscated and damaged file: the repair writes
// the target under its set name and leaves the copy). Only names from par2's
// report that are exactly files we gave it as extras are touched, and only
// regular files.
func removePartials(si *setInfo, names []string) error {
	extra := make(map[string]bool, len(si.extra))
	for _, p := range si.extra {
		extra[p] = true
	}
	for _, n := range names {
		p := filepath.Join(si.dir, n)
		if !extra[p] || strings.EqualFold(filepath.Ext(n), ".par2") {
			continue
		}
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// removeBackups deletes the name.N files par2 leaves when it replaces a
// damaged target: regular files next to a target whose name is a target name
// plus ".N" and that are not targets themselves. They are old copies of data
// that has been repaired; left in place they would be moved into the library.
func removeBackups(si *setInfo) error {
	targets := map[string]bool{}
	dirs := map[string]bool{}
	for _, n := range si.names {
		targets[n] = true
		dirs[path.Dir(n)] = true
	}
	for rel := range dirs {
		p, err := fsutil.SafeJoin(si.dir, rel)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrUnsafeSet, err)
		}
		ents, err := os.ReadDir(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range ents {
			m := backupRE.FindStringSubmatch(e.Name())
			if m == nil || !e.Type().IsRegular() {
				continue
			}
			full := path.Join(rel, e.Name())
			if targets[full] || !targets[path.Join(rel, m[1])] {
				continue
			}
			if err := os.Remove(filepath.Join(p, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
