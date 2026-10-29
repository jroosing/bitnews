package par2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/jroosing/bitnews/internal/core"
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
	exitRepairRequired    = 1 // verify only: damaged, and repairable
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

// Repairer implements core.Repairer with one run of `par2 r` (ADR-0007),
// which verifies the files, renames obfuscated ones to their PAR2 names and
// rebuilds damaged and missing ones from the recovery blocks.
//
// The zero value works: par2 is looked up on PATH at each call.
type Repairer struct {
	// Path is the par2 program; empty means tool.Locate("par2").
	Path string
	// Threads is par2's -t. Zero means GOMAXPROCS (container aware), at
	// most 16.
	Threads int
	// MemoryMB is par2's -m. Zero means DefaultMemoryMB.
	MemoryMB int
}

var _ core.Repairer = Repairer{}

func (t Repairer) path() (string, error) {
	if t.Path != "" {
		return t.Path, nil
	}
	return tool.Locate("par2")
}

func (t Repairer) threads() int {
	if t.Threads > 0 {
		return t.Threads
	}
	return min(max(runtime.GOMAXPROCS(0), 1), maxThreads)
}

func (t Repairer) memoryMB() int {
	if t.MemoryMB > 0 {
		return t.MemoryMB
	}
	return DefaultMemoryMB
}

// adoptPAR2Files gives a .par2 name to every file directly in dir that is a
// PAR2 file by content but not by name. Fully obfuscated posts name the PAR2
// files like the data (random hex, no extension); without the extension
// neither inspect nor par2 would know them, the data would keep its random
// names and the PAR2 files would be left in the output. The new name is the
// old one plus ".par2", made safe and unique; nothing is overwritten.
func adoptPAR2Files(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || strings.EqualFold(filepath.Ext(name), ".par2") || !startsWithPAR2(filepath.Join(dir, name)) {
			continue
		}
		safe, err := fsutil.SafeName(name + ".par2")
		if err != nil {
			continue
		}
		if err := os.Rename(filepath.Join(dir, name), fsutil.Unique(filepath.Join(dir, safe))); err != nil {
			return fmt.Errorf("par2: name PAR2 file %q: %w", name, err)
		}
	}
	return nil
}

// startsWithPAR2 reports whether the file at p begins with a PAR2 packet.
func startsWithPAR2(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(packetMagic))
	_, err = io.ReadFull(f, head)
	return err == nil && bytes.Equal(head, packetMagic)
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

// run starts `par2 r`.
func (t Repairer) run(ctx context.Context, si *setInfo) (tool.Result, error) {
	p, err := t.path()
	if err != nil {
		return tool.Result{}, err
	}
	// -B keeps par2 to dir: it ignores files outside the basepath. Never
	// used: -N (block skipping), -p (purge: it would delete the .par2 files)
	// or -S.
	args := []string{"r", "-B" + si.dir, "-t" + strconv.Itoa(t.threads()), "-m" + strconv.Itoa(t.memoryMB())}
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

// Repair checks the files in dir (top level, plus the subdirectories the set
// names) against its PAR2 set and repairs them in one run of `par2 r`, which
// verifies first, renames files that match a target by content (obfuscated
// names), rebuilds damaged and missing files from the recovery blocks and
// checks the result. The damaged originals par2 keeps as name.1, name.2, ...
// and the damaged copies under obfuscated names are removed afterwards, also
// when nothing needed repairing (the leftovers of an interrupted repair).
//
// PAR2 files are found by content too: one without a .par2 name (a fully
// obfuscated post) is given one first (adoptPAR2Files). A directory without a
// usable PAR2 set (no PAR2 file, or none with a main packet) is not an error:
// it yields HasPAR2 false.
//
// Errors: *core.RecoveryShortfall when the recovery blocks on disk do not
// cover the damage (par2 changes nothing then); core.ErrRepairFailed when
// par2 failed to produce correct files; core.ErrPathViolation (ErrUnsafeSet)
// for a hostile set, refused before par2 runs; the context's error when it is
// cancelled (the process group is killed). A cancelled repair may leave name.N
// files and half-written targets; the next Repair picks them up as data
// sources. Anything else, including par2 not being installed, is a plain
// error.
func (t Repairer) Repair(ctx context.Context, dir string) (core.RepairOutcome, error) {
	if err := ctx.Err(); err != nil {
		return core.RepairOutcome{}, err
	}
	if err := adoptPAR2Files(dir); err != nil {
		return core.RepairOutcome{}, err
	}
	si, err := inspect(dir)
	if err != nil || si == nil {
		return core.RepairOutcome{}, err
	}
	res, err := t.run(ctx, si)
	if err != nil {
		return core.RepairOutcome{}, err
	}
	out := parseOutput(res.Stdout)
	switch res.ExitCode {
	case exitOK:
		if err := removeBackups(si); err != nil {
			return core.RepairOutcome{}, err
		}
		if err := removePartials(si, out.partials); err != nil {
			return core.RepairOutcome{}, err
		}
		return core.RepairOutcome{HasPAR2: true, RepairedSlices: out.willUse}, nil
	case exitRepairImpossible:
		short := &core.RecoveryShortfall{Needed: out.damaged(), Available: out.recoveryBlocks()}
		if !out.haveAvail {
			short.Needed = out.recoveryBlocks() + out.needMore
		}
		return core.RepairOutcome{}, short
	case exitRepairFailed:
		return core.RepairOutcome{}, fmt.Errorf("%w: %w", core.ErrRepairFailed, exitError("repair", res))
	case exitInsufficientCrit:
		return core.RepairOutcome{}, nil // "Main packet not found": no usable set
	}
	return core.RepairOutcome{}, exitError("repair", res)
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
