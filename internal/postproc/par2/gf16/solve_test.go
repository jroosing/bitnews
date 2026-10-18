package gf16_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

// encode is a test-only PAR2 encoder: recovery slice e is
// Σ_i c_i^e · input_i over 16-bit little-endian words, with every input
// zero-padded to sliceSize.
func encode(inputs [][]byte, sliceSize int, exps []uint32) [][]byte {
	cs := gf16.InputConstants(len(inputs))
	out := make([][]byte, len(exps))
	for r, e := range exps {
		rec := make([]byte, sliceSize)
		for i, in := range inputs {
			f := gf16.Pow(cs[i], e)
			padded := make([]byte, sliceSize)
			copy(padded, in)
			for w := 0; w < sliceSize; w += 2 {
				v := binary.LittleEndian.Uint16(rec[w:]) ^ gf16.Mul(f, binary.LittleEndian.Uint16(padded[w:]))
				binary.LittleEndian.PutUint16(rec[w:], v)
			}
		}
		out[r] = rec
	}
	return out
}

func padTo(b []byte, n int) []byte {
	p := make([]byte, n)
	copy(p, b)
	return p
}

func TestSolve_RestoresOneErasedInput(t *testing.T) {
	r := newRand(30)
	const sliceSize = 64
	inputs := make([][]byte, 5)
	for i := range inputs {
		inputs[i] = randBytes(r, sliceSize)
	}
	exps := []uint32{0}
	recovery := encode(inputs, sliceSize, exps)

	want := inputs[3]
	damaged := append([][]byte(nil), inputs...)
	damaged[3] = nil

	got, err := gf16.Solve(damaged, []int{3}, exps, recovery)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("Solve restored %x, want %x", got, want)
	}
}

type repairCase struct {
	name      string
	inputs    int
	sliceSize int
	missing   int
	exps      []uint32 // chosen recovery exponents, len == missing
}

// runRoundTrip builds random inputs (some short, so zero-padded), encodes
// recovery slices, erases inputs and checks Solve restores them byte-exactly.
func runRoundTrip(t *testing.T, seed uint64, c repairCase) {
	t.Helper()
	r := newRand(seed)
	inputs := make([][]byte, c.inputs)
	for i := range inputs {
		n := c.sliceSize
		if r.IntN(3) == 0 {
			n = r.IntN(c.sliceSize + 1) // short last slice of a file, any parity
		}
		inputs[i] = randBytes(r, n)
	}
	recovery := encode(inputs, c.sliceSize, c.exps)

	missing := r.Perm(c.inputs)[:c.missing]
	damaged := append([][]byte(nil), inputs...)
	for _, i := range missing {
		damaged[i] = nil
	}
	got, err := gf16.Solve(damaged, missing, c.exps, recovery)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if len(got) != len(missing) {
		t.Fatalf("Solve returned %d slices, want %d", len(got), len(missing))
	}
	for m, i := range missing {
		if want := padTo(inputs[i], c.sliceSize); !bytes.Equal(got[m], want) {
			t.Fatalf("restored input %d differs from the original", i)
		}
	}
}

func consecutive(start uint32, n int) []uint32 {
	e := make([]uint32, n)
	for i := range e {
		e[i] = start + uint32(i)
	}
	return e
}

func TestSolve_RestoresErasedInputsByteExactly(t *testing.T) {
	cases := []repairCase{
		{"single input", 1, 4, 1, []uint32{0}},
		{"two of three", 3, 10, 2, []uint32{0, 1}},
		{"all inputs lost", 4, 32, 4, consecutive(0, 4)},
		{"sparse exponents", 12, 100, 3, []uint32{1, 4, 9}},
		{"high exponents", 20, 202, 5, consecutive(60000, 5)},
		{"wrapping exponents", 8, 66, 3, []uint32{65534, 65535, 65536}},
		{"many inputs", 300, 64, 20, consecutive(7, 20)},
		{"multi-chunk slices", 6, 600 << 10, 3, consecutive(2, 3)},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) { runRoundTrip(t, uint64(100+i), c) })
	}
}

func TestSolve_RecoversWhenEliminationNeedsRowExchange(t *testing.T) {
	// c_0 = 2^1 and c_2 = 2^4, so (c_2/c_0)^21845 = 2^65535 = 1: with exponents
	// 0 and 21845 first, the leading 2×2 minor is zero and elimination must
	// pivot, yet the full 3×3 matrix (adding c_1 = 2^2 and exponent 1) is
	// invertible.
	r := newRand(40)
	const sliceSize = 48
	inputs := make([][]byte, 4)
	for i := range inputs {
		inputs[i] = randBytes(r, sliceSize)
	}
	missing := []int{0, 2, 1}
	exps := []uint32{0, 21845, 1}
	recovery := encode(inputs, sliceSize, exps)
	damaged := [][]byte{nil, nil, nil, inputs[3]}

	got, err := gf16.Solve(damaged, missing, exps, recovery)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	for m, i := range missing {
		if !bytes.Equal(got[m], inputs[i]) {
			t.Fatalf("restored input %d differs from the original", i)
		}
	}
}
