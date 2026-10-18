package gf16_test

import (
	"bytes"
	"errors"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

func TestNewDecoder_DetectsSingularRecoveryChoice(t *testing.T) {
	cases := []struct {
		name    string
		missing []int
		exps    []uint32
	}{
		{"duplicate exponent", []int{1, 4}, []uint32{3, 3}},
		{"exponents equal modulo 65535", []int{0, 5, 6}, []uint32{2, 7, 65537}},
		// Distinct exponents can still be singular: (c_2/c_0)^21845 = 1.
		{"constants collide at this exponent gap", []int{0, 2}, []uint32{0, 21845}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := gf16.NewDecoder(10, c.missing, c.exps)
			if !errors.Is(err, gf16.ErrSingular) {
				t.Fatalf("err = %v, want ErrSingular", err)
			}
		})
	}
}

func TestSolve_ReportsSingularRecoveryChoice(t *testing.T) {
	inputs := [][]byte{nil, {1, 2}, nil}
	recovery := [][]byte{{0, 0}, {0, 0}}
	_, err := gf16.Solve(inputs, []int{0, 2}, []uint32{5, 5}, recovery)
	if !errors.Is(err, gf16.ErrSingular) {
		t.Fatalf("err = %v, want ErrSingular", err)
	}
}

func TestDecoder_StreamsChunksWithReusedBuffers(t *testing.T) {
	r := newRand(50)
	const sliceSize, chunk = 1000, 96
	inputs := make([][]byte, 9)
	for i := range inputs {
		inputs[i] = randBytes(r, sliceSize)
	}
	inputs[8] = inputs[8][:333] // short last slice, zero-padded
	missing := []int{8, 2, 5}
	exps := []uint32{11, 12, 13}
	recovery := encode(inputs, sliceSize, exps)

	d, err := gf16.NewDecoder(len(inputs), missing, exps)
	if err != nil {
		t.Fatal(err)
	}
	restored := make([][]byte, len(missing))
	bufs := make([][]byte, len(missing))
	for m := range bufs {
		bufs[m] = make([]byte, chunk)
	}
	for lo := 0; lo < sliceSize; lo += chunk {
		hi := min(lo+chunk, sliceSize)
		out := make([][]byte, len(bufs))
		for m := range bufs {
			clear(bufs[m])
			out[m] = bufs[m][:hi-lo]
		}
		// Present inputs first, then recovery: order must not matter.
		for i, in := range inputs {
			if i == 2 || i == 5 || i == 8 {
				continue
			}
			if err := d.AddInput(out, i, in[lo:hi]); err != nil {
				t.Fatal(err)
			}
		}
		for k, rec := range recovery {
			if err := d.AddRecovery(out, k, rec[lo:hi]); err != nil {
				t.Fatal(err)
			}
		}
		for m := range out {
			restored[m] = append(restored[m], out[m]...)
		}
	}
	for m, i := range missing {
		if want := padTo(inputs[i], sliceSize); !bytes.Equal(restored[m], want) {
			t.Fatalf("streamed restore of input %d differs", i)
		}
	}
}

func TestNewDecoder_RejectsInvalidParameters(t *testing.T) {
	cases := []struct {
		name       string
		inputCount int
		missing    []int
		exps       []uint32
	}{
		{"no inputs", 0, nil, nil},
		{"more inputs than PAR2 allows", gf16.MaxInputSlices + 1, []int{0}, []uint32{0}},
		{"fewer recovery slices than missing inputs", 5, []int{0, 1}, []uint32{0}},
		{"negative index", 5, []int{-1}, []uint32{0}},
		{"index past the end", 5, []int{5}, []uint32{0}},
		{"index listed twice", 5, []int{2, 2}, []uint32{0, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := gf16.NewDecoder(c.inputCount, c.missing, c.exps); !errors.Is(err, gf16.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestDecoder_RejectsBadBuffersWithoutWriting(t *testing.T) {
	d, err := gf16.NewDecoder(4, []int{1, 3}, []uint32{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	two := func(a, b int) [][]byte { return [][]byte{make([]byte, a), make([]byte, b)} }
	full := bytes.Repeat([]byte{0xAB}, 8)
	cases := []struct {
		name string
		call func(out [][]byte) error
		out  [][]byte
		want error
	}{
		{"input that is missing", func(o [][]byte) error { return d.AddInput(o, 3, full) }, two(8, 8), gf16.ErrInvalid},
		{"input out of range", func(o [][]byte) error { return d.AddInput(o, 4, full) }, two(8, 8), gf16.ErrInvalid},
		{"recovery out of range", func(o [][]byte) error { return d.AddRecovery(o, 2, full) }, two(8, 8), gf16.ErrInvalid},
		{"wrong number of outputs", func(o [][]byte) error { return d.AddInput(o, 0, full) }, [][]byte{make([]byte, 8)}, gf16.ErrInvalid},
		{"outputs of different lengths", func(o [][]byte) error { return d.AddInput(o, 0, full[:6]) }, two(8, 6), gf16.ErrLength},
		{"odd output length", func(o [][]byte) error { return d.AddRecovery(o, 0, full[:7]) }, two(7, 7), gf16.ErrLength},
		{"chunk longer than outputs", func(o [][]byte) error { return d.AddInput(o, 0, full) }, two(6, 6), gf16.ErrLength},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(c.out); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			for m, o := range c.out {
				if !bytes.Equal(o, make([]byte, len(o))) {
					t.Fatalf("out[%d] modified on error: %x", m, o)
				}
			}
		})
	}
}

func TestSolve_RejectsInconsistentSlices(t *testing.T) {
	cases := []struct {
		name     string
		inputs   [][]byte
		recovery [][]byte
		want     error
	}{
		{"recovery count differs from exponents", [][]byte{nil, {1, 2}}, [][]byte{{0, 0}, {0, 0}}, gf16.ErrInvalid},
		{"odd slice size", [][]byte{nil, {1}}, [][]byte{{0, 0, 0}}, gf16.ErrLength},
		{"input longer than the slice size", [][]byte{nil, {1, 2, 3, 4}}, [][]byte{{0, 0}}, gf16.ErrLength},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := gf16.Solve(c.inputs, []int{0}, []uint32{0}, c.recovery); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	t.Run("recovery slices of different lengths", func(t *testing.T) {
		inputs := [][]byte{nil, nil, {1, 2}}
		_, err := gf16.Solve(inputs, []int{0, 1}, []uint32{0, 1}, [][]byte{{0, 0}, {0, 0, 0, 0}})
		if !errors.Is(err, gf16.ErrLength) {
			t.Fatalf("err = %v, want ErrLength", err)
		}
	})
}
