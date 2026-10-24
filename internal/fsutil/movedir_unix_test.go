//go:build unix

package fsutil_test

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/fsutil"
)

func TestMoveDir_CrossDeviceRefusesSpecialFiles(t *testing.T) {
	defer fsutil.ForceCrossDevice()()
	base := t.TempDir()
	src := makeTree(t, base)
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "zz-fifo"), 0o600))
	dst := filepath.Join(base, "dst")

	_, err := fsutil.MoveDir(src, dst)

	assert.ErrorIs(t, err, fsutil.ErrSpecialFile)
	assertMissing(t, dst)
	assert.FileExists(t, filepath.Join(src, "movie.mkv"))
}
