package fsutil_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/fsutil"
)

func TestFreeBytes_ReportsTheSpaceOfTheFilesystemHoldingThePath(t *testing.T) {
	n, err := fsutil.FreeBytes(t.TempDir())
	if errors.Is(err, fsutil.ErrUnsupported) {
		t.Skip("no free-space probe on this platform")
	}
	require.NoError(t, err)
	assert.NotZero(t, n)
}

func TestFreeBytes_MissingPathIsAnErrorNamingThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := fsutil.FreeBytes(missing)
	if errors.Is(err, fsutil.ErrUnsupported) {
		t.Skip("no free-space probe on this platform")
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
}

func TestDiskSpace_FreeNeverExceedsTotal(t *testing.T) {
	free, total, err := fsutil.DiskSpace(t.TempDir())
	if errors.Is(err, fsutil.ErrUnsupported) {
		t.Skip("no free-space probe on this platform")
	}
	require.NoError(t, err)
	assert.NotZero(t, total)
	assert.LessOrEqual(t, free, total)
}

func TestFreeBytes_AgreesWithDiskSpace(t *testing.T) {
	dir := t.TempDir()
	_, total, err := fsutil.DiskSpace(dir)
	if errors.Is(err, fsutil.ErrUnsupported) {
		t.Skip("no free-space probe on this platform")
	}
	require.NoError(t, err)
	n, err := fsutil.FreeBytes(dir)
	require.NoError(t, err)
	assert.LessOrEqual(t, n, total)
}
