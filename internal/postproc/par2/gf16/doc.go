// Package gf16 implements arithmetic in GF(2^16) and the PAR2 Reed-Solomon
// math used by Repair.
//
// # Field
//
// The field is GF(2^16) with the PAR2 generator polynomial 0x1100B
// (x^16 + x^12 + x^3 + x + 1) and generator element 2. Addition is XOR.
// Mul, Div, Inv, Pow, Exp and Log use log/antilog tables with 65 536 entries,
// built once at package initialisation. Operations without a result (Inv(0),
// Div by 0, Log(0)) report false instead of panicking.
//
// # PAR2 Reed-Solomon
//
// Input slice i has constant c_i = 2^(n_i), where n_i is the i-th integer
// n ≥ 0 that is not a multiple of 3, 5, 17 or 257 (see InputConstants). The
// recovery slice with exponent e is Σ_i c_i^e · input_i, computed over 16-bit
// little-endian words with every input zero-padded to the slice size.
//
// To Repair k missing inputs from k recovery slices, NewDecoder builds the
// k×k matrix M[r][c] = c_missing[c]^e_r and inverts it by Gauss-Jordan
// elimination, returning ErrSingular when that choice of recovery slices
// cannot work (Repair should then try another choice). The Decoder then
// computes missing = M⁻¹·(recovery − Σ_known c_j^e·input_j) one chunk at a
// time, so memory stays bounded by chunk size × k whatever the slice size.
// Solve is an in-memory convenience on top of it.
//
// The kernel is MulAdd (dst ^= c·src) with split multiply tables: for each
// coefficient, 256 products for the low byte and 256 for the high byte of a
// word. Decoding spreads it over goroutines, by missing slice or by byte
// range. There is no assembly.
//
// # Verification
//
// Tests compare Mul against an independent bitwise carry-less multiply
// reduced by 0x1100B, check the field axioms as properties, and round-trip a
// test-only encoder through Solve. The parent package's golden tests (M3-06)
// compare every recovery slice written by par2cmdline 1.4.0 byte for byte
// against InputConstants, Pow and MulAdd, over 12 fixture sets.
//
// # Benchmarks
//
// Apple M4 (10 cores), go1.27.2, darwin/arm64, measured on a shared machine
// (load average ~3):
//
//	BenchmarkMulAdd                 1 MiB                        5.2 GB/s (one core)
//	BenchmarkMulAddParallel         1 MiB                       14.3 GB/s
//	BenchmarkSolve/100x64KiB        10 missing of 100 inputs     5.7 ms/op, 1.15 GB/s
//	BenchmarkSolve/100x1024KiB      10 missing of 100 inputs    47 ms/op,   2.2 GB/s
//	BenchmarkNewDecoder/k=10        32768 inputs                 0.057 ms/op
//	BenchmarkNewDecoder/k=100       32768 inputs                 1.2 ms/op
//	BenchmarkNewDecoder/k=500       32768 inputs               141 ms/op
//
// Solve throughput is the size of the whole input set divided by the time;
// it does k multiply-accumulates per input byte (about 22 GB/s of kernel work
// at 1 MiB slices). Solve uses 1 MiB chunks: 64 KiB and 256 KiB chunks were
// 30–50% slower because the per-call setup is amortised over less data.
// Matrix inversion is O(k³) and single-threaded, so very large repairs
// (k in the thousands) spend seconds there.
package gf16
