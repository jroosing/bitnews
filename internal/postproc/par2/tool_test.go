package par2_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

var two = map[string]int{"a.bin": 10000, "b.bin": 4500}

func TestVerifyCleanSet(t *testing.T) {
	s := makeSet(t, 6, two)

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, &par2.Report{HasSet: true, Clean: true, Repairable: true, TotalBlocks: 15, RecoveryBlocks: 6}, rep)
}

func TestVerifyWithoutPAR2(t *testing.T) {
	s := makeSet(t, 6, two)
	for _, p := range []string{"set.par2", "set.vol0+3.par2", "set.vol3+3.par2"} {
		_ = os.Remove(s.path(p))
	}
	require.ElementsMatch(t, []string{"a.bin", "b.bin"}, s.listing(t))

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, &par2.Report{}, rep)
}

func TestVerifyGarbagePAR2HasNoSet(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(dir+"/x.par2", []byte("not a par2 file at all"), 0o600))
	require.NoError(t, os.WriteFile(dir+"/empty.par2", nil, 0o600))

	rep, err := toolFor(t).Verify(context.Background(), dir)

	require.NoError(t, err)
	assert.False(t, rep.HasSet)
}

func TestVerifyMissingDirIsAnError(t *testing.T) {
	_, err := toolFor(t).Verify(context.Background(), t.TempDir()+"/nope")

	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestVerifyCountsDamagedBlocks(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10, 5500) // blocks 0 and 5
	s.truncate(t, "b.bin", 2500)    // blocks 3,4 of 5 lost (partial block 2 kept? 2500 = 2.5 blocks)

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.HasSet)
	assert.False(t, rep.Clean)
	assert.True(t, rep.NeedsRepair)
	assert.Equal(t, 15, rep.TotalBlocks)
	assert.Equal(t, 6, rep.RecoveryBlocks)
	assert.Equal(t, 5, rep.DamagedBlocks, "a: blocks 0 and 5; b: blocks 2, 3 and 4")
	assert.True(t, rep.Repairable)
}

func TestVerifyMissingFileCountsAllItsBlocks(t *testing.T) {
	s := makeSet(t, 6, two)
	s.remove(t, "a.bin")

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, 10, rep.DamagedBlocks)
	assert.Equal(t, 6, rep.RecoveryBlocks)
	assert.False(t, rep.Repairable)
}

func TestVerifyWithoutRecoveryVolumes(t *testing.T) {
	s := makeSet(t, 6, two)
	s.removeVolumes(t)
	s.corrupt(t, "b.bin", 0)

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.NeedsRepair)
	assert.Equal(t, 1, rep.DamagedBlocks)
	assert.Zero(t, rep.RecoveryBlocks)
	assert.False(t, rep.Repairable)
}

func TestVerifyCountsAPartialRecoveryVolume(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 0, 1500, 2500)
	fi, err := os.Stat(s.path("set.vol3+3.par2"))
	require.NoError(t, err)
	s.truncate(t, "set.vol3+3.par2", fi.Size()/2) // loses its last blocks
	s.remove(t, "set.vol0+3.par2")

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, 3, rep.DamagedBlocks)
	assert.Positive(t, rep.RecoveryBlocks)
	assert.Less(t, rep.RecoveryBlocks, 3)
	assert.False(t, rep.Repairable)
}

func TestVerifyWorksWithoutTheMainPAR2(t *testing.T) {
	s := makeSet(t, 6, two)
	s.remove(t, "set.par2")
	s.corrupt(t, "a.bin", 0)

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.HasSet)
	assert.Equal(t, 1, rep.DamagedBlocks)
	assert.Equal(t, 6, rep.RecoveryBlocks)
	assert.True(t, rep.Repairable)
}

func TestVerifyWorksWithADamagedMainPAR2(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "set.par2", payload(7, 600))
	s.corrupt(t, "b.bin", 0)

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, 1, rep.DamagedBlocks)
	assert.True(t, rep.Repairable)
}

