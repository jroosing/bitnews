package gf16

import "errors"

// ErrSingular is returned when the chosen recovery slices cannot recover the
// missing inputs because their matrix has no inverse. Choosing a different
// set of recovery slices may succeed.
var ErrSingular = errors.New("gf16: recovery matrix is singular")

// matrix is a square matrix over GF(2^16), stored row-major.
type matrix struct {
	n int
	v []uint16
}

func newMatrix(n int) matrix { return matrix{n: n, v: make([]uint16, n*n)} }

func (m matrix) row(r int) []uint16 { return m.v[r*m.n : (r+1)*m.n] }

// repairMatrix returns M with M[r][c] = cols[c]^exps[r]: row r is recovery
// slice r and column c is missing input c.
func repairMatrix(cols []uint16, exps []uint32) matrix {
	m := newMatrix(len(cols))
	for r, e := range exps {
		row := m.row(r)
		for c, x := range cols {
			row[c] = Pow(x, e)
		}
	}
	return m
}

// invert returns the inverse of m using Gauss-Jordan elimination, or
// ErrSingular. m is overwritten.
func (m matrix) invert() (matrix, error) {
	n := m.n
	inv := newMatrix(n)
	for i := range n {
		inv.row(i)[i] = 1
	}
	for col := range n {
		p := col
		for p < n && m.row(p)[col] == 0 {
			p++
		}
		if p == n {
			return matrix{}, ErrSingular
		}
		if p != col {
			swapRows(m.row(p), m.row(col))
			swapRows(inv.row(p), inv.row(col))
		}
		s, _ := Inv(m.row(col)[col])
		scaleRow(m.row(col), s)
		scaleRow(inv.row(col), s)
		for r := range n {
			if f := m.row(r)[col]; r != col && f != 0 {
				addScaledRow(m.row(r), m.row(col), f)
				addScaledRow(inv.row(r), inv.row(col), f)
			}
		}
	}
	return inv, nil
}

func swapRows(a, b []uint16) {
	for i := range a {
		a[i], b[i] = b[i], a[i]
	}
}

func scaleRow(a []uint16, s uint16) {
	for i := range a {
		a[i] = Mul(a[i], s)
	}
}

// addScaledRow computes dst += f·src.
func addScaledRow(dst, src []uint16, f uint16) {
	for i := range dst {
		dst[i] ^= Mul(src[i], f)
	}
}
