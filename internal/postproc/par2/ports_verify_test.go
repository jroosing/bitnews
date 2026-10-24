package par2

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

var twoFiles = map[string]int{"alpha.bin": 6500, "beta.dat": 3000} // 7 + 3 blocks

func TestVerifier_IntactSetIsClean(t *testing.T) {
	s := makePortSet(t, 4, twoFiles)

	out, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{DownloadID: "d"})

	require.NoError(t, err)
	assert.True(t, out.HasPAR2)
	assert.True(t, out.Clean)
	assert.False(t, out.NeedsRepair)
	assert.Zero(t, out.DamagedSlices)
	assert.Equal(t, 4, out.RecoveryOnDisk)
	assert.True(t, out.Repairable)
}

func TestVerifier_DamagedCopyNeedsRepair(t *testing.T) {
	s := makePortSet(t, 4, twoFiles)
	s.flipByte(t, "alpha.bin", 0)
	s.flipByte(t, "alpha.bin", 1500) // blocks 0 and 1

	out, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})

	require.NoError(t, err)
	assert.True(t, out.HasPAR2)
	assert.False(t, out.Clean)
	assert.True(t, out.NeedsRepair)
	assert.Equal(t, 2, out.DamagedSlices)
	assert.Equal(t, 4, out.RecoveryOnDisk)
	assert.True(t, out.Repairable)
}

func TestVerifier_MissingFileCountsAllItsSlices(t *testing.T) {
	s := makePortSet(t, 4, twoFiles)
	require.NoError(t, os.Remove(s.path("alpha.bin")))

	out, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})

	require.NoError(t, err)
	assert.True(t, out.NeedsRepair)
	assert.Equal(t, 7, out.DamagedSlices)
	assert.False(t, out.Repairable, "7 damaged slices, 4 recovery blocks")
}

func TestVerifier_NoPAR2(t *testing.T) {
	s := makePortSet(t, 4, twoFiles)
	s.removePAR2(t)

	out, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})

	require.NoError(t, err)
	assert.Equal(t, core.VerifyOutcome{}, out)
}

func TestVerifier_EmptyDirHasNoPAR2(t *testing.T) {
	out, err := portVerifier(t).Verify(context.Background(), t.TempDir(), core.VerifyOptions{})

	require.NoError(t, err)
	assert.False(t, out.HasPAR2)
}

func TestVerifier_GarbagePAR2HasNoPAR2(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.par2"), []byte("not a par2 file"), 0o644))

	out, err := portVerifier(t).Verify(context.Background(), dir, core.VerifyOptions{})

	require.NoError(t, err)
	assert.False(t, out.HasPAR2)
}

func TestVerifier_MissingDirectoryIsAnError(t *testing.T) {
	_, err := portVerifier(t).Verify(context.Background(), filepath.Join(t.TempDir(), "nope"), core.VerifyOptions{})

	require.Error(t, err)
	assert.NotErrorIs(t, err, core.ErrInsufficientRecovery)
}

func TestVerifier_DeferredBlocksFlipRepairable(t *testing.T) {
	s := makePortSet(t, 2, map[string]int{"single.bin": 3000})
	for _, off := range []int{0, 1000, 2000} {
		s.flipByte(t, "single.bin", off)
	}

	out, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{DownloadID: "d"})
	require.NoError(t, err)
	assert.Equal(t, 3, out.DamagedSlices)
	assert.Equal(t, 2, out.RecoveryOnDisk)
	assert.False(t, out.Repairable)

	var asked core.ID
	v := portVerifier(t)
	v.DeferredBlocks = func(id core.ID) int { asked = id; return 1 }
	out, err = v.Verify(context.Background(), s.dir, core.VerifyOptions{DownloadID: "d"})
	require.NoError(t, err)
	assert.Equal(t, core.ID("d"), asked)
	assert.Equal(t, 2, out.RecoveryOnDisk, "deferred blocks are not on disk")
	assert.True(t, out.Repairable)
}

func TestVerifier_CancelledContext(t *testing.T) {
	s := makePortSet(t, 2, map[string]int{"single.bin": 3000})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := portVerifier(t).Verify(ctx, s.dir, core.VerifyOptions{})

	assert.ErrorIs(t, err, context.Canceled)
}

func TestVerifier_RenamesObfuscatedFiles(t *testing.T) {
	s := makePortSet(t, 2, map[string]int{"single.bin": 3000})
	require.NoError(t, os.Rename(s.path("single.bin"), s.path("a1b2c3")))

	out, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})
	require.NoError(t, err)
	assert.True(t, out.Clean)
	s.requireOriginals(t)

	// A second run finds it already done.
	out, err = portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})
	require.NoError(t, err)
	assert.True(t, out.Clean)
}

func TestVerifier_UnsafeNameIsAPathViolation(t *testing.T) {
	s := makePortSet(t, 2, map[string]int{"single.bin": 3000})
	portPatchName(t, s.path("set.par2"), "single.bin", "../escaped")

	_, err := portVerifier(t).Verify(context.Background(), s.dir, core.VerifyOptions{})

	assert.ErrorIs(t, err, core.ErrPathViolation)
}

func TestVerifier_MissingPar2ProgramIsAnError(t *testing.T) {
	s := makePortSet(t, 2, map[string]int{"single.bin": 3000})
	t.Setenv("PATH", t.TempDir())

	_, err := Verifier{}.Verify(context.Background(), s.dir, core.VerifyOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "par2")
}
