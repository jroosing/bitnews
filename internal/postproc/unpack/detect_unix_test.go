//go:build unix

package unpack

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectIgnoresNamedPipes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe.rar"), 0o600))

	sets, err := Detect(dir) // must not block opening the pipe

	require.NoError(t, err)
	require.Empty(t, sets)
}
