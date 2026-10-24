package adapt

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/postproc/par2"
	"github.com/jroosing/bitnews/internal/postproc/tool"
	"github.com/jroosing/bitnews/internal/postproc/tool/tooltest"
)

// par2Tool is the pinned par2 for the adapters under test.
func par2Tool(t testing.TB) par2.Tool {
	t.Helper()
	return par2.Tool{Path: tooltest.Path(t, "par2"), Threads: 2, MemoryMB: 64}
}

func verifier(t testing.TB) Verifier { return Verifier{Par2: par2Tool(t)} }
func repairer(t testing.TB) Repairer { return Repairer{Par2: par2Tool(t)} }

// testSet is a PAR2 set made by the real par2 in a temporary directory.
type testSet struct {
	dir   string
	files map[string][]byte
}

// makeSet writes the files (name -> size) and runs `par2 c` with 1000 byte
// blocks and the given number of recovery blocks.
func makeSet(t testing.TB, recovery int, sizes map[string]int) *testSet {
	t.Helper()
	s := &testSet{dir: t.TempDir(), files: map[string][]byte{}}
	args := []string{"c", "-B" + s.dir, "-s1000", "-c" + strconv.Itoa(recovery), "-n2", "-q", "--", "set.par2"}
	seed := uint64(1)
	for name, size := range sizes {
		r := rand.New(rand.NewPCG(seed, 5))
		seed++
		b := make([]byte, size)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		s.files[name] = b
		require.NoError(t, os.WriteFile(s.path(name), b, 0o644))
		args = append(args, s.path(name))
	}
	res, err := tool.Run(context.Background(), tool.Spec{Path: tooltest.Path(t, "par2"), Args: args, Dir: s.dir})
	require.NoError(t, err)
	require.Zero(t, res.ExitCode, string(res.Stdout))
	return s
}

func (s *testSet) path(name string) string { return filepath.Join(s.dir, name) }

// flipByte flips every bit of the byte at offset in the file.
func (s *testSet) flipByte(t testing.TB, name string, offset int) {
	t.Helper()
	b, err := os.ReadFile(s.path(name))
	require.NoError(t, err)
	b[offset] ^= 0xff
	require.NoError(t, os.WriteFile(s.path(name), b, 0o644))
}

// removePAR2 deletes every .par2 file.
func (s *testSet) removePAR2(t testing.TB) {
	t.Helper()
	m, err := filepath.Glob(s.path("*.par2"))
	require.NoError(t, err)
	for _, p := range m {
		require.NoError(t, os.Remove(p))
	}
}

// requireOriginals checks that every file has its original bytes.
func (s *testSet) requireOriginals(t testing.TB) {
	t.Helper()
	for name, want := range s.files {
		got, err := os.ReadFile(s.path(name))
		require.NoError(t, err)
		require.Equal(t, want, got, name)
	}
}

// digests maps every file name in dir to its MD5.
func digests(t testing.TB, dir string) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string]string{}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		sum := md5.Sum(b)
		out[e.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

// patchName replaces name by repl (same length) inside the File Description
// packet of a .par2 file and seals the packet again with a valid MD5, which is
// how a hostile uploader would write the name.
func patchName(t testing.TB, path, name, repl string) {
	t.Helper()
	require.Len(t, repl, len(name))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	at := bytes.Index(b, []byte(name))
	require.GreaterOrEqual(t, at, 0)
	start := bytes.LastIndex(b[:at], []byte("PAR2\x00PKT"))
	require.GreaterOrEqual(t, start, 0)
	length := int(binary.LittleEndian.Uint64(b[start+8:]))
	copy(b[at:], repl)
	sum := md5.Sum(b[start+32 : start+length])
	copy(b[start+16:], sum[:])
	require.NoError(t, os.WriteFile(path, b, 0o644))
}
