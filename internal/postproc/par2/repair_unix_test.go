//go:build unix

package par2_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func TestRepair_SymlinkInPlaceOfAMissingFileIsNeverFollowed(t *testing.T) {
	files := testFiles(3000)
	env := newRepairEnv(t, 1024, files, exps(0, 4)...)
	outside := t.TempDir()
	victim := writeFile(t, outside, "victim.bin", []byte("do not touch"))
	require.NoError(t, os.Remove(env.path("fileA.bin")))
	require.NoError(t, os.Symlink(victim, env.path("fileA.bin")))

	_, err := env.repair()

	require.Error(t, err)
	require.Equal(t, []byte("do not touch"), mustRead(t, victim))
}

func TestRepair_FileSwappedForASymlinkAfterVerifyIsNotFollowed(t *testing.T) {
	files := testFiles(3000, 2000)
	env := newRepairEnv(t, 1024, files, exps(0, 4)...)
	env.corruptSlice("fileA.bin", 1)
	vr := env.verify()
	outside := t.TempDir()
	victim := writeFile(t, outside, "victim.bin", []byte("do not touch"))
	require.NoError(t, os.Remove(env.path("fileB.bin"))) // a good file read during repair
	require.NoError(t, os.Symlink(victim, env.path("fileB.bin")))

	_, err := par2.Repair(context.Background(), env.Dir, env.Set, vr)

	require.Error(t, err)
	require.Equal(t, []byte("do not touch"), mustRead(t, victim))
}
