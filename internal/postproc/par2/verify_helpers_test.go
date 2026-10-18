package par2_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

// vtEnv is a download directory holding the set's data files plus the loaded
// Set. The .par2 files live in a separate directory so they never show up as
// Verify candidates.
type vtEnv struct {
	t   *testing.T
	Dir string
	Set *par2.Set
	TS  *testSet
}

// newVTEnv writes every file into a fresh data dir and loads a set with
// recovery slices for the given exponents.
func newVTEnv(t *testing.T, sliceSize int, files []fileSpec, exponents ...uint32) *vtEnv {
	t.Helper()
	ts := newSet(sliceSize, files...)
	return finishVTEnv(t, ts, concat(ts.Index(), ts.Volume(exponents...)), files)
}

// newVTEnvNoIFSC is like newVTEnv but the par2 data has no IFSC packets.
func newVTEnvNoIFSC(t *testing.T, sliceSize int, files []fileSpec) *vtEnv {
	t.Helper()
	ts := newSet(sliceSize, files...)
	idx := ts.creatorPacket("bitnews test builder")
	idx = append(idx, ts.mainPacket()...)
	for i := range files {
		idx = append(idx, ts.fileDescPacket(i)...)
	}
	return finishVTEnv(t, ts, idx, files)
}

// newVTEnvBare writes no data files at all; the caller places them.
func newVTEnvBare(t *testing.T, sliceSize int, files []fileSpec, exponents ...uint32) *vtEnv {
	t.Helper()
	ts := newSet(sliceSize, files...)
	return finishVTEnv(t, ts, concat(ts.Index(), ts.Volume(exponents...)), nil)
}

func finishVTEnv(t *testing.T, ts *testSet, par2data []byte, files []fileSpec) *vtEnv {
	t.Helper()
	dir, parDir := t.TempDir(), t.TempDir()
	for _, f := range files {
		writeFile(t, dir, f.Name, f.Data)
	}
	writeFile(t, parDir, "set.par2", par2data)
	set, err := par2.LoadDir(parDir)
	require.NoError(t, err)
	return &vtEnv{t: t, Dir: dir, Set: set, TS: ts}
}

func (e *vtEnv) path(name string) string { return filepath.Join(e.Dir, name) }

func (e *vtEnv) read(name string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(e.path(name))
	require.NoError(e.t, err)
	return b
}

func (e *vtEnv) write(name string, data []byte) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(e.path(name), data, 0o600))
}

// names lists the directory entries, sorted.
func (e *vtEnv) names() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.Dir)
	require.NoError(e.t, err)
	var out []string
	for _, en := range entries {
		out = append(out, en.Name())
	}
	return out
}

// byName finds the result of the file with the given expected name.
func vtFile(t *testing.T, r *par2.Result, name string) par2.FileResult {
	t.Helper()
	for _, f := range r.Files {
		if f.Name == name {
			return f
		}
	}
	require.Failf(t, "file result not found", "%q in %+v", name, r.Files)
	return par2.FileResult{}
}
