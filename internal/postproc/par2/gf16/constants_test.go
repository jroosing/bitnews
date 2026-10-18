package gf16_test

import (
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

// refPow2 computes 2^n with the bitwise reference multiply.
func refPow2(n int) uint16 {
	x := uint16(1)
	for range n {
		x = refMul(x, 2)
	}
	return x
}

func TestInputConstants_FollowTheExclusionRule(t *testing.T) {
	// n_i skips multiples of 3, 5, 17 and 257.
	wantExps := []int{1, 2, 4, 7, 8, 11, 13, 14, 16, 19, 22, 23, 26, 28, 29, 31, 32, 37, 38, 41}
	got := gf16.InputConstants(len(wantExps))
	if len(got) != len(wantExps) {
		t.Fatalf("len = %d, want %d", len(got), len(wantExps))
	}
	for i, n := range wantExps {
		if want := refPow2(n); got[i] != want {
			t.Errorf("c_%d = %#04x, want 2^%d = %#04x", i, got[i], n, want)
		}
	}
}

func TestInputConstants_AreDistinctGeneratorsUpToTheSliceLimit(t *testing.T) {
	cs := gf16.InputConstants(gf16.MaxInputSlices)
	if len(cs) != 32768 {
		t.Fatalf("len = %d, want 32768", len(cs))
	}
	seen := make(map[uint16]bool, len(cs))
	for i, c := range cs {
		if seen[c] {
			t.Fatalf("c_%d = %#04x repeats", i, c)
		}
		seen[c] = true
		for _, p := range []uint32{3, 5, 17, 257} {
			if gf16.Pow(c, 65535/p) == 1 {
				t.Fatalf("c_%d = %#04x is not a generator (order divides 65535/%d)", i, c, p)
			}
		}
	}
}

func TestInputConstants_Empty(t *testing.T) {
	if got := gf16.InputConstants(0); len(got) != 0 {
		t.Fatalf("InputConstants(0) = %v, want empty", got)
	}
}
