package unpack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/tool"
	"gitlab.com/kyura4k/gitnews/internal/postproc/tool/tooltest"
)

// writeFile writes content to dir/name and returns the full path.
func writeFile(t testing.TB, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, content, 0o600))
	return p
}

// fakeVolume is a file that carries only a magic header and some padding. It
// is enough for Detect, which never parses the archive.
func fakeVolume(magic []byte) []byte {
	return append(append([]byte{}, magic...), make([]byte, 32)...)
}

// run7zz runs the real 7zz in dir (to build test archives) and requires it to
// succeed. The password, if any, goes through stdin like in production.
func run7zz(t testing.TB, dir string, args ...string) {
	t.Helper()
	run7zzIn(t, dir, "", args...)
}

func run7zzIn(t testing.TB, dir, stdin string, args ...string) {
	t.Helper()
	spec := tool.Spec{Path: tooltest.Path(t, "7zz"), Args: args, Dir: dir}
	if stdin != "" {
		spec.Stdin = strings.NewReader(stdin)
	}
	res, err := tool.Run(context.Background(), spec)
	require.NoError(t, err)
	require.Zero(t, res.ExitCode, "7zz %v: %s%s", args, res.Stdout, res.Stderr)
}

// unpackSet runs Unpack with the pinned 7zz and quiet logging.
func unpackSet(t testing.TB, set ArchiveSet, dst string, opts ...Option) (Result, error) {
	t.Helper()
	return unpackCtx(t, context.Background(), set, dst, opts...)
}

func unpackCtx(t testing.TB, ctx context.Context, set ArchiveSet, dst string, opts ...Option) (Result, error) {
	t.Helper()
	all := append([]Option{WithSevenZip(tooltest.Path(t, "7zz")), WithLogger(nil)}, opts...)
	return Unpack(ctx, set, dst, all...)
}

// copyFixtures copies the named files from testdata/<sub> into a fresh dir.
func copyFixtures(t testing.TB, sub string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("testdata", sub, n))
		require.NoError(t, err)
		writeFile(t, dir, n, b)
	}
	return dir
}

func detectOne(t testing.TB, dir string) ArchiveSet {
	t.Helper()
	sets, err := Detect(dir)
	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.NoError(t, sets[0].Err)
	return sets[0]
}

func readFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// oneSet is an ArchiveSet for a single file; 7zz recognizes the real format
// from the content, so the Format field only matters to Detect.
func oneSet(path string) ArchiveSet {
	return ArchiveSet{Format: Format7z, Name: filepath.Base(path), Volumes: []string{path}}
}

// listing returns every path below dir (relative, slash separated), sorted.
func listing(t testing.TB, dir string) []string {
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

// children returns the names directly inside dir, sorted.
func children(t testing.TB, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := []string{}
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func flip(b []byte, i int) []byte {
	c := append([]byte{}, b...)
	c[i] ^= 0xff
	return c
}

// out is a destination in a fresh, otherwise empty parent: whatever the unpack
// touches shows up in the parent.
type out struct{ parent, dst string }

func newOut(t testing.TB) out {
	t.Helper()
	p := t.TempDir()
	return out{parent: p, dst: filepath.Join(p, "out")}
}

// createEncrypted7z writes arch/enc.7z from the files in src, with encrypted
// headers, under the password (read from stdin like the unpacking does).
func createEncrypted7z(t testing.TB, arch, src, password string) {
	t.Helper()
	run7zzIn(t, src, password+"\n"+password+"\n", "a", "-p", "-mhe=on", "-bd", "-mx1", filepath.Join(arch, "enc.7z"), ".")
}

// errorsIsAny reports whether err matches one of the targets.
func errorsIsAny(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}
