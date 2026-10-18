package unpack

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// sliceSource serves synthetic entries, so the guards and the extractor can be
// tested without a real archive.
type sliceSource struct {
	entries []*entry
	i       int
}

func (s *sliceSource) next() (*entry, error) {
	if s.i >= len(s.entries) {
		return nil, io.EOF
	}
	e := s.entries[s.i]
	s.i++
	return e, nil
}

func (s *sliceSource) close() error { return nil }

func opener(entries ...*entry) openFunc {
	return func() (source, error) { return &sliceSource{entries: entries}, nil }
}

func fileEntry(name, content string) *entry {
	return &entry{
		name: name, kind: kindFile, size: int64(len(content)),
		open: func() (io.Reader, error) { return bytes.NewReader([]byte(content)), nil },
	}
}

func dirEntry(name string) *entry { return &entry{name: name, kind: kindDir} }

func otherEntry(name string, kind entryKind) *entry { return &entry{name: name, kind: kind} }

// seam is a destination next to which the test can see everything the unpack
// touches: dst lives in its own otherwise empty parent.
type seam struct {
	parent string
	dst    string
}

func newSeam(t *testing.T) seam {
	t.Helper()
	parent := t.TempDir()
	return seam{parent: parent, dst: filepath.Join(parent, "out")}
}

func (s seam) run(t *testing.T, open openFunc, opts ...Option) (Result, error) {
	t.Helper()
	return s.runPacked(t, 1, open, opts...)
}

// runPacked is run for an archive of the given size on disk.
func (s seam) runPacked(t *testing.T, packed int64, open openFunc, opts ...Option) (Result, error) {
	t.Helper()
	return unpackWith(context.Background(), s.dst, open, packed, newConfig(append([]Option{WithLogger(nil)}, opts...)))
}

// listing returns every path below dir (relative, slash separated), sorted.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(out)
	return out
}

func TestUnpackWithExtractsFilesAndDirectoriesIntoDst(t *testing.T) {
	s := newSeam(t)

	res, err := s.run(t, opener(
		dirEntry("sub"),
		fileEntry("sub/a.txt", "alpha"),
		fileEntry("b.txt", "bravo!"),
	))

	require.NoError(t, err)
	require.Equal(t, []string{"b.txt", "sub", "sub/a.txt"}, listing(t, s.dst))
	got, err := os.ReadFile(filepath.Join(s.dst, "sub", "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha", string(got))
	require.Equal(t, []string{"b.txt", "sub/a.txt"}, res.Files)
	require.EqualValues(t, 11, res.Bytes)
	require.False(t, res.Nested)
	require.Equal(t, []string{"out"}, children(t, s.parent), "staging is gone after the rename")
}

// children returns the names directly inside dir, sorted.
func children(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := []string{}
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}
