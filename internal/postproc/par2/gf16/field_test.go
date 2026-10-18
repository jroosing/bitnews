package gf16_test

import (
	mrand "math/rand"
	"math/rand/v2"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

// refMul is an independent reference: carry-less multiply reduced modulo the
// PAR2 generator polynomial x^16 + x^12 + x^3 + x + 1 (0x1100B), bit by bit.
func refMul(a, b uint16) uint16 {
	var p uint32
	x := uint32(a)
	for i := range 16 {
		if b&(1<<i) != 0 {
			p ^= x << i
		}
	}
	for bit := 31; bit >= 16; bit-- {
		if p&(1<<bit) != 0 {
			p ^= 0x1100B << (bit - 16)
		}
	}
	return uint16(p)
}

func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15)) }

func randElem(r *rand.Rand) uint16 { return uint16(r.Uint32()) }

func TestMul_KnownAnswers(t *testing.T) {
	cases := []struct{ a, b, want uint16 }{
		{0, 0x1234, 0},
		{1, 0xBEEF, 0xBEEF},
		{2, 0x8000, 0x100B}, // x * x^15 = x^16 = x^12 + x^3 + x + 1
		{0x8000, 0x8000, refMul(0x8000, 0x8000)},
	}
	for _, c := range cases {
		if got := gf16.Mul(c.a, c.b); got != c.want {
			t.Errorf("Mul(%#04x, %#04x) = %#04x, want %#04x", c.a, c.b, got, c.want)
		}
	}
}

func TestMul_MatchesBitwiseReference(t *testing.T) {
	r := newRand(1)
	for range 200000 {
		a, b := randElem(r), randElem(r)
		if got, want := gf16.Mul(a, b), refMul(a, b); got != want {
			t.Fatalf("Mul(%#04x, %#04x) = %#04x, want %#04x", a, b, got, want)
		}
	}
}

func TestInv_MultipliesToOneForEveryNonZeroElement(t *testing.T) {
	for a := 1; a < 1<<16; a++ {
		inv, ok := gf16.Inv(uint16(a))
		if !ok {
			t.Fatalf("Inv(%#04x) reported not invertible", a)
		}
		if got := gf16.Mul(uint16(a), inv); got != 1 {
			t.Fatalf("Mul(%#04x, Inv) = %#04x, want 1", a, got)
		}
	}
}

func TestInv_ZeroHasNoInverse(t *testing.T) {
	if _, ok := gf16.Inv(0); ok {
		t.Fatal("Inv(0) reported ok")
	}
}

func TestDiv_UndoesMul(t *testing.T) {
	r := newRand(2)
	for range 100000 {
		a, b := randElem(r), randElem(r)|1
		q, ok := gf16.Div(gf16.Mul(a, b), b)
		if !ok || q != a {
			t.Fatalf("Div(Mul(%#04x, %#04x), %#04x) = %#04x, %v; want %#04x, true", a, b, b, q, ok, a)
		}
	}
}

func TestDiv_ByZeroReportsNotOK(t *testing.T) {
	if _, ok := gf16.Div(7, 0); ok {
		t.Fatal("Div(7, 0) reported ok")
	}
}

func TestPow_EqualsRepeatedMul(t *testing.T) {
	r := newRand(3)
	for range 2000 {
		a := randElem(r)
		want := uint16(1)
		for e := range uint32(70) {
			if got := gf16.Pow(a, e); got != want {
				t.Fatalf("Pow(%#04x, %d) = %#04x, want %#04x", a, e, got, want)
			}
			want = gf16.Mul(want, a)
		}
	}
}

func TestPow_GroupOrderExponentIsOne(t *testing.T) {
	// Every non-zero element satisfies a^65535 = 1, and exponents wrap
	// modulo the group order.
	r := newRand(4)
	for range 10000 {
		a := randElem(r) | 1
		if got := gf16.Pow(a, 65535); got != 1 {
			t.Fatalf("Pow(%#04x, 65535) = %#04x, want 1", a, got)
		}
		e := r.Uint32()
		if got, want := gf16.Pow(a, e), gf16.Pow(a, e%65535); got != want {
			t.Fatalf("Pow(%#04x, %d) = %#04x, want %#04x", a, e, got, want)
		}
	}
}

func TestPow_ZeroBase(t *testing.T) {
	if got := gf16.Pow(0, 0); got != 1 {
		t.Errorf("Pow(0, 0) = %d, want 1", got)
	}
	if got := gf16.Pow(0, 65535); got != 0 {
		t.Errorf("Pow(0, 65535) = %d, want 0", got)
	}
}

func TestExpLog_AreInverses(t *testing.T) {
	for a := 1; a < 1<<16; a++ {
		l, ok := gf16.Log(uint16(a))
		if !ok {
			t.Fatalf("Log(%#04x) reported not ok", a)
		}
		if got := gf16.Exp(uint32(l)); got != uint16(a) {
			t.Fatalf("Exp(Log(%#04x)) = %#04x", a, got)
		}
	}
	for n := range uint32(65535) {
		l, _ := gf16.Log(gf16.Exp(n))
		if uint32(l) != n {
			t.Fatalf("Log(Exp(%d)) = %d", n, l)
		}
	}
}

func TestExp_IsPowersOfTwo(t *testing.T) {
	r := newRand(5)
	for range 10000 {
		e := r.Uint32()
		if got, want := gf16.Exp(e), gf16.Pow(2, e); got != want {
			t.Fatalf("Exp(%d) = %#04x, want Pow(2, %d) = %#04x", e, got, e, want)
		}
	}
}

func TestLog_ZeroReportsNotOK(t *testing.T) {
	if _, ok := gf16.Log(0); ok {
		t.Fatal("Log(0) reported ok")
	}
}

// newMathRand returns a deterministic math/rand source for testing/quick.
func newMathRand(seed uint64) *mrand.Rand { return mrand.New(mrand.NewSource(int64(seed))) }
