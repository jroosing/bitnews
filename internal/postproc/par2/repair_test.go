package par2_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

var two = map[string]int{"a.bin": 10000, "b.bin": 4500}

var setListing = []string{"a.bin", "b.bin", "set.par2", "set.vol0+3.par2", "set.vol3+3.par2"}

func TestRepairOfAnIntactSetChangesNothing(t *testing.T) {
	s := makeSet(t, 6, two)
	before := snapshot(t, s)

	out, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, core.RepairOutcome{HasPAR2: true}, out)
	assert.Equal(t, before, snapshot(t, s))
}

func TestRepairWithoutPAR2HasNoSet(t *testing.T) {
	s := makeSet(t, 6, two)
	for _, p := range []string{"set.par2", "set.vol0+3.par2", "set.vol3+3.par2"} {
		s.remove(t, p)
	}

	out, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.False(t, out.HasPAR2)
}

func TestRepairOfAnEmptyDirHasNoSet(t *testing.T) {
	out, err := toolFor(t).Repair(context.Background(), t.TempDir())

	require.NoError(t, err)
	assert.False(t, out.HasPAR2)
}

func TestRepairWithAGarbagePAR2HasNoSet(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(dir+"/x.par2", []byte("not a par2 file at all"), 0o600))
	require.NoError(t, os.WriteFile(dir+"/empty.par2", nil, 0o600))

	out, err := toolFor(t).Repair(context.Background(), dir)

	require.NoError(t, err)
	assert.False(t, out.HasPAR2)
}

func TestRepairOfAMissingDirIsAnError(t *testing.T) {
	_, err := toolFor(t).Repair(context.Background(), t.TempDir()+"/nope")

	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRepairRestoresDamagedMissingAndTruncatedFiles(t *testing.T) {
	s := makeSet(t, 8, map[string]int{"a.bin": 10000, "b.bin": 4500, "c.bin": 3000})
	s.corrupt(t, "a.bin", 10, 7777) // blocks 0 and 7
	s.truncate(t, "b.bin", 2000)    // blocks 2, 3, 4
	s.remove(t, "c.bin")            // 3 blocks

	out, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, core.RepairOutcome{HasPAR2: true, RepairedSlices: 8}, out)
	s.requireOriginals(t)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "c.bin", "set.par2", "set.vol0+4.par2", "set.vol4+4.par2"}, s.listing(t), "the name.1 backups are gone")
}

func TestRepairIsRepeatable(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10)
	tl := toolFor(t)

	_, err := tl.Repair(context.Background(), s.dir)
	require.NoError(t, err)
	out, err := tl.Repair(context.Background(), s.dir)

	require.NoError(t, err, "nothing left to repair is success")
	assert.Zero(t, out.RepairedSlices)
	s.requireOriginals(t)
}

func TestRepairWorksWithoutTheMainPAR2(t *testing.T) {
	s := makeSet(t, 6, two)
	s.remove(t, "set.par2")
	s.corrupt(t, "a.bin", 0)

	out, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, out.HasPAR2)
	s.requireOriginals(t)
}

func TestRepairWorksWithADamagedMainPAR2(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "set.par2", payload(7, 600))
	s.corrupt(t, "b.bin", 0)

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
}

func TestRepairFindsRecoveryVolumesWithOtherNames(t *testing.T) {
	s := makeSet(t, 6, two)
	s.rename(t, "set.vol0+3.par2", "zz9.vol0+3.par2")
	s.rename(t, "set.vol3+3.par2", "qq1.par2")
	s.corrupt(t, "a.bin", 0, 1500, 2500, 3500, 4500) // needs 5 of the 6 blocks

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
}

func TestRepairRenamesObfuscatedFiles(t *testing.T) {
	s := makeSet(t, 6, two)
	s.rename(t, "a.bin", "x7f3a9")
	s.rename(t, "b.bin", "0c1d2e.dat")

	out, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.True(t, out.HasPAR2)
	assert.ElementsMatch(t, setListing, s.listing(t))
	s.requireOriginals(t)

	_, err = toolFor(t).Repair(context.Background(), s.dir)
	require.NoError(t, err, "a second run finds it already done")
	assert.ElementsMatch(t, setListing, s.listing(t))
}

func TestRepairRenamesAnObfuscatedFileOverAWrongOneAndDropsTheBackup(t *testing.T) {
	s := makeSet(t, 6, two)
	s.rename(t, "a.bin", "x7f3a9")
	s.write(t, "a.bin", payload(99, 10000)) // a wrong file under the right name

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.ElementsMatch(t, setListing, s.listing(t), "no a.bin.1 left")
	s.requireOriginals(t)
}

