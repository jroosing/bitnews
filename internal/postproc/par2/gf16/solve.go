package gf16

import "fmt"

// solveChunk is the byte range Solve decodes per pass, so the data touched
// per pass stays cache- and memory-friendly.
const solveChunk = 1 << 20

// Solve recovers missing input slices entirely in memory. It is a convenience
// over Decoder for callers that already hold every slice.
//
// inputs holds every input slice of the set by index; entries listed in
// missing are ignored and may be nil. Present inputs may be shorter than the
// slice size and are treated as zero-padded. recovery[r] is the recovery
// slice with exponent exponents[r]; all recovery slices share the slice size,
// which must be even. It returns the restored inputs, padded to the slice
// size, in the order of missing. Work proceeds in chunks of 1 MiB across
// all slices.
func Solve(inputs [][]byte, missing []int, exponents []uint32, recovery [][]byte) ([][]byte, error) {
	if len(recovery) != len(exponents) {
		return nil, fmt.Errorf("%w: %d recovery slices but %d exponents", ErrInvalid, len(recovery), len(exponents))
	}
	d, err := NewDecoder(len(inputs), missing, exponents)
	if err != nil {
		return nil, err
	}
	size := 0
	if len(recovery) > 0 {
		size = len(recovery[0])
	}
	for _, r := range recovery {
		if len(r) != size || size%2 != 0 {
			return nil, fmt.Errorf("%w: recovery slices must share one even length", ErrLength)
		}
	}
	for i, in := range inputs {
		if !d.isMissing[i] && len(in) > size {
			return nil, fmt.Errorf("%w: input %d is longer than the slice size %d", ErrLength, i, size)
		}
	}

	out := make([][]byte, len(missing))
	for m := range out {
		out[m] = make([]byte, size)
	}
	part := make([][]byte, len(out))
	for lo := 0; lo < size; lo += solveChunk {
		hi := min(lo+solveChunk, size)
		for m := range out {
			part[m] = out[m][lo:hi]
		}
		for r, rec := range recovery {
			if err := d.AddRecovery(part, r, rec[lo:hi]); err != nil {
				return nil, err
			}
		}
		for i, in := range inputs {
			if d.isMissing[i] || len(in) <= lo {
				continue
			}
			if err := d.AddInput(part, i, in[lo:min(hi, len(in))]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