func TestVerifyFindsRecoveryVolumesWithOtherNames(t *testing.T) {
	s := makeSet(t, 6, two)
	s.rename(t, "set.vol0+3.par2", "zz9.vol0+3.par2")
	s.rename(t, "set.vol3+3.par2", "qq1.par2")
	s.corrupt(t, "a.bin", 0)

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, 6, rep.RecoveryBlocks)
}

func TestVerifyRenamesObfuscatedFiles(t *testing.T) {
	s := makeSet(t, 6, two)
	s.rename(t, "a.bin", "x7f3a9")
	s.rename(t, "b.bin", "0c1d2e.dat")

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.Clean)
	assert.Equal(t, 2, rep.Renamed)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "set.par2", "set.vol0+3.par2", "set.vol3+3.par2"}, s.listing(t))
	s.requireOriginals(t)

	rep, err = toolFor(t).Verify(context.Background(), s.dir)
	require.NoError(t, err)
	assert.True(t, rep.Clean, "a second run finds it already done")
	assert.Zero(t, rep.Renamed)
}

func TestVerifyRenamesAnObfuscatedFileOverADamagedOneAndDropsTheBackup(t *testing.T) {
	s := makeSet(t, 6, two)
	s.rename(t, "a.bin", "x7f3a9")
	s.write(t, "a.bin", payload(99, 10000)) // a wrong file under the right name

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.Clean)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "set.par2", "set.vol0+3.par2", "set.vol3+3.par2"}, s.listing(t), "no a.bin.1 left")
	s.requireOriginals(t)
}

func TestVerifyReportsDamageOfAnObfuscatedAndDamagedFile(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10)
	s.rename(t, "a.bin", "x7f3a9")

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.NeedsRepair)
	assert.Equal(t, 1, rep.DamagedBlocks, "the obfuscated copy is used as a data source")
	assert.True(t, rep.Repairable)
}

func TestVerifyIgnoresUnrelatedFilesAndSubdirectories(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "release.nfo", []byte("hello"))
	s.write(t, "empty.bin", nil)
	s.write(t, "other/inner.bin", payload(5, 3000))
	s.write(t, "-weird name .txt", []byte("x"))

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.Clean)
	assert.Equal(t, []byte("hello"), s.read(t, "release.nfo"))
	assert.Equal(t, payload(5, 3000), s.read(t, "other/inner.bin"))
}

func TestVerifyHandlesTargetsInSubdirectories(t *testing.T) {
	s := makeSet(t, 6, map[string]int{"a.bin": 5000, "sub/c.bin": 3000})
	s.corrupt(t, "sub/c.bin", 0)

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, 1, rep.DamagedBlocks)
	assert.True(t, rep.Repairable)
}

func TestVerifyTrailingGarbageNeedsARepairWithoutDamagedBlocks(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "b.bin", append(s.read(t, "b.bin"), "trailing junk"...))

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.NeedsRepair)
	assert.Zero(t, rep.DamagedBlocks)
	assert.True(t, rep.Repairable)
	_, err = toolFor(t).Repair(context.Background(), s.dir)
	require.NoError(t, err)
	s.requireOriginals(t)
}

func TestVerifyCancelledContext(t *testing.T) {
	s := makeSet(t, 6, two)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := toolFor(t).Verify(ctx, s.dir)

	assert.ErrorIs(t, err, context.Canceled)
}

func TestVerifyDropsBackupsOfAnInterruptedRepairWhenClean(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "a.bin.1", payload(3, 777))
	s.write(t, "a.bin.12", payload(4, 777))
	s.write(t, "unrelated.1", []byte("keep"))

	rep, err := toolFor(t).Verify(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, rep.Clean)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "unrelated.1", "set.par2", "set.vol0+3.par2", "set.vol3+3.par2"}, s.listing(t))
}

