package gf16

// poly is the PAR2 generator polynomial x^16 + x^12 + x^3 + x + 1.
const poly = 0x1100B

// order is the size of the multiplicative group: 2^16 - 1.
const order = 65535

var (
	// logTable[a] is the discrete log of a (base 2) for a != 0.
	logTable [1 << 16]uint16
	// expTable[n] is 2^n. It is doubled in length so that the sum of two logs
	// can index it without a modulo.
	expTable [2 * order]uint16
)

func init() {
	x := uint32(1)
	for n := range order {
		expTable[n] = uint16(x)
		expTable[n+order] = uint16(x)
		logTable[x] = uint16(n)
		x <<= 1
		if x&0x10000 != 0 {
			x ^= poly
		}
	}
}

// Mul returns a·b in GF(2^16).
func Mul(a, b uint16) uint16 {
	if a == 0 || b == 0 {
		return 0
	}
	return expTable[int(logTable[a])+int(logTable[b])]
}

// Inv returns the multiplicative inverse of a. It reports false when a is 0,
// which has no inverse.
func Inv(a uint16) (uint16, bool) {
	if a == 0 {
		return 0, false
	}
	return expTable[order-int(logTable[a])], true
}

// Div returns a/b. It reports false when b is 0.
func Div(a, b uint16) (uint16, bool) {
	if b == 0 {
		return 0, false
	}
	if a == 0 {
		return 0, true
	}
	return expTable[int(logTable[a])+order-int(logTable[b])], true
}

// Pow returns a^e. By convention Pow(0, 0) is 1.
func Pow(a uint16, e uint32) uint16 {
	if e == 0 {
		return 1
	}
	if a == 0 {
		return 0
	}
	return expTable[uint64(logTable[a])*uint64(e)%order]
}

// Exp returns 2^n, where 2 is the generator of the multiplicative group.
func Exp(n uint32) uint16 {
	return expTable[n%order]
}

// Log returns the discrete logarithm of a to base 2, in [0, 65535). It
// reports false when a is 0, which has no logarithm.
func Log(a uint16) (uint16, bool) {
	if a == 0 {
		return 0, false
	}
	return logTable[a], true
}