func TestRepairRepairsAnObfuscatedAndDamagedFile(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10)
	s.rename(t, "a.bin", "x7f3a9")

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
	assert.ElementsMatch(t, setListing, s.listing(t), "the damaged copy is gone")
}

func TestRepairInsufficientRecoveryIsAShortfallAndChangesNothing(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 0, 1500, 2500, 3500, 4500, 5500, 6500, 7500) // 8 blocks, 6 recovery
	before := snapshot(t, s)

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	var short *core.RecoveryShortfall
	require.ErrorAs(t, err, &short)
	assert.ErrorIs(t, err, core.ErrInsufficientRecovery)
	assert.Equal(t, 8, short.Needed)
	assert.Equal(t, 6, short.Available)
	assert.Equal(t, before, snapshot(t, s))
}

func TestRepairOfAMissingFileNeedsAllItsBlocks(t *testing.T) {
	s := makeSet(t, 6, two)
	s.remove(t, "a.bin")

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	var short *core.RecoveryShortfall
	require.ErrorAs(t, err, &short)
	assert.Equal(t, 10, short.Needed)
	assert.Equal(t, 6, short.Available)
}

func TestRepairWithoutRecoveryVolumesIsAShortfall(t *testing.T) {
	s := makeSet(t, 6, two)
	s.removeVolumes(t)
	s.corrupt(t, "a.bin", 0)

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	var short *core.RecoveryShortfall
	require.ErrorAs(t, err, &short)
	assert.Equal(t, 1, short.Needed)
	assert.Zero(t, short.Available)
}

func TestRepairCountsAPartialRecoveryVolume(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 0, 1500, 2500)
	fi, err := os.Stat(s.path("set.vol3+3.par2"))
	require.NoError(t, err)
	s.truncate(t, "set.vol3+3.par2", fi.Size()/2) // loses its last blocks
	s.remove(t, "set.vol0+3.par2")

	_, err = toolFor(t).Repair(context.Background(), s.dir)

	var short *core.RecoveryShortfall
	require.ErrorAs(t, err, &short)
	assert.Equal(t, 3, short.Needed)
	assert.Positive(t, short.Available)
	assert.Less(t, short.Available, 3)
}

func TestRepairCancelledContextChangesNothing(t *testing.T) {
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10)
	before := snapshot(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := toolFor(t).Repair(ctx, s.dir)

	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, core.ErrRepairFailed)
	assert.Equal(t, before, snapshot(t, s))
}

func TestRepairPicksUpTheStateOfAnInterruptedRepair(t *testing.T) {
	// A repair killed after par2 moved the damaged a.bin to a.bin.1 and
	// before it wrote the new a.bin leaves exactly this.
	s := makeSet(t, 6, two)
	s.corrupt(t, "a.bin", 10, 4500)
	s.rename(t, "a.bin", "a.bin.1")

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
	assert.ElementsMatch(t, setListing, s.listing(t))
}

func TestRepairDropsBackupsOfAnInterruptedRepairWhenClean(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "a.bin.1", payload(3, 777))
	s.write(t, "a.bin.12", payload(4, 777))
	s.write(t, "unrelated.1", []byte("keep"))

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.ElementsMatch(t, append([]string{"unrelated.1"}, setListing...), s.listing(t))
}

func TestRepairRestoresInSubdirectories(t *testing.T) {
	s := makeSet(t, 6, map[string]int{"a.bin": 5000, "sub/c.bin": 3000})
	s.remove(t, "sub/c.bin")
	require.NoError(t, os.Remove(s.path("sub")))

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
}

func TestRepairFixesTrailingGarbage(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "b.bin", append(s.read(t, "b.bin"), "trailing junk"...))

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	s.requireOriginals(t)
}

func TestRepairKeepsUnrelatedFilesAndSubdirectories(t *testing.T) {
	s := makeSet(t, 6, two)
	s.write(t, "release.nfo", []byte("hello"))
	s.write(t, "sample.mkv", payload(8, 2000))
	s.write(t, "dup.dat", s.files["b.bin"])
	s.write(t, "other/inner.bin", payload(5, 3000))
	s.write(t, "-weird name .txt", []byte("x"))
	s.corrupt(t, "a.bin", 10)

	_, err := toolFor(t).Repair(context.Background(), s.dir)

	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), s.read(t, "release.nfo"))
	assert.Equal(t, payload(5, 3000), s.read(t, "other/inner.bin"))
	assert.ElementsMatch(t, append([]string{"dup.dat", "sample.mkv", "release.nfo", "other/inner.bin", "-weird name .txt"}, setListing...), s.listing(t))
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
