//go:build unix

package par2_test

import (
	"context"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func TestVerify_FIFOsAreIgnoredAndNeverOpened(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files)
	require.NoError(t, syscall.Mkfifo(env.path("pipe"), 0o600))
	// A FIFO under the expected name would block forever if it were opened.
	require.NoError(t, syscall.Mkfifo(env.path("other.bin"), 0o600))
	files2 := testFiles(3000, 4000)
	env2 := newVTEnvBare(t, 1024, files2[1:])
	require.NoError(t, syscall.Mkfifo(env2.path("fileB.bin"), 0o600))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)
	require.NoError(t, err)
	require.False(t, res.NeedsRepair())

	res, err = par2.Verify(context.Background(), env2.Dir, env2.Set)
	require.NoError(t, err)
	require.Equal(t, par2.StateMissing, res.Files[0].State)
}
