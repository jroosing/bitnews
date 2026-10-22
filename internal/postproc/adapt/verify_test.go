package adapt

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestVerifier_IntactSetIsClean(t *testing.T) {
	dir, m := goldenDir(t, "multi_files")
	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{DownloadID: "d"})
	require.NoError(t, err)
	assert.True(t, out.HasPAR2)
	assert.True(t, out.Clean)
	assert.False(t, out.NeedsRepair)
	assert.Zero(t, out.DamagedSlices)
	assert.Equal(t, m.RecoveryBlocks, out.RecoveryOnDisk)
	assert.True(t, out.Repairable)
	assert.NotNil(t, out.Opaque)
}

func TestVerifier_DamagedCopyNeedsRepair(t *testing.T) {
	dir, m := goldenDir(t, "multi_files")
	// 4096-byte slices: offsets 0 and 5000 hit slices 0 and 1 of alpha.bin.
	flipByte(t, filepath.Join(dir, "alpha.bin"), 0)
	flipByte(t, filepath.Join(dir, "alpha.bin"), 5000)

	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
	require.NoError(t, err)
	assert.True(t, out.HasPAR2)
	assert.False(t, out.Clean)
	assert.True(t, out.NeedsRepair)
	assert.Equal(t, 2, out.DamagedSlices)
	assert.Equal(t, m.RecoveryBlocks, out.RecoveryOnDisk)
	assert.True(t, out.Repairable)
}

func TestVerifier_MissingFileCountsAllItsSlices(t *testing.T) {
	dir, _ := goldenDir(t, "multi_files")
	require.NoError(t, os.Remove(filepath.Join(dir, "beta.dat")))

	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
	require.NoError(t, err)
	assert.True(t, out.NeedsRepair)
	assert.Equal(t, 9, out.DamagedSlices)
	assert.False(t, out.Repairable, "9 damaged slices, 4 recovery blocks")
}

func TestVerifier_NoPAR2(t *testing.T) {
	dir, _ := goldenDir(t, "multi_files")
	removePAR2(t, dir)

	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
	require.NoError(t, err)
	assert.Equal(t, core.VerifyOutcome{}, out)
}

func TestVerifier_EmptyDirHasNoPAR2(t *testing.T) {
	out, err := Verifier{}.Verify(context.Background(), t.TempDir(), core.VerifyOptions{})
	require.NoError(t, err)
	assert.False(t, out.HasPAR2)
}

func TestVerifier_GarbagePAR2HasNoPAR2(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.par2"), []byte("not a par2 file"), 0o644))
	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
	require.NoError(t, err)
	assert.False(t, out.HasPAR2)
}

func TestVerifier_MissingDirectoryIsAnError(t *testing.T) {
	_, err := Verifier{}.Verify(context.Background(), filepath.Join(t.TempDir(), "nope"), core.VerifyOptions{})
	require.Error(t, err)
	assert.NotErrorIs(t, err, core.ErrInsufficientRecovery)
}

func TestVerifier_DeferredBlocksFlipRepairable(t *testing.T) {
	// single_small has 3 slices and 2 recovery blocks.
	dir, _ := goldenDir(t, "single_small")
	for _, off := range []int64{0, 4096, 8192} {
		flipByte(t, filepath.Join(dir, "single.bin"), off)
	}

	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{DownloadID: "d"})
	require.NoError(t, err)
	assert.Equal(t, 3, out.DamagedSlices)
	assert.Equal(t, 2, out.RecoveryOnDisk)
	assert.False(t, out.Repairable)

	var asked core.ID
	v := Verifier{DeferredBlocks: func(id core.ID) int { asked = id; return 1 }}
	out, err = v.Verify(context.Background(), dir, core.VerifyOptions{DownloadID: "d"})
	require.NoError(t, err)
	assert.Equal(t, core.ID("d"), asked)
	assert.Equal(t, 2, out.RecoveryOnDisk, "deferred blocks are not on disk")
	assert.True(t, out.Repairable)
}

func TestVerifier_CancelledContext(t *testing.T) {
	dir, _ := goldenDir(t, "single_small")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Verifier{}.Verify(ctx, dir, core.VerifyOptions{})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestVerifier_RenamesObfuscatedFiles(t *testing.T) {
	dir, m := goldenDir(t, "single_small")
	require.NoError(t, os.Rename(filepath.Join(dir, "single.bin"), filepath.Join(dir, "a1b2c3")))

	out, err := Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
	require.NoError(t, err)
	assert.True(t, out.Clean)
	requireManifestFiles(t, dir, m)

	// A second run finds it already done.
	out, err = Verifier{}.Verify(context.Background(), dir, core.VerifyOptions{})
	require.NoError(t, err)
	assert.True(t, out.Clean)
}
