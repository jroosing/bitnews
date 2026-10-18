package gf16

// MaxInputSlices is the largest number of input slices a PAR2 set can have.
// It equals the number of generators of the multiplicative group, so it is
// also the number of distinct input constants.
const MaxInputSlices = 32768

// InputConstants returns the PAR2 input-slice constants c_0 … c_{n-1}.
//
// c_i = 2^(n_i), where n_i is the i-th integer n ≥ 0 that is not a multiple
// of 3, 5, 17 or 257 (the prime factors of 65535). This keeps every c_i a
// generator of the multiplicative group. Only the first MaxInputSlices
// constants are distinct; n should not exceed it.
func InputConstants(n int) []uint16 {
	cs := make([]uint16, 0, max(n, 0))
	for e := uint32(0); len(cs) < n; e++ {
		if e%3 == 0 || e%5 == 0 || e%17 == 0 || e%257 == 0 {
			continue
		}
		cs = append(cs, Exp(e))
	}
	return cs
}
