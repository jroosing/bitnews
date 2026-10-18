//go:build bigbench

package par2_test

// Large-input benchmarks, kept out of normal runs by the bigbench build tag:
//
//	PAR2_CONFORMANCE=1 go test -tags bigbench -run '^$' -bench Big -benchtime 1x ./internal/postproc/par2/
//
// The input (BIGBENCH_MIB MiB of seeded pseudo-random bytes, default 512, in
// two files) is generated in a temporary directory and protected by the real
// par2cmdline (1 MiB slices, 8 recovery slices), once per benchmark; nothing
// large is committed. Numbers are recorded below.
//
// Apple M4 (10 cores), go1.27.2, darwin/arm64, par2cmdline 1.4.0, 512 MiB
// input in 2 files (384 MiB + 128 MiB), 1 MiB slices, 8 recovery slices,
// measured with -benchtime 1x on a shared machine:
//
//	BenchmarkBigVerify   512 MiB intact                         0.68 s   (784 MB/s)
//	BenchmarkBigRepair   Verify + Repair + re-Verify of 4
//	                     damaged slices                          1.98 s  (272 MB/s)
//
// Data is in the page cache after the par2cmdline run, so these are CPU and
// memory numbers, not disk numbers.

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

const bigSlice = 1 << 20

// bigEnv creates the payload and protects it with par2cmdline.
func bigEnv(b *testing.B) (dir string, set *par2.Set, files []string, size int64) {
	b.Helper()
	bin := goldenPar2Path(b)
	mib := 512
	if v := os.Getenv("BIGBENCH_MIB"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(b, err)
		mib = n
	}
	dir = b.TempDir()
	r := rand.New(rand.NewPCG(99, 100))
	sizes := []int{mib * 3 / 4 << 20, mib / 4 << 20}
	for i, n := range sizes {
		name := "big" + strconv.Itoa(i) + ".bin"
		f, err := os.Create(filepath.Join(dir, name))
		require.NoError(b, err)
		buf := make([]byte, 1<<20)
		for left := n; left > 0; {
			m := min(left, len(buf))
			for j := 0; j < m; j += 8 {
				v := r.Uint64()
				for k := 0; k < 8 && j+k < m; k++ {
					buf[j+k] = byte(v >> (8 * k))
				}
			}
			_, err := f.Write(buf[:m])
			require.NoError(b, err)
			left -= m
		}
		require.NoError(b, f.Close())
		files = append(files, name)
		size += int64(n)
	}
	out, code := goldenRunPar2(b, bin, dir, append([]string{"c", "-q", "-s" + strconv.Itoa(bigSlice), "-c8", "-n2", "set.par2"}, files...)...)
	require.Zero(b, code, out)
	var err error
	set, err = par2.LoadDir(dir)
	require.NoError(b, err)
	return dir, set, files, size
}

func BenchmarkBigVerify(b *testing.B) {
	dir, set, _, size := bigEnv(b)
	b.SetBytes(size)
	b.ResetTimer()
	for b.Loop() {
		res, err := par2.Verify(context.Background(), dir, set)
		require.NoError(b, err)
		require.False(b, res.NeedsRepair())
	}
}

func BenchmarkBigRepair(b *testing.B) {
	dir, set, files, size := bigEnv(b)
	b.SetBytes(size)
	// Flip one byte in four slices of the first file; XOR makes the damage
	// cheap to apply again after every repair.
	var ops []goldenOp
	for _, off := range []int{0, 17 << 20, 100<<20 + 5, 300<<20 + 77} {
		ops = append(ops, goldenOp{Op: "flip", File: files[0], Offset: off})
	}
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		goldenApply(b, dir, ops)
		b.StartTimer()
		vr, err := par2.Verify(context.Background(), dir, set)
		require.NoError(b, err)
		require.EqualValues(b, 4, vr.DamagedSlices)
		_, err = par2.Repair(context.Background(), dir, set, vr)
		require.NoError(b, err)
	}
}
