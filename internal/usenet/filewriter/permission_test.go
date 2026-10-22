package filewriter_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/usenet/filewriter"
)

func TestAnUnwritableParentFailsNamingTheFolderAndTheUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	parent := filepath.Join(t.TempDir(), "incomplete")
	require.NoError(t, os.Mkdir(parent, 0o555))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	_, err := filewriter.New(filepath.Join(parent, "dl"))

	require.ErrorIs(t, err, fs.ErrPermission)
	require.Contains(t, err.Error(), parent)
	require.Contains(t, err.Error(), fmt.Sprintf("uid %d", os.Geteuid()))
}

func TestAnUnwritableDownloadFolderFailsTheFirstWriteNamingTheFolderAndTheUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	w, dir := newWriter(t)
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := w.WritePart(0, splitParts("movie.mkv", pattern(10), 10)[0])

	require.ErrorIs(t, err, fs.ErrPermission)
	require.Contains(t, err.Error(), dir)
	require.Contains(t, err.Error(), fmt.Sprintf("uid %d", os.Geteuid()))
}
