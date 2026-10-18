package unpack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeFile writes content to dir/name and returns the full path.
func writeFile(t testing.TB, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, content, 0o600))
	return p
}

// fakeVolume is a file that carries only a magic header and some padding. It
// is enough for Detect, which never parses the archive.
func fakeVolume(magic []byte) []byte {
	return append(append([]byte{}, magic...), make([]byte, 32)...)
}
