package par2_test

import (
	"context"
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

// Block size and recovery count of the test sets: small, so par2 is fast.
const blockSize = 1000

// toolFor returns a Repairer that runs the pinned par2.
func toolFor(t testing.TB) par2.Repairer {
	t.Helper()
	return par2.Repairer{Path: tooltest.Path(t, "par2"), Threads: 2, MemoryMB: 64}
}

// payload returns n deterministic pseudo-random bytes.
func payload(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, 9))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// set is a PAR2 set made by the real par2 in a temporary directory.
type set struct {
	dir   string
	files map[string][]byte // original contents by name
}

// makeSet writes the files (name -> size; sizes seed the contents), runs
// `par2 c` with blockSize and the given number of recovery blocks in two
// volumes, and returns the set. Names may hold a subdirectory.
func makeSet(t testing.TB, recovery int, sizes map[string]int) *set {
	t.Helper()
	dir := t.TempDir()
	s := &set{dir: dir, files: map[string][]byte{}}
	var args []string
	seed := uint64(1)
	names := make([]string, 0, len(sizes))
	for n := range sizes {
		names = append(names, n)
	}
	// stable order
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		b := payload(seed, sizes[name])
		seed++
		s.files[name] = b
		s.write(t, name, b)
		args = append(args, s.path(name)) // absolute, so names are relative to -B
	}
	create := append([]string{"c", "-B" + dir, "-s" + strconv.Itoa(blockSize), "-c" + strconv.Itoa(recovery), "-n2", "-q", "--", "set.par2"}, args...)
	res, err := tool.Run(context.Background(), tool.Spec{Path: tooltest.Path(t, "par2"), Args: create, Dir: dir})
	require.NoError(t, err)
	require.Zero(t, res.ExitCode, string(res.Stdout))
	return s
}

func (s *set) path(name string) string { return filepath.Join(s.dir, name) }

func (s *set) write(t testing.TB, name string, b []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(s.path(name)), 0o755))
	require.NoError(t, os.WriteFile(s.path(name), b, 0o644))
}

func (s *set) read(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(s.path(name))
	require.NoError(t, err)
	return b
}

// corrupt flips one byte at each offset of the file.
func (s *set) corrupt(t testing.TB, name string, offsets ...int) {
	t.Helper()
	b := s.read(t, name)
	for _, o := range offsets {
		b[o] ^= 0xff
	}
	s.write(t, name, b)
}

func (s *set) truncate(t testing.TB, name string, size int64) {
	t.Helper()
	require.NoError(t, os.Truncate(s.path(name), size))
}

func (s *set) remove(t testing.TB, name string) {
	t.Helper()
	require.NoError(t, os.Remove(s.path(name)))
}

func (s *set) rename(t testing.TB, from, to string) {
	t.Helper()
	require.NoError(t, os.Rename(s.path(from), s.path(to)))
}

// removeVolumes deletes every recovery volume, leaving the index file.
func (s *set) removeVolumes(t testing.TB) {
	t.Helper()
	vols, err := filepath.Glob(s.path("set.vol*.par2"))
	require.NoError(t, err)
	require.NotEmpty(t, vols)
	for _, v := range vols {
		require.NoError(t, os.Remove(v))
	}
}

// listing returns the names of the regular files below the set's directory.
func (s *set) listing(t testing.TB) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(s.dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	}))
	return out
}

// requireOriginals checks that every original file has its original bytes.
func (s *set) requireOriginals(t testing.TB) {
	t.Helper()
	for name, want := range s.files {
		require.Equal(t, want, s.read(t, name), name)
	}
}
