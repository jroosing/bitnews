package gf16_test

import (
	"fmt"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

func BenchmarkMulAdd(b *testing.B) {
	r := newRand(90)
	src, dst := randBytes(r, 1<<20), randBytes(r, 1<<20)
	b.SetBytes(1 << 20)
	for b.Loop() {
		_ = gf16.MulAdd(dst, src, 0x1234)
	}
}

func BenchmarkMulAddParallel(b *testing.B) {
	r := newRand(91)
	src, dst := randBytes(r, 1<<20), randBytes(r, 1<<20)
	b.SetBytes(1 << 20)
	for b.Loop() {
		_ = gf16.MulAddParallel(dst, src, 0x1234)
	}
}

// BenchmarkSolve recovers 10 of 100 inputs. The arithmetic does not depend
// on the data, so random recovery slices stand in for encoded ones. Bytes per
// op is the size of the whole input set.
func BenchmarkSolve(b *testing.B) {
	for _, sliceSize := range []int{64 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("100x%dKiB", sliceSize>>10), func(b *testing.B) {
			r := newRand(92)
			const n, k = 100, 10
			inputs := make([][]byte, n)
			for i := range inputs {
				inputs[i] = randBytes(r, sliceSize)
			}
			missing := make([]int, k)
			exps := make([]uint32, k)
			recovery := make([][]byte, k)
			for m := range k {
				missing[m] = m * 10
				inputs[m*10] = nil
				exps[m] = uint32(m)
				recovery[m] = randBytes(r, sliceSize)
			}
			b.SetBytes(int64(n * sliceSize))
			for b.Loop() {
				if _, err := gf16.Solve(inputs, missing, exps, recovery); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkNewDecoder measures building and inverting the k×k matrix.
func BenchmarkNewDecoder(b *testing.B) {
	for _, k := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("k=%d", k), func(b *testing.B) {
			missing := make([]int, k)
			exps := make([]uint32, k)
			for m := range k {
				missing[m] = m * 3
				exps[m] = uint32(m)
			}
			for b.Loop() {
				if _, err := gf16.NewDecoder(gf16.MaxInputSlices, missing, exps); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
