// Command fixturegen supports scripts/gen-par2-fixtures.sh. It is a dev tool:
// the product never runs it, and nothing in the product imports it.
//
//	fixturegen payload -seed N -size N -out FILE
//	fixturegen manifest -case NAME -dir CASEDIR -args "..." -version V -slice-size N -recovery-blocks N
//
// payload writes SIZE pseudo-random bytes from a fixed seed (math/rand/v2
// PCG, whose output is stable across Go releases), so fixtures are
// reproducible.
//
// manifest writes CASEDIR/manifest.json from CASEDIR/data (the payload) and
// CASEDIR/par2 (what par2cmdline wrote). Everything in it is derived from the
// payload bytes, from par2cmdline's printed block size and recovery block
// count, and from the volume file names (volNN+MM.par2 holds exponents
// NN..NN+MM-1). It never reads a packet, so the conformance tests compare our
// parser with something independent of it. It also lists damage scenarios
// with their expected Verify and Repair outcome, computed from the slice
// arithmetic alone.
package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: fixturegen payload|manifest [flags]")
	}
	var err error
	switch os.Args[1] {
	case "payload":
		err = payload(os.Args[2:])
	case "manifest":
		err = manifest(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("fixturegen: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func payload(args []string) error {
	fs := flag.NewFlagSet("payload", flag.ExitOnError)
	seed := fs.Uint64("seed", 1, "PRNG seed")
	size := fs.Int("size", 0, "number of bytes")
	out := fs.String("out", "", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := rand.New(rand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15))
	buf := make([]byte, *size)
	for i := 0; i < len(buf); i += 8 {
		v := r.Uint64()
		for j := 0; j < 8 && i+j < len(buf); j++ {
			buf[i+j] = byte(v >> (8 * j))
		}
	}
	return os.WriteFile(*out, buf, 0o644)
}

// Manifest is manifest.json.
type Manifest struct {
	Case           string   `json:"case"`
	Par2cmdline    string   `json:"par2cmdline"`
	Par2Args       string   `json:"par2_args"`
	SliceSize      int      `json:"slice_size"`
	RecoveryBlocks int      `json:"recovery_blocks"`
	Exponents      []uint32 `json:"exponents"`
	TotalSlices    int      `json:"total_slices"`
	Files          []File   `json:"files"`
	Damage         []Damage `json:"damage"`
}

// File is one payload file. Empty files are skipped by par2cmdline and so
// are not part of the recovery set (InSet false).
type File struct {
	Name   string `json:"name"`
	Size   int    `json:"size"`
	MD5    string `json:"md5"`
	InSet  bool   `json:"in_set"`
	Slices int    `json:"slices"`
}

// Op is one step of a damage scenario.
type Op struct {
	Op     string `json:"op"` // flip, truncate, delete, rename, append
	File   string `json:"file"`
	Offset int    `json:"offset,omitempty"` // flip: XOR 0xFF at this offset
	Size   int    `json:"size,omitempty"`   // truncate: new size; append: number of bytes
	To     string `json:"to,omitempty"`     // rename: new name
}

// Expect is the expected Verify verdict for one file after the damage.
type Expect struct {
	State       string `json:"state"` // ok, damaged, missing
	Damaged     int    `json:"damaged"`
	Slices      []int  `json:"slices,omitempty"` // damaged slice indexes (state damaged)
	RenamedFrom string `json:"renamed_from,omitempty"`
}

// Damage is a scenario with its expected outcome.
type Damage struct {
	Name       string            `json:"name"`
	Ops        []Op              `json:"ops"`
	Needed     int               `json:"needed"`     // damaged slices in total
	Repairable bool              `json:"repairable"` // needed <= recovery_blocks
	Files      map[string]Expect `json:"files"`
}

var volRE = regexp.MustCompile(`\.vol(\d+)\+(\d+)\.par2$`)

func manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	name := fs.String("case", "", "case name")
	dir := fs.String("dir", "", "case directory with data/ and par2/")
	par2args := fs.String("args", "", "par2cmdline arguments used")
	version := fs.String("version", "", "par2cmdline version")
	slice := fs.Int("slice-size", 0, "block size printed by par2cmdline")
	recov := fs.Int("recovery-blocks", 0, "recovery block count printed by par2cmdline")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m := &Manifest{Case: *name, Par2cmdline: *version, Par2Args: *par2args, SliceSize: *slice, RecoveryBlocks: *recov}

	ents, err := os.ReadDir(filepath.Join(*dir, "data"))
	if err != nil {
		return err
	}
	for _, e := range ents { // sorted by name
		b, err := os.ReadFile(filepath.Join(*dir, "data", e.Name()))
		if err != nil {
			return err
		}
		sum := md5.Sum(b)
		f := File{Name: e.Name(), Size: len(b), MD5: hex.EncodeToString(sum[:]), InSet: len(b) > 0}
		if f.InSet {
			f.Slices = (len(b) + *slice - 1) / *slice
			m.TotalSlices += f.Slices
		}
		m.Files = append(m.Files, f)
	}

	pents, err := os.ReadDir(filepath.Join(*dir, "par2"))
	if err != nil {
		return err
	}
	for _, e := range pents {
		if sm := volRE.FindStringSubmatch(e.Name()); sm != nil {
			first, _ := strconv.Atoi(sm[1])
			n, _ := strconv.Atoi(sm[2])
			for i := range n {
				m.Exponents = append(m.Exponents, uint32(first+i))
			}
		}
	}
	slices.Sort(m.Exponents)
	if len(m.Exponents) != *recov {
		return fmt.Errorf("volume names hold %d recovery blocks but par2cmdline said %d", len(m.Exponents), *recov)
	}
	m.Damage = scenarios(m)

	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dir, "manifest.json"), append(out, '\n'), 0o644)
}

