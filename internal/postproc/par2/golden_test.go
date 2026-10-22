package par2_test

// Golden conformance tests: our packet parser, Verify and GF(2^16) math
// against sets written by the real par2cmdline (see testdata/README.md and
// scripts/gen-par2-fixtures.sh). The expected values in every manifest.json
// come from the payload bytes and from par2cmdline's own output, never from
// our code. A failure here is a conformance bug in our code, not in the data.
//
// Tests that run par2cmdline itself are gated: set PAR2_CONFORMANCE=1 and
// have `par2` on PATH. Default runs need no external program.

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

// goldenManifest mirrors manifest.json (written by internal/fixturegen).
type goldenManifest struct {
	Case           string         `json:"case"`
	Par2cmdline    string         `json:"par2cmdline"`
	Par2Args       string         `json:"par2_args"`
	SliceSize      int            `json:"slice_size"`
	RecoveryBlocks int            `json:"recovery_blocks"`
	Exponents      []uint32       `json:"exponents"`
	TotalSlices    int            `json:"total_slices"`
	Files          []goldenFile   `json:"files"`
	Damage         []goldenDamage `json:"damage"`
}

type goldenFile struct {
	Name   string `json:"name"`
	Size   int    `json:"size"`
	MD5    string `json:"md5"`
	InSet  bool   `json:"in_set"`
	Slices int    `json:"slices"`
}

type goldenOp struct {
	Op     string `json:"op"`
	File   string `json:"file"`
	Offset int    `json:"offset"`
	Size   int    `json:"size"`
	To     string `json:"to"`
}

type goldenExpect struct {
	State       string `json:"state"`
	Damaged     int    `json:"damaged"`
	Slices      []int  `json:"slices"`
	RenamedFrom string `json:"renamed_from"`
}

type goldenDamage struct {
	Name       string                  `json:"name"`
	Ops        []goldenOp              `json:"ops"`
	Needed     int                     `json:"needed"`
	Repairable bool                    `json:"repairable"`
	Files      map[string]goldenExpect `json:"files"`
}

// goldenCase is one directory below testdata/golden.
type goldenCase struct {
	Dir string
	M   goldenManifest
}

func goldenCases(t testing.TB) []*goldenCase {
	t.Helper()
	root := filepath.Join("testdata", "golden")
	ents, err := os.ReadDir(root)
	require.NoError(t, err)
	var out []*goldenCase
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		c := &goldenCase{Dir: filepath.Join(root, e.Name())}
		raw, err := os.ReadFile(filepath.Join(c.Dir, "manifest.json"))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &c.M))
		out = append(out, c)
	}
	require.NotEmpty(t, out, "no golden fixtures; run scripts/gen-par2-fixtures.sh")
	return out
}

func goldenCaseByName(t testing.TB, name string) *goldenCase {
	t.Helper()
	for _, c := range goldenCases(t) {
		if c.M.Case == name {
			return c
		}
	}
	require.Failf(t, "golden case not found", "%s", name)
	return nil
}

func (c *goldenCase) par2Dir() string { return filepath.Join(c.Dir, "par2") }

func (c *goldenCase) par2Files(t testing.TB) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(c.par2Dir(), "*.par2"))
	require.NoError(t, err)
	slices.Sort(m)
	return m
}

func (c *goldenCase) payload(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.Dir, "data", name))
	require.NoError(t, err)
	return b
}

// inSet returns the files of the recovery set (par2cmdline skips empty ones).
func (c *goldenCase) inSet() []goldenFile {
	var out []goldenFile
	for _, f := range c.M.Files {
		if f.InSet {
			out = append(out, f)
		}
	}
	return out
}

// workDir copies the payload into a fresh temporary directory.
func (c *goldenCase) workDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range c.M.Files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f.Name), c.payload(t, f.Name), 0o600))
	}
	return dir
}

func (c *goldenCase) loadSet(t testing.TB) *par2.Set {
	t.Helper()
	set, err := par2.LoadDir(c.par2Dir())
	require.NoError(t, err)
	return set
}

