package gf16_test

import (
	"testing"
	"testing/quick"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

// Addition in GF(2^16) is XOR, so the axioms below exercise Mul against it.

func quickConfig(seed uint64) *quick.Config {
	return &quick.Config{MaxCount: 50000, Rand: newMathRand(seed)}
}

func check(t *testing.T, name string, f any, seed uint64) {
	t.Helper()
	if err := quick.Check(f, quickConfig(seed)); err != nil {
		t.Errorf("%s: %v", name, err)
	}
}

func TestFieldAxioms(t *testing.T) {
	check(t, "Mul is commutative", func(a, b uint16) bool {
		return gf16.Mul(a, b) == gf16.Mul(b, a)
	}, 10)
	check(t, "Mul is associative", func(a, b, c uint16) bool {
		return gf16.Mul(gf16.Mul(a, b), c) == gf16.Mul(a, gf16.Mul(b, c))
	}, 11)
	check(t, "Mul distributes over addition", func(a, b, c uint16) bool {
		return gf16.Mul(a, b^c) == gf16.Mul(a, b)^gf16.Mul(a, c)
	}, 12)
	check(t, "1 is the multiplicative identity", func(a uint16) bool {
		return gf16.Mul(a, 1) == a
	}, 13)
	check(t, "0 absorbs", func(a uint16) bool {
		return gf16.Mul(a, 0) == 0
	}, 14)
	check(t, "Pow adds exponents", func(a uint16, m, n uint16) bool {
		return gf16.Pow(a, uint32(m)+uint32(n)) == gf16.Mul(gf16.Pow(a, uint32(m)), gf16.Pow(a, uint32(n)))
	}, 15)
	check(t, "Pow multiplies exponents", func(a uint16, m, n uint16) bool {
		return gf16.Pow(a, uint32(m)*uint32(n)) == gf16.Pow(gf16.Pow(a, uint32(m)), uint32(n))
	}, 16)
	check(t, "Div is Mul by Inv", func(a, b uint16) bool {
		b |= 1
		q, _ := gf16.Div(a, b)
		inv, _ := gf16.Inv(b)
		return q == gf16.Mul(a, inv)
	}, 17)
}
