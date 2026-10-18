package gf16

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrInvalid is returned for decoder parameters or buffers that do not
// describe a valid repair.
var ErrInvalid = errors.New("gf16: invalid repair parameters")

// Decoder reconstructs missing input slices of one PAR2 set from a fixed
// choice of recovery slices. It is immutable after NewDecoder and safe for
// concurrent use, provided concurrent calls write to distinct output buffers.
//
// Decoding is streamed: the caller decodes the slices one chunk (a byte range
// at the same offset in every slice) at a time. For each chunk it zeroes one
// output buffer per missing input, then passes the chunk of every chosen
// recovery slice to AddRecovery and the chunk of every present input slice
// to AddInput, in any order. The output buffers then hold that chunk of the
// missing inputs. Memory is bounded by the chunk size times the number of
// missing inputs, independent of the slice size. Each call costs k split-table
// builds and k² multiplies on top of the data work, so chunks of about 1 MiB
// keep that overhead small.
//
// With R_r the recovery slices, M[r][c] = c_missing[c]^e_r and x the missing
// inputs, the decoder computes x = M⁻¹·(R − Σ_known c_j^e·input_j).
type Decoder struct {
	consts    []uint16 // input constants c_0 … c_{n-1}
	exps      []uint32 // exponent of each chosen recovery slice
	isMissing []bool   // per input
	inv       matrix   // M⁻¹: inv[m][r] weighs recovery r for missing input m
}

// NewDecoder prepares to recover the inputs with the given indexes (0-based,
// distinct, < inputCount) from the recovery slices with the given exponents,
// one per missing input. inputCount must not exceed MaxInputSlices. It returns
// ErrSingular when that choice of recovery slices cannot recover these
// inputs, and an error wrapping ErrInvalid for inconsistent parameters.
func NewDecoder(inputCount int, missing []int, exponents []uint32) (*Decoder, error) {
	if inputCount < 1 || inputCount > MaxInputSlices {
		return nil, fmt.Errorf("%w: input count %d not in [1, %d]", ErrInvalid, inputCount, MaxInputSlices)
	}
	if len(missing) != len(exponents) {
		return nil, fmt.Errorf("%w: %d missing inputs but %d recovery slices", ErrInvalid, len(missing), len(exponents))
	}
	d := &Decoder{
		consts:    InputConstants(inputCount),
		exps:      append([]uint32(nil), exponents...),
		isMissing: make([]bool, inputCount),
	}
	cols := make([]uint16, len(missing))
	for c, i := range missing {
		if i < 0 || i >= inputCount {
			return nil, fmt.Errorf("%w: missing input %d out of range", ErrInvalid, i)
		}
		if d.isMissing[i] {
			return nil, fmt.Errorf("%w: missing input %d listed twice", ErrInvalid, i)
		}
		d.isMissing[i] = true
		cols[c] = d.consts[i]
	}
	inv, err := repairMatrix(cols, exponents).invert()
	if err != nil {
		return nil, err
	}
	d.inv = inv
	return d, nil
}

// AddRecovery accumulates the contribution of a chunk of recovery slice r
// (the r-th exponent given to NewDecoder) into out, one buffer per missing
// input in NewDecoder order. See AddInput for the buffer rules.
func (d *Decoder) AddRecovery(out [][]byte, r int, chunk []byte) error {
	if r < 0 || r >= len(d.exps) {
		return fmt.Errorf("%w: recovery slice %d out of range", ErrInvalid, r)
	}
	coefs := make([]uint16, d.inv.n)
	for m := range coefs {
		coefs[m] = d.inv.row(m)[r]
	}
	return accumulate(out, chunk, coefs)
}

// AddInput accumulates the contribution of a chunk of present input slice i
// into out, one buffer per missing input in NewDecoder order.
//
// All out buffers must have the same even length and chunk must be no longer
// than them. A shorter chunk is treated as zero-padded, which matches PAR2's
// padding of a file's last slice. Chunks must start at an even offset within
// their slice. On error, out is left untouched.
func (d *Decoder) AddInput(out [][]byte, i int, chunk []byte) error {
	if i < 0 || i >= len(d.consts) || d.isMissing[i] {
		return fmt.Errorf("%w: input %d is not a present input", ErrInvalid, i)
	}
	// The input enters recovery r with weight c_i^e_r, and recovery r enters
	// missing input m with weight inv[m][r].
	pows := make([]uint16, len(d.exps))
	for r, e := range d.exps {
		pows[r] = Pow(d.consts[i], e)
	}
	coefs := make([]uint16, d.inv.n)
	for m := range coefs {
		var s uint16
		for r, w := range d.inv.row(m) {
			s ^= Mul(w, pows[r])
		}
		coefs[m] = s
	}
	return accumulate(out, chunk, coefs)
}

// accumulate computes out[m] ^= coefs[m]·chunk for every m. The work is
// spread over goroutines: by output buffer when there are enough of them,
// otherwise by byte range of the chunk.
func accumulate(out [][]byte, chunk []byte, coefs []uint16) error {
	if len(out) != len(coefs) {
		return fmt.Errorf("%w: %d output buffers for %d missing inputs", ErrInvalid, len(out), len(coefs))
	}
	if len(out) == 0 {
		return nil
	}
	size := len(out[0])
	for _, o := range out {
		if len(o) != size || size%2 != 0 {
			return fmt.Errorf("%w: output buffers must share one even length", ErrLength)
		}
	}
	if len(chunk) > size {
		return fmt.Errorf("%w: chunk of %d bytes exceeds output of %d", ErrLength, len(chunk), size)
	}
	tables := make([]*mulTable, len(coefs))
	for m, c := range coefs {
		if c != 0 {
			tables[m] = newMulTable(c)
		}
	}
	even := len(chunk) &^ 1
	addRange := func(m, lo, hi int) {
		if t := tables[m]; t != nil {
			t.mulAdd(out[m][lo:hi], chunk[lo:hi])
		}
	}
	workers := min(runtime.GOMAXPROCS(0), len(tables)*even/minParallelRange)
	if workers > 1 && len(tables) >= workers {
		parallelItems(len(tables), workers, func(m int) { addRange(m, 0, even) })
	} else {
		parallelRanges(even, func(lo, hi int) {
			for m := range tables {
				addRange(m, lo, hi)
			}
		})
	}
	if even < len(chunk) {
		// Trailing odd byte: the low byte of a word whose high byte is padding.
		b := chunk[even]
		for m, t := range tables {
			if t != nil {
				w := t.lo[b]
				out[m][even] ^= byte(w)
				out[m][even+1] ^= byte(w >> 8)
			}
		}
	}
	return nil
}