// scenarios lists the damage scenarios for a case. Expectations come from
// slice arithmetic only.
func scenarios(m *Manifest) []Damage {
	var inSet []File
	for _, f := range m.Files {
		if f.InSet {
			inSet = append(inSet, f)
		}
	}
	S, R, total := m.SliceSize, m.RecoveryBlocks, m.TotalSlices
	var out []Damage

	finish := func(d Damage) Damage {
		d.Files = map[string]Expect{}
		for _, f := range inSet {
			d.Files[f.Name] = Expect{State: "ok"}
		}
		slicesOf := map[string][]int{}
		for _, op := range d.Ops {
			f := lookup(inSet, op.File)
			switch op.Op {
			case "flip":
				slicesOf[f.Name] = append(slicesOf[f.Name], op.Offset/S)
				d.Files[f.Name] = Expect{State: "damaged"}
			case "truncate":
				for s := op.Size / S; s < f.Slices; s++ {
					slicesOf[f.Name] = append(slicesOf[f.Name], s)
				}
				d.Files[f.Name] = Expect{State: "damaged"}
			case "append":
				// Every slice still matches (par2cmdline and Verify match a
				// short last slice by its own length), only the length is wrong.
				d.Files[f.Name] = Expect{State: "damaged"}
			case "delete":
				d.Files[f.Name] = Expect{State: "missing", Damaged: f.Slices}
			case "rename":
				d.Files[f.Name] = Expect{State: "ok", RenamedFrom: op.To}
			}
		}
		d.Needed = 0
		for _, f := range inSet {
			e := d.Files[f.Name]
			if e.State == "damaged" {
				s := slicesOf[f.Name]
				slices.Sort(s)
				e.Slices = slices.Compact(s)
				e.Damaged = len(e.Slices)
				d.Files[f.Name] = e
			}
			d.Needed += e.Damaged
		}
		d.Repairable = d.Needed <= R
		return d
	}
	add := func(name string, ops ...Op) { out = append(out, finish(Damage{Name: name, Ops: ops})) }

	// flipSlices damages n slices spread over the whole set, one flipped
	// byte at the start of each.
	flipSlices := func(n int) []Op {
		var ops []Op
		for j := range n {
			g := j * total / n
			for _, f := range inSet {
				if g < f.Slices {
					ops = append(ops, Op{Op: "flip", File: f.Name, Offset: g * S})
					break
				}
				g -= f.Slices
			}
		}
		return ops
	}

	first, last := inSet[0], inSet[len(inSet)-1]
	add("flip_one", Op{Op: "flip", File: first.Name, Offset: first.Size / 2})
	add("flip_last_byte", Op{Op: "flip", File: last.Name, Offset: last.Size - 1})
	if total >= 3 && R >= 3 {
		add("flip_three", flipSlices(3)...)
	}
	add("flip_capacity", flipSlices(min(R, total))...)
	if total > R {
		add("flip_beyond_capacity", flipSlices(R+1)...)
	}
	for _, f := range inSet {
		add("delete_"+f.Name, Op{Op: "delete", File: f.Name})
	}
	if first.Size >= 2 {
		add("truncate_half", Op{Op: "truncate", File: first.Name, Size: first.Size / 2})
		add("truncate_zero", Op{Op: "truncate", File: first.Name, Size: 0})
	}
	if last.Size >= 2 {
		add("truncate_last_byte", Op{Op: "truncate", File: last.Name, Size: last.Size - 1})
	}
	add("append_garbage", Op{Op: "append", File: last.Name, Size: 1000})
	add("rename_obfuscated", Op{Op: "rename", File: first.Name, To: "x7Kq9zLm3Pw.bin"})
	return out
}

func lookup(fs []File, name string) File {
	for _, f := range fs {
		if f.Name == name {
			return f
		}
	}
	panic("unknown file " + name)
}