// goldenFileID is the PAR2 file ID formula, written out from the spec:
// MD5(MD5 of the first 16 KiB, length as u64 LE, name).
func goldenFileID(data []byte, name string) par2.ID {
	h16 := md5.Sum(data[:min(len(data), 16*1024)])
	var l [8]byte
	binary.LittleEndian.PutUint64(l[:], uint64(len(data)))
	return md5.Sum(slices.Concat(h16[:], l[:], []byte(name)))
}

// goldenCompareID orders file IDs the way par2cmdline does: as 128-bit
// little-endian integers (the last byte is the most significant), not as byte
// strings. The Main packet lists the recovery set's files in this order and it
// is also the order of the input slices.
func goldenCompareID(a, b par2.ID) int {
	for i := 15; i >= 0; i-- {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return 0
}

// goldenSlices cuts data into slices of size, the last one zero padded.
func goldenSlices(data []byte, size int) [][]byte {
	var out [][]byte
	for off := 0; off < len(data); off += size {
		s := make([]byte, size)
		copy(s, data[off:min(off+size, len(data))])
		out = append(out, s)
	}
	return out
}

// goldenApply applies damage ops to dir. Flip XORs 0xFF into one byte,
// append adds a deterministic pattern.
func goldenApply(t testing.TB, dir string, ops []goldenOp) {
	t.Helper()
	for _, op := range ops {
		p := filepath.Join(dir, op.File)
		switch op.Op {
		case "flip":
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			b[op.Offset] ^= 0xFF
			require.NoError(t, os.WriteFile(p, b, 0o600))
		case "truncate":
			require.NoError(t, os.Truncate(p, int64(op.Size)))
		case "delete":
			require.NoError(t, os.Remove(p))
		case "rename":
			require.NoError(t, os.Rename(p, filepath.Join(dir, op.To)))
		case "append":
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			for i := range op.Size {
				b = append(b, byte(i*7+3))
			}
			require.NoError(t, os.WriteFile(p, b, 0o600))
		default:
			require.Failf(t, "unknown damage op", "%q", op.Op)
		}
	}
}

// ---- 1. Parse ------------------------------------------------------------

func TestGoldenParse(t *testing.T) {
	for _, c := range goldenCases(t) {
		t.Run(c.M.Case, func(t *testing.T) {
			m := c.M
			assert.Contains(t, m.Par2cmdline, "1.4", "fixtures made by par2cmdline 1.4.x")
			S := m.SliceSize

			// Every packet of every file: valid, contiguous, of a known type.
			exps := map[uint32]bool{}
			for _, path := range c.par2Files(t) {
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				next := int64(0)
				var fileExps []uint32
				var main *par2.Packet
				for p, err := range par2.Scan(bytes.NewReader(raw), int64(len(raw))) {
					require.NoError(t, err)
					assert.Equal(t, next, p.Offset, "%s: packets must tile the file", filepath.Base(path))
					assert.Zero(t, p.Length%4)
					next = p.Offset + p.Length
					require.NotNil(t, p.Body, "%s: unexpected packet type at %d", filepath.Base(path), p.Offset)
					switch b := p.Body.(type) {
					case *par2.Main:
						main = &p
						// Set ID == MD5 of the Main body, derived here from raw bytes.
						sum := md5.Sum(raw[p.Offset+64 : p.Offset+p.Length])
						assert.Equal(t, par2.ID(sum), p.SetID)
						assert.Equal(t, p.SetID, b.DerivedSetID)
					case *par2.RecvSlic:
						assert.Equal(t, int64(S), b.DataLength)
						assert.Equal(t, p.Offset+p.Length-int64(S), b.DataOffset, "slice data is the tail of the packet")
						fileExps = append(fileExps, b.Exponent)
						exps[b.Exponent] = true
					}
				}
				assert.Equal(t, int64(len(raw)), next, "%s: scanner must consume the whole file", filepath.Base(path))
				assert.NotNil(t, main, "%s: every par2 file repeats the Main packet", filepath.Base(path))
				// The volume's name announces which exponents it holds.
				if sm := regexp.MustCompile(`\.vol(\d+)\+(\d+)\.par2$`).FindStringSubmatch(path); sm != nil {
					first, _ := strconv.Atoi(sm[1])
					n, _ := strconv.Atoi(sm[2])
					want := make([]uint32, n)
					for i := range want {
						want[i] = uint32(first + i)
					}
					assert.Equal(t, want, fileExps, "%s: exponents in file order", filepath.Base(path))
				} else {
					assert.Empty(t, fileExps, "the index file holds no recovery slices")
				}
			}
			assert.ElementsMatch(t, m.Exponents, slices.Collect(func(yield func(uint32) bool) {
				for e := range exps {
					if !yield(e) {
						return
					}
				}
			}))

			set := c.loadSet(t)
			assert.Empty(t, set.Warnings, "a clean set must load without warnings")
			assert.Equal(t, uint64(S), set.SliceSize)
			assert.Contains(t, set.Creator, "par2cmdline")
			assert.Empty(t, set.NonRecoveryIDs)

			// File IDs: formula, ascending order, descriptions and checksums.
			type exp struct {
				id par2.ID
				f  goldenFile
			}
			var want []exp
			for _, f := range c.inSet() {
				want = append(want, exp{goldenFileID(c.payload(t, f.Name), f.Name), f})
			}
			slices.SortFunc(want, func(a, b exp) int { return goldenCompareID(a.id, b.id) })
			require.Len(t, set.FileIDs, len(want))
			for i, w := range want {
				assert.Equal(t, w.id, set.FileIDs[i], "Main packet lists file IDs in ascending (little-endian numeric) order")
			}
			for _, w := range want {
				data := c.payload(t, w.f.Name)
				fd := set.FileDescs[w.id]
				require.NotNil(t, fd, w.f.Name)
				assert.Equal(t, w.f.Name, fd.Name)
				assert.Equal(t, uint64(w.f.Size), fd.Length)
				assert.Equal(t, w.f.MD5, par2HexOf(fd.MD5))
				h16 := md5.Sum(data[:min(len(data), 16*1024)])
				assert.Equal(t, h16, fd.MD516k)
				assert.Equal(t, w.id, fd.FileID)

				ifsc := set.IFSCs[w.id]
				require.NotNil(t, ifsc, w.f.Name)
				sl := goldenSlices(data, S)
				require.Len(t, ifsc.Slices, len(sl))
				assert.Equal(t, w.f.Slices, len(sl))
				for i, s := range sl {
					assert.Equal(t, md5.Sum(s), ifsc.Slices[i].MD5, "%s slice %d MD5 (padded)", w.f.Name, i)
					assert.Equal(t, crc32.ChecksumIEEE(s), ifsc.Slices[i].CRC32, "%s slice %d CRC32 (padded)", w.f.Name, i)
				}
			}

			got := make([]uint32, 0, len(set.Recovery))
			for e := range set.Recovery {
				got = append(got, e)
			}
			assert.ElementsMatch(t, m.Exponents, got)
		})
	}
}

func par2HexOf(b [16]byte) string { return par2.ID(b).String() }

// ---- 2. Verify -----------------------------------------------------------

func TestGoldenVerifyIntact(t *testing.T) {
	for _, c := range goldenCases(t) {
		t.Run(c.M.Case, func(t *testing.T) {
			dir := c.workDir(t)
			res, err := par2.Verify(context.Background(), dir, c.loadSet(t))
			require.NoError(t, err)
			assert.False(t, res.NeedsRepair())
			assert.Zero(t, res.DamagedSlices)
			assert.Equal(t, c.M.RecoveryBlocks, res.RecoverySlicesOnDisk)
			assert.Empty(t, res.Warnings)
			require.Len(t, res.Files, len(c.inSet()), "empty files are not part of the set")
			for _, f := range res.Files {
				assert.Equal(t, par2.StateOK, f.State, f.Name)
				assert.Nil(t, f.Renamed)
			}
			for _, f := range c.inSet() {
				fr := verifyResultFile(t, res, f.Name)
				assert.EqualValues(t, f.Slices, fr.TotalSlices)
				assert.EqualValues(t, f.Size, fr.Size)
			}
		})
	}
}

func verifyResultFile(t testing.TB, r *par2.Result, name string) par2.FileResult {
	t.Helper()
	for _, f := range r.Files {
		if f.Name == name {
			return f
		}
	}
	require.Failf(t, "file missing from the Verify result", "%q", name)
	return par2.FileResult{}
}

func TestGoldenVerifyDamage(t *testing.T) {
	for _, c := range goldenCases(t) {
		for _, d := range c.M.Damage {
			t.Run(c.M.Case+"/"+d.Name, func(t *testing.T) {
				dir := c.workDir(t)
				goldenApply(t, dir, d.Ops)
				res, err := par2.Verify(context.Background(), dir, c.loadSet(t))
				require.NoError(t, err)
				goldenCheckVerify(t, d, res)
			})
		}
	}
}

func goldenCheckVerify(t *testing.T, d goldenDamage, res *par2.Result) {
	t.Helper()
	assert.EqualValues(t, d.Needed, res.DamagedSlices)
	assert.Equal(t, d.Repairable, res.Repairable())
	require.Len(t, res.Files, len(d.Files))
	for name, want := range d.Files {
		got := verifyResultFile(t, res, name)
		assert.Equal(t, want.State, got.State.String(), name)
		assert.EqualValues(t, want.Damaged, got.DamagedCount, name)
		if want.State == "damaged" {
			assert.Equal(t, slices.Clone(want.Slices), slices.Clone(got.DamagedSlices), "%s: damaged slice indexes", name)
		}
		if want.RenamedFrom != "" {
			require.NotNil(t, got.Renamed, name)
			assert.Equal(t, want.RenamedFrom, got.Renamed.From)
			assert.Equal(t, name, got.Renamed.To)
		} else {
			assert.Nil(t, got.Renamed, name)
		}
	}
}

// ---- 3. GF(2^16) recovery data, byte for byte ----------------------------

// TestGoldenRecoveryBytes recomputes every recovery slice par2cmdline wrote
// with our gf16 (InputConstants + Pow + MulAdd over the real payload in
// PAR2's global input order) and demands identical bytes.
func TestGoldenRecoveryBytes(t *testing.T) {
	for _, c := range goldenCases(t) {
		t.Run(c.M.Case, func(t *testing.T) {
			// Input order: files by ascending file ID, slices in file order.
			type ent struct {
				id par2.ID
				f  goldenFile
			}
			var order []ent
			for _, f := range c.inSet() {
				order = append(order, ent{goldenFileID(c.payload(t, f.Name), f.Name), f})
			}
			slices.SortFunc(order, func(a, b ent) int { return goldenCompareID(a.id, b.id) })
			var inputs [][]byte
			for _, e := range order {
				inputs = append(inputs, goldenSlices(c.payload(t, e.f.Name), c.M.SliceSize)...)
			}
			require.Len(t, inputs, c.M.TotalSlices)
			consts := gf16.InputConstants(len(inputs))

			set := c.loadSet(t)
			require.Len(t, set.Recovery, c.M.RecoveryBlocks)
			for _, e := range c.M.Exponents {
				loc, ok := set.Recovery[e]
				require.True(t, ok, "exponent %d", e)
				f, err := os.Open(loc.Path)
				require.NoError(t, err)
				onDisk := make([]byte, loc.Length)
				_, err = io.ReadFull(io.NewSectionReader(f, loc.Offset, loc.Length), onDisk)
				require.NoError(t, err)
				require.NoError(t, f.Close())

				mine := make([]byte, c.M.SliceSize)
				for i, in := range inputs {
					require.NoError(t, gf16.MulAdd(mine, in, gf16.Pow(consts[i], e)))
				}
				if !bytes.Equal(mine, onDisk) {
					at := 0
					for at < len(mine) && mine[at] == onDisk[at] {
						at++
					}
					require.Failf(t, "recovery slice differs from par2cmdline", "exponent %d, first difference at byte %d (of %d); %d input slices", e, at, len(mine), len(inputs))
				}
			}
		})
	}
}

// ---- par2cmdline harness (PAR2_CONFORMANCE=1) -----------------------------

// goldenPar2Path returns the par2 binary or skips the test.
func goldenPar2Path(t testing.TB) string {
	t.Helper()
	if os.Getenv("PAR2_CONFORMANCE") != "1" {
		t.Skip("set PAR2_CONFORMANCE=1 to run par2cmdline")
	}
	p, err := exec.LookPath("par2")
	if err != nil {
		t.Skip("par2 not installed")
	}
	return p
}

// goldenRunPar2 runs par2 in dir and returns its combined output and exit code.
func goldenRunPar2(t testing.TB, bin, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := strings.ReplaceAll(string(out), "\r", "\n")
	if err == nil {
		return text, 0
	}
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee, "par2 did not run: %s", text)
	return text, ee.ExitCode()
}

