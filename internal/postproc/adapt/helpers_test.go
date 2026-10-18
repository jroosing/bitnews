package adapt

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// goldenRoot holds the committed PAR2 fixtures of the par2 package. Tests copy
// from it and never write to it.
const goldenRoot = "../par2/testdata/golden"

type goldenFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	MD5  string `json:"md5"`
}

type goldenManifest struct {
	SliceSize      int          `json:"slice_size"`
	RecoveryBlocks int          `json:"recovery_blocks"`
	TotalSlices    int          `json:"total_slices"`
	Files          []goldenFile `json:"files"`
}

// copyFlat copies the regular files of every src directory into dst.
func copyFlat(t testing.TB, dst string, srcs ...string) {
	t.Helper()
	for _, src := range srcs {
		ents, err := os.ReadDir(src)
		require.NoError(t, err)
		for _, e := range ents {
			if !e.Type().IsRegular() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(src, e.Name()))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644))
		}
	}
}

// goldenDir copies the data and PAR2 files of a golden case into a new
// temporary directory and returns it with the case's manifest.
func goldenDir(t testing.TB, name string) (string, goldenManifest) {
	t.Helper()
	root := filepath.Join(goldenRoot, name)
	dir := t.TempDir()
	copyFlat(t, dir, filepath.Join(root, "data"), filepath.Join(root, "par2"))
	var m goldenManifest
	b, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &m))
	return dir, m
}

// removePAR2 deletes every .par2 file in dir.
func removePAR2(t testing.TB, dir string) {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.par2"))
	require.NoError(t, err)
	for _, p := range m {
		require.NoError(t, os.Remove(p))
	}
}

// flipByte flips every bit of the byte at offset in the file.
func flipByte(t testing.TB, path string, offset int64) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	b[offset] ^= 0xff
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

func md5Of(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// requireManifestFiles checks every file of the manifest in dir against its
// MD5.
func requireManifestFiles(t testing.TB, dir string, m goldenManifest) {
	t.Helper()
	for _, f := range m.Files {
		require.Equal(t, f.MD5, md5Of(t, filepath.Join(dir, f.Name)), f.Name)
	}
}

// digests maps every file name in dir to its MD5.
func digests(t testing.TB, dir string) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string]string{}
	for _, e := range ents {
		out[e.Name()] = md5Of(t, filepath.Join(dir, e.Name()))
	}
	return out
}
