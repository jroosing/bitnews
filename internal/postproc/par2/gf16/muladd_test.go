package gf16_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

func randBytes(r interface{ Uint32() uint32 }, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// wordMulAdd is the obvious reference: dst ^= c·src per little-endian word.
func wordMulAdd(dst, src []byte, c uint16) {
	for i := 0; i+1 < len(src); i += 2 {
		w := binary.LittleEndian.Uint16(dst[i:]) ^ refMul(c, binary.LittleEndian.Uint16(src[i:]))
		binary.LittleEndian.PutUint16(dst[i:], w)
	}
}

func TestMulAdd_AccumulatesProductPerLittleEndianWord(t *testing.T) {
	r := newRand(20)
	for _, size := range []int{0, 2, 6, 8, 14, 16, 1000, 4098} {
		for _, c := range []uint16{0, 1, 2, 0x100B, randElem(r), randElem(r)} {
			src := randBytes(r, size)
			dst := randBytes(r, size)
			want := bytes.Clone(dst)
			wordMulAdd(want, src, c)
			if err := gf16.MulAdd(dst, src, c); err != nil {
				t.Fatalf("size %d c %#04x: %v", size, c, err)
			}
			if !bytes.Equal(dst, want) {
				t.Fatalf("size %d c %#04x: dst mismatch", size, c)
			}
		}
	}
}

func TestMulAdd_RejectsBadLengthsWithoutWriting(t *testing.T) {
	cases := []struct {
		name     string
		dst, src int
	}{
		{"odd", 3, 3},
		{"src shorter", 4, 2},
		{"src longer", 2, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst := make([]byte, c.dst)
			src := bytes.Repeat([]byte{0xFF}, c.src)
			err := gf16.MulAdd(dst, src, 7)
			if !errors.Is(err, gf16.ErrLength) {
				t.Fatalf("err = %v, want ErrLength", err)
			}
			if !bytes.Equal(dst, make([]byte, c.dst)) {
				t.Fatalf("dst modified on error: %x", dst)
			}
		})
	}
}

func TestMulAddParallel_MatchesMulAdd(t *testing.T) {
	r := newRand(21)
	for _, size := range []int{0, 2, 4096, 1<<20 + 6, 3<<20 + 2} {
		c := randElem(r)
		src := randBytes(r, size)
		dst := randBytes(r, size)
		want := bytes.Clone(dst)
		if err := gf16.MulAdd(want, src, c); err != nil {
			t.Fatal(err)
		}
		if err := gf16.MulAddParallel(dst, src, c); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(dst, want) {
			t.Fatalf("size %d: parallel result differs from MulAdd", size)
		}
	}
}

func TestMulAddParallel_RejectsBadLengths(t *testing.T) {
	if err := gf16.MulAddParallel(make([]byte, 4), make([]byte, 6), 3); !errors.Is(err, gf16.ErrLength) {
		t.Fatalf("err = %v, want ErrLength", err)
	}
}