// goldenWithPar2 copies the par2 files into dir (par2cmdline wants them next
// to the data) and returns the names of the data files present in dir.
func goldenWithPar2(t testing.TB, c *goldenCase, dir string) (index string, data []string) {
	t.Helper()
	for _, p := range c.par2Files(t) {
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, filepath.Base(p)), b, 0o600))
	}
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".par2") {
			data = append(data, e.Name())
		}
	}
	return "set.par2", data
}

var goldenBlocksRE = regexp.MustCompile(`You have (\d+) out of (\d+) data blocks available`)

// TestGoldenPar2VerifyAgrees runs real `par2 v` on the same damaged copies
// and compares how many data blocks it finds damaged with the golden
// expectation (which our Verify must match too).
func TestGoldenPar2VerifyAgrees(t *testing.T) {
	bin := goldenPar2Path(t)
	for _, c := range goldenCases(t) {
		for _, d := range c.M.Damage {
			t.Run(c.M.Case+"/"+d.Name, func(t *testing.T) {
				dir := c.workDir(t)
				goldenApply(t, dir, d.Ops)
				index, data := goldenWithPar2(t, c, dir)
				out, code := goldenRunPar2(t, bin, dir, append([]string{"v", index}, data...)...)
				sm := goldenBlocksRE.FindStringSubmatch(out)
				if sm == nil {
					// Only printed when something is wrong.
					assert.Zero(t, d.Needed, "no block summary but damage expected:\n%s", out)
					return
				}
				have, _ := strconv.Atoi(sm[1])
				total, _ := strconv.Atoi(sm[2])
				assert.Equal(t, c.M.TotalSlices, total)
				if goldenAppendToSingleShortSlice(c, d) {
					// Known difference (testdata/CONFORMANCE.md): par2cmdline cannot
					// match the lone short slice of a file once bytes follow it and
					// counts it lost; Verify matches it by its own length and only
					// reports the wrong file length. Repair ends the same way.
					assert.LessOrEqual(t, total-have, d.Needed+1, "par2 v damaged blocks:\n%s", out)
				} else {
					assert.Equal(t, d.Needed, total-have, "par2 v damaged blocks:\n%s", out)
				}
				switch {
				case goldenAppendToSingleShortSlice(c, d):
				case d.Repairable:
					assert.Equal(t, 1, code, "repairable: exit code 1\n%s", out)
				default:
					assert.Equal(t, 2, code, "not repairable: exit code 2\n%s", out)
				}
			})
		}
	}
}

// goldenAppendToSingleShortSlice reports whether d appends bytes to a file
// that is shorter than one slice.
func goldenAppendToSingleShortSlice(c *goldenCase, d goldenDamage) bool {
	for _, op := range d.Ops {
		if op.Op != "append" {
			continue
		}
		for _, f := range c.M.Files {
			if f.Name == op.File && f.Size < c.M.SliceSize {
				return true
			}
		}
	}
	return false
}

// TestGoldenPar2VerifyIntact checks real par2 accepts the pristine payload.
func TestGoldenPar2VerifyIntact(t *testing.T) {
	bin := goldenPar2Path(t)
	for _, c := range goldenCases(t) {
		t.Run(c.M.Case, func(t *testing.T) {
			dir := c.workDir(t)
			index, data := goldenWithPar2(t, c, dir)
			out, code := goldenRunPar2(t, bin, dir, append([]string{"v", index}, data...)...)
			assert.Zero(t, code, out)
		})
	}
}