func TestRepairRestoresDamagedMissingAndTruncatedFiles(t *testing.T) {
	s := makeSet(t, 8, map[string]int{"a.bin": 10000, "b.bin": 4500, "c.bin": 3000})
	s.corrupt(t, "a.bin", 10, 7777) // blocks 0 and 7
	s.truncate(t, "b.bin", 2000)    // blocks 2, 3, 4
	s.remove(t, "c.bin")            // 3 blocks
	tl := toolFor(t)
	rep, err := tl.Verify(context.Background(), s.dir)
	require.NoError(t, err)
	require.True(t, rep.Repairable)
	require.Equal(t, 8, rep.DamagedBlocks)

	res, err := tl.Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, 8, res.RepairedBlocks)
	s.requireOriginals(t)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "c.bin", "set.par2", "set.vol0+4.par2", "set.vol4+4.par2"}, s.listing(t), "the name.1 backups are gone")
	rep, err = tl.Verify(context.Background(), s.dir)
	require.NoError(t, err)
	assert.True(t, rep.Clean)
}

func TestRepairIsRepeatable(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10)
	tl := toolFor(t)

	_, err := tl.Repair(context.Background(), s.dir)
	require.NoError(t, err)
	res, err := tl.Repair(context.Background(), s.dir)

	require.NoError(t, err, "nothing left to repair is success")
	assert.Zero(t, res.RepairedBlocks)
	s.requireOriginals(t)
}

func TestRepairInsufficientRecoveryChangesNothing(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 0, 1500, 2500, 3500, 4500, 5500, 6500, 7500) // 8 blocks, 6 recovery
	before := snapshot(t, s)

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	var short *par2.InsufficientError
	require.ErrorAs(t, err, &short)
	assert.Equal(t, 8, short.Needed)
	assert.Equal(t, 6, short.Available)
	assert.Equal(t, before, snapshot(t, s))
}

func TestRepairWithoutRecoveryVolumes(t *testing.T) {
	s := makeSet(t, 6, two)
	s.removeVolumes(t)
	s.corrupt(t, "a.bin", 0)

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	var short *par2.InsufficientError
	require.ErrorAs(t, err, &short)
	assert.Equal(t, 1, short.Needed)
	assert.Zero(t, short.Available)
}

func TestRepairWithoutASetIsErrNoSet(t *testing.T) {
	_, err := toolFor(t).Repair(context.Background(), t.TempDir())

	assert.ErrorIs(t, err, par2.ErrNoSet)
}

func TestRepairCancelledContextChangesNothing(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10)
	before := snapshot(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := toolFor(t).Repair(ctx, s.dir)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, before, snapshot(t, s))
}

func TestRepairPicksUpTheStateOfAnInterruptedRepair(t *testing.T) {
	// A repair killed after par2 moved the damaged a.bin to a.bin.1 and
	// before it wrote the new a.bin leaves exactly this.
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10, 4500)
	s.rename(t, "a.bin", "a.bin.1")
	tl := toolFor(t)

	rep, err := tl.Verify(context.Background(), s.dir)
	require.NoError(t, err)
	require.True(t, rep.NeedsRepair)
	require.True(t, rep.Repairable)
	_, err = tl.Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "set.par2", "set.vol0+3.par2", "set.vol3+3.par2"}, s.listing(t))
}

func TestRepairRestoresInSubdirectories(t *testing.T) {
	s := makeSet(t, 6, map[string]int{"a.bin": 5000, "sub/c.bin": 3000})
	s.corrupt(t, "sub/c.bin", 0)
	s.remove(t, "sub/c.bin")
	require.NoError(t, os.Remove(s.path("sub")))

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
}

// snapshot maps each file below the set's directory to its content.
func snapshot(t testing.TB, s *set) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range s.listing(t) {
		out[n] = string(s.read(t, n))
	}
	return out
}

func TestRepairRepairsAnObfuscatedAndDamagedFile(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10)
	s.rename(t, "a.bin", "x7f3a9")
	tl := toolFor(t)

	_, err := tl.Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "set.par2", "set.vol0+3.par2", "set.vol3+3.par2"}, s.listing(t), "the damaged copy is gone")
}

func TestRepairKeepsUnrelatedFiles(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "release.nfo", []byte("hello"))
	s.write(t, "sample.mkv", payload(8, 2000))
	s.write(t, "dup.dat", s.files["b.bin"])
	s.corrupt(t, "a.bin", 10)

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "dup.dat", "sample.mkv", "release.nfo", "set.par2", "set.vol0+3.par2", "set.vol3+3.par2"}, s.listing(t))
}
