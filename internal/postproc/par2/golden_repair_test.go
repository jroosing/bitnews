package par2_test

// Repair against recovery volumes written by the real par2cmdline, plus the
// BenchmarkGolden* benchmarks. See golden_test.go for the fixtures.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

// goldenRequireOriginal checks dir holds exactly the case's payload files,
// byte for byte (empty files included), and nothing else.
func goldenRequireOriginal(t testing.TB, c *goldenCase, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var got, want []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	for _, f := range c.M.Files {
		want = append(want, f.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	require.Equal(t, want, got, "files in the repaired directory")
	for _, f := range c.M.Files {
		b, err := os.ReadFile(filepath.Join(dir, f.Name))
		require.NoError(t, err)
		require.True(t, string(b) == string(c.payload(t, f.Name)), "%s is not byte-identical to the original", f.Name)
	}
}

// TestGoldenRepair damages a copy of the payload and repairs it with the real
// recovery volumes. Damage within the recovery capacity must come back
// byte-exact; beyond it Repair must refuse with the right counts and leave
// the directory alone.
func TestGoldenRepair(t *testing.T) {
	for _, c := range goldenCases(t) {
		for _, d := range c.M.Damage {
			t.Run(c.M.Case+"/"+d.Name, func(t *testing.T) {
				dir := c.workDir(t)
				goldenApply(t, dir, d.Ops)
				set := c.loadSet(t)
				vr, err := par2.Verify(context.Background(), dir, set)
				require.NoError(t, err)

				if !d.Repairable {
					before := dirDigests(t, dir)
					_, err := par2.Repair(context.Background(), dir, set, vr)
					var ie *par2.InsufficientError
					require.ErrorAs(t, err, &ie)
					assert.ErrorIs(t, err, par2.ErrInsufficientRecovery)
					assert.Equal(t, d.Needed, ie.Needed)
					assert.Equal(t, c.M.RecoveryBlocks, ie.Available)
					assert.Equal(t, before, dirDigests(t, dir), "a refused repair must not touch anything")
					return
				}
				rr, err := par2.Repair(context.Background(), dir, set, vr)
				require.NoError(t, err)
				assert.Equal(t, d.Needed, rr.RepairedSlices)
				assert.False(t, rr.Verify.NeedsRepair())
				goldenRequireOriginal(t, c, dir)
			})
		}
	}
}

// TestGoldenRepairSmallChunks repeats the capacity repair with a chunk far
// smaller than the slice, so a slice is decoded in many passes.
func TestGoldenRepairSmallChunks(t *testing.T) {
	for _, name := range []string{"large", "slice_16k", "block_count"} {
		c := goldenCaseByName(t, name)
		for _, d := range c.M.Damage {
			if d.Name != "flip_capacity" {
				continue
			}
			t.Run(name, func(t *testing.T) {
				dir := c.workDir(t)
				goldenApply(t, dir, d.Ops)
				set := c.loadSet(t)
				vr, err := par2.Verify(context.Background(), dir, set)
				require.NoError(t, err)
				_, err = par2.Repair(context.Background(), dir, set, vr, par2.WithChunkSize(1000))
				require.NoError(t, err)
				goldenRequireOriginal(t, c, dir)
			})
		}
	}
}

// TestGoldenRepairFromOneVolume loads the index plus a single recovery volume,
// so Repair must decode with an arbitrary run of exponents (not 0..n).
func TestGoldenRepairFromOneVolume(t *testing.T) {
	for _, c := range goldenCases(t) {
		files := c.par2Files(t)
		for _, d := range c.M.Damage {
			if d.Name != "flip_one" && d.Name != "flip_three" {
				continue
			}
			for _, vol := range files {
				t.Run(c.M.Case+"/"+d.Name+"/"+filepath.Base(vol), func(t *testing.T) {
					set, err := par2.LoadFiles([]string{vol})
					require.NoError(t, err)
					if len(set.Recovery) < d.Needed {
						t.Skip("volume too small for this damage")
					}
					dir := c.workDir(t)
					goldenApply(t, dir, d.Ops)
					vr, err := par2.Verify(context.Background(), dir, set)
					require.NoError(t, err)
					_, err = par2.Repair(context.Background(), dir, set, vr)
					if len(set.Recovery) == 0 {
						var ie *par2.InsufficientError
						require.ErrorAs(t, err, &ie)
						return
					}
					require.NoError(t, err)
					goldenRequireOriginal(t, c, dir)
				})
			}
		}
	}
}

// TestGoldenRepairMultipleDamagedFiles damages several files at once: a
// missing file, a truncated one and a byte flip, then repairs in one go.
func TestGoldenRepairMultipleDamagedFiles(t *testing.T) {
	c := goldenCaseByName(t, "explicit_uniform") // 20 recovery slices
	files := c.inSet()
	require.Len(t, files, 2)
	dir := c.workDir(t)
	// u1.bin: 37 slices; flip 3 slices, truncate u2.bin (3 slices) to 1 slice.
	goldenApply(t, dir, []goldenOp{
		{Op: "flip", File: files[0].Name, Offset: 0},
		{Op: "flip", File: files[0].Name, Offset: 5 * 4096},
		{Op: "flip", File: files[0].Name, Offset: 36*4096 + 3},
		{Op: "truncate", File: files[1].Name, Size: 4096},
	})
	set := c.loadSet(t)
	vr, err := par2.Verify(context.Background(), dir, set)
	require.NoError(t, err)
	require.EqualValues(t, 3+files[1].Slices-1, vr.DamagedSlices)
	rr, err := par2.Repair(context.Background(), dir, set, vr)
	require.NoError(t, err)
	assert.Equal(t, int(vr.DamagedSlices), rr.RepairedSlices)
	goldenRequireOriginal(t, c, dir)
}

// TestGoldenPar2RepairAgrees (PAR2_CONFORMANCE=1) runs the real `par2 r` on
// the same damage. A repairable scenario must end byte-identical to the
// original; an unrepairable one must fail. Either way the directory Repair
// produced is accepted by `par2 v`.
func TestGoldenPar2RepairAgrees(t *testing.T) {
	bin := goldenPar2Path(t)
	for _, c := range goldenCases(t) {
		for _, d := range c.M.Damage {
			t.Run(c.M.Case+"/"+d.Name, func(t *testing.T) {
				// par2 repairs a copy.
				theirs := c.workDir(t)
				goldenApply(t, theirs, d.Ops)
				index, data := goldenWithPar2(t, c, theirs)
				out, code := goldenRunPar2(t, bin, theirs, append([]string{"r", index}, data...)...)
				if d.Repairable {
					require.Zero(t, code, "par2 r should repair this damage:\n%s", out)
				} else {
					require.NotZero(t, code, "par2 r should refuse this damage:\n%s", out)
				}

				// Ours repairs another copy; par2 v must then be satisfied.
				ours := c.workDir(t)
				goldenApply(t, ours, d.Ops)
				set := c.loadSet(t)
				vr, err := par2.Verify(context.Background(), ours, set)
				require.NoError(t, err)
				_, err = par2.Repair(context.Background(), ours, set, vr)
				if !d.Repairable {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
				goldenRequireOriginal(t, c, ours)
				index, data = goldenWithPar2(t, c, ours)
				out, code = goldenRunPar2(t, bin, ours, append([]string{"v", index}, data...)...)
				assert.Zero(t, code, "par2 v on our repaired output:\n%s", out)
			})
		}
	}
}

// ---- Benchmarks ------------------------------------------------------------

// goldenBenchEnv is the largest case with its recovery capacity of damage
// applied in place; restoring it is a few XORs, so iterations are cheap to
// reset (done with the timer stopped).
func goldenBenchEnv(b *testing.B) (c *goldenCase, dir string, set *par2.Set, damage goldenDamage) {
	b.Helper()
	c = goldenCaseByName(b, "large")
	for _, d := range c.M.Damage {
		if d.Name == "flip_capacity" {
			damage = d
		}
	}
	require.NotEmpty(b, damage.Ops)
	return c, c.workDir(b), c.loadSet(b), damage
}

func goldenPayloadBytes(c *goldenCase) int64 {
	var n int64
	for _, f := range c.M.Files {
		n += int64(f.Size)
	}
	return n
}

// Numbers (Apple M4, go1.27.2, darwin/arm64, -benchtime 20x, case "large",
// 1.45 MB payload, 90 slices of 16 KiB, 9 recovery slices):
//
//	BenchmarkGoldenVerify   2.5 ms/op   ~580 MB/s
//	BenchmarkGoldenRepair  17.8 ms/op    ~81 MB/s  (9 damaged slices, Verify + Repair + re-Verify)
//
// The 512 MiB equivalents are in golden_bigbench_test.go.

// BenchmarkGoldenVerify verifies the intact "large" case (1.4 MB).
func BenchmarkGoldenVerify(b *testing.B) {
	c, dir, set, _ := goldenBenchEnv(b)
	b.SetBytes(goldenPayloadBytes(c))
	b.ResetTimer()
	for b.Loop() {
		res, err := par2.Verify(context.Background(), dir, set)
		if err != nil || res.NeedsRepair() {
			b.Fatalf("verify: %v", err)
		}
	}
}

// BenchmarkGoldenRepair verifies and repairs the "large" case damaged in as
// many slices as it has recovery slices. Each iteration includes the Verify
// before and the one inside Repair.
func BenchmarkGoldenRepair(b *testing.B) {
	c, dir, set, damage := goldenBenchEnv(b)
	b.SetBytes(goldenPayloadBytes(c))
	for b.Loop() {
		b.StopTimer()
		goldenApply(b, dir, damage.Ops)
		b.StartTimer()
		vr, err := par2.Verify(context.Background(), dir, set)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := par2.Repair(context.Background(), dir, set, vr); err != nil {
			if errors.Is(err, par2.ErrRepairFailed) {
				b.Fatalf("repair left damage: %v", err)
			}
			b.Fatal(err)
		}
	}
	b.StopTimer()
	goldenRequireOriginal(b, c, dir)
}
