//go:build !windows

package privdrop_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/bootstrap"
	"gitlab.com/kyura4k/gitnews/internal/privdrop"
)

// These tests use the real system calls but never change ids, so they are
// safe to run as any user.

func TestOSUmaskAppliesToFilesCreatedAfterwards(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	require.NoError(t, privdrop.Apply(privdrop.Options{Umask: 0o077}, privdrop.OS(), discard()))

	path := filepath.Join(t.TempDir(), "f")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestOSRunAsEqualToCurrentIdentityIsNoopWhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root would really drop privileges")
	}
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	runAs := &bootstrap.Identity{UID: os.Geteuid(), GID: os.Getegid()}

	err := privdrop.Apply(privdrop.Options{RunAs: runAs, Umask: 0o022}, privdrop.OS(), discard())

	assert.NoError(t, err)
}

func TestOSRunAsDifferentFromCurrentIdentityIsAnErrorWhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root would really drop privileges")
	}
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	runAs := &bootstrap.Identity{UID: os.Geteuid() + 1, GID: os.Getegid()}

	err := privdrop.Apply(privdrop.Options{RunAs: runAs, Umask: 0o022}, privdrop.OS(), discard())

	assert.Error(t, err)
}
