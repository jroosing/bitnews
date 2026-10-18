package gf16

import (
	"encoding/binary"
	"errors"
)

// ErrLength is returned when buffers have an odd length or lengths that do
// not match.
var ErrLength = errors.New("gf16: buffer lengths must be even and equal")

// mulTable holds the split multiply tables for one coefficient c:
// c·w = lo[w&0xFF] ^ hi[w>>8] for any 16-bit word w.
type mulTable struct {
	lo, hi [256]uint16
}

func newMulTable(c uint16) *mulTable {
	t := new(mulTable)
	for b := range 256 {
		t.lo[b] = Mul(c, uint16(b))
		t.hi[b] = Mul(c, uint16(b)<<8)
	}
	return t
}

// mulAdd computes dst ^= c·src word-wise. len(src) must be even and at most
// len(dst).
func (t *mulTable) mulAdd(dst, src []byte) {
	lo, hi := &t.lo, &t.hi
	n := len(src) &^ 7
	dst = dst[:len(src)]
	for i := 0; i < n; i += 8 {
		s := src[i : i+8 : i+8]
		v := uint64(lo[s[0]]^hi[s[1]]) |
			uint64(lo[s[2]]^hi[s[3]])<<16 |
			uint64(lo[s[4]]^hi[s[5]])<<32 |
			uint64(lo[s[6]]^hi[s[7]])<<48
		d := dst[i : i+8 : i+8]
		binary.LittleEndian.PutUint64(d, binary.LittleEndian.Uint64(d)^v)
	}
	for i := n; i+1 < len(src); i += 2 {
		w := lo[src[i]] ^ hi[src[i+1]]
		dst[i] ^= byte(w)
		dst[i+1] ^= byte(w >> 8)
	}
}

// MulAdd computes dst ^= c·src, treating both as sequences of 16-bit
// little-endian words. Both buffers must have the same even length;
// otherwise it returns ErrLength and leaves dst untouched.
func MulAdd(dst, src []byte, c uint16) error {
	if len(dst) != len(src) || len(src)%2 != 0 {
		return ErrLength
	}
	newMulTable(c).mulAdd(dst, src)
	return nil
}
