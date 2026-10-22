package par2_test

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
	"gitlab.com/kyura4k/gitnews/internal/postproc/par2/gf16"
)

// encodeRecovery is a test-only PAR2 Reed-Solomon encoder. It computes the
// true recovery slice for every requested exponent: the sum over all input
// slices of the set (the slices of every file in set order, each file's last
// slice zero-padded) of c_i^e times the slice, over GF(2^16). It is built on
// gf16.InputConstants and gf16.MulAdd only, independent of the Decoder.
func encodeRecovery(t testing.TB, files []fileSpec, sliceSize int, exponents []uint32) map[uint32][]byte {
	t.Helper()
	var inputs [][]byte
	for _, f := range files {
		for off := 0; off < len(f.Data); off += sliceSize {
			s := make([]byte, sliceSize)
			copy(s, f.Data[off:min(off+sliceSize, len(f.Data))])
			inputs = append(inputs, s)
		}
	}
	consts := gf16.InputConstants(len(inputs))
	out := map[uint32][]byte{}
	for _, e := range exponents {
		rec := make([]byte, sliceSize)
		for i, in := range inputs {
			require.NoError(t, gf16.MulAdd(rec, in, gf16.Pow(consts[i], e)))
		}
		out[e] = rec
	}
	return out
}

// realVolume is a recovery volume holding true Reed-Solomon slices.
func realVolume(t testing.TB, ts *testSet, exponents ...uint32) []byte {
	t.Helper()
	recs := encodeRecovery(t, ts.Files, ts.SliceSize, exponents)
	out := ts.mainPacket()
	for _, e := range exponents {
		out = append(out, rawPacket(ts.SetID, typeRecvSlic, recvSlicBody(e, recs[e]))...)
	}
	return out
}

// newRepairEnv writes the files and a par2 set with true recovery slices for
// the given exponents.
func newRepairEnv(t *testing.T, sliceSize int, files []fileSpec, exponents ...uint32) *vtEnv {
	t.Helper()
	ts := newSet(sliceSize, files...)
	return finishVTEnv(t, ts, concat(ts.Index(), realVolume(t, ts, exponents...)), files)
}

// exps returns n consecutive exponents starting at start.
func exps(start uint32, n int) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = start + uint32(i)
	}
	return out
}

// corruptSlice flips bits in every byte of a slice of a file on disk.
func (e *vtEnv) corruptSlice(name string, slice int) {
	e.t.Helper()
	data := e.read(name)
	lo := slice * e.TS.SliceSize
	hi := min(lo+e.TS.SliceSize, len(data))
	for i := lo; i < hi; i++ {
		data[i] ^= 0xA5
	}
	e.write(name, data)
}

// dirDigests hashes every regular file below dir.
func dirDigests(t testing.TB, dir string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, en := range entries {
		b, err := os.ReadFile(filepath.Join(dir, en.Name()))
		require.NoError(t, err)
		out[en.Name()] = sha256.Sum256(b)
	}
	return out
}

func (e *vtEnv) verify() *par2.Result {
	e.t.Helper()
	res, err := par2.Verify(context.Background(), e.Dir, e.Set)
	require.NoError(e.t, err)
	return res
}

// repair verifies, then repairs.
func (e *vtEnv) repair(opts ...par2.RepairOption) (*par2.RepairResult, error) {
	e.t.Helper()
	return par2.Repair(context.Background(), e.Dir, e.Set, e.verify(), opts...)
}

// requireIntact checks every file on disk equals its original bytes.
func (e *vtEnv) requireIntact(files []fileSpec) {
	e.t.Helper()
	for _, f := range files {
		require.True(e.t, string(e.read(f.Name)) == string(f.Data), "%s differs from the original", f.Name)
	}
}
