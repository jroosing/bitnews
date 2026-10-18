package par2_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func TestVerify_IntactSetIsOKAndNeedsNoRepair(t *testing.T) {
	files := testFiles(1000, 4096, 5000)
	env := newVTEnv(t, 1024, files, 0, 1)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Len(t, res.Files, 3)
	for _, f := range files {
		got := vtFile(t, res, f.Name)
		require.Equal(t, par2.StateOK, got.State, f.Name)
		require.Empty(t, got.DamagedSlices)
		require.Nil(t, got.Renamed)
	}
	require.False(t, res.NeedsRepair())
	require.True(t, res.Repairable())
	require.Zero(t, res.RecoverySlicesNeeded())
	require.Equal(t, 2, res.RecoverySlicesOnDisk)
}

func TestVerify_FlippedByteDamagesExactlyItsSlice(t *testing.T) {
	files := testFiles(1000, 5000)
	env := newVTEnv(t, 1024, files, 0, 1)
	data := env.read("fileB.bin")
	data[2500] ^= 0xFF // slice 2 of 0..4
	env.write("fileB.bin", data)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	b := vtFile(t, res, "fileB.bin")
	require.Equal(t, par2.StateDamaged, b.State)
	require.Equal(t, []int{2}, b.DamagedSlices)
	require.EqualValues(t, 5, b.TotalSlices)
	require.EqualValues(t, 1, b.DamagedCount)
	require.Equal(t, par2.StateOK, vtFile(t, res, "fileA.bin").State)
	require.True(t, res.NeedsRepair())
	require.Equal(t, 1, res.RecoverySlicesNeeded())
	require.True(t, res.Repairable())
}

func TestVerify_FlippedByteInLastPartialSliceIsDetected(t *testing.T) {
	files := testFiles(5000)
	env := newVTEnv(t, 1024, files, 0)
	data := env.read("fileA.bin")
	data[4999] ^= 1
	env.write("fileA.bin", data)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, []int{4}, vtFile(t, res, "fileA.bin").DamagedSlices)
}

func TestVerify_TruncatedFileDamagesPartialAndTrailingSlices(t *testing.T) {
	files := testFiles(5000) // 5 slices of 1024, last one partial
	env := newVTEnv(t, 1024, files, 0, 1, 2)
	env.write("fileA.bin", env.read("fileA.bin")[:2500]) // slices 0,1 intact; 2 cut

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	a := vtFile(t, res, "fileA.bin")
	require.Equal(t, par2.StateDamaged, a.State)
	require.Equal(t, []int{2, 3, 4}, a.DamagedSlices)
	require.EqualValues(t, 2500, a.Size)
}

func TestVerify_TruncatedToZeroBytesDamagesEverySlice(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files, 0, 1, 2)
	env.write("fileA.bin", nil)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	a := vtFile(t, res, "fileA.bin")
	require.Equal(t, par2.StateDamaged, a.State)
	require.Equal(t, []int{0, 1, 2}, a.DamagedSlices)
}

func TestVerify_ExtendedFileKeepsItsGoodPrefix(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files, 0)
	data := env.read("fileA.bin")
	data[1500] ^= 0x55 // slice 1 damaged
	env.write("fileA.bin", append(data, make([]byte, 700)...))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	a := vtFile(t, res, "fileA.bin")
	require.Equal(t, par2.StateDamaged, a.State)
	require.Equal(t, []int{1}, a.DamagedSlices)
}

func TestVerify_FileWithOnlyTrailingGarbageIsDamagedWithNoDamagedSlices(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files)
	env.write("fileA.bin", append(env.read("fileA.bin"), "junk"...))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	a := vtFile(t, res, "fileA.bin")
	require.Equal(t, par2.StateDamaged, a.State)
	require.Empty(t, a.DamagedSlices)
	require.True(t, res.NeedsRepair())
	require.Zero(t, res.RecoverySlicesNeeded())
}

func TestVerify_MissingFileCountsEverySlice(t *testing.T) {
	files := testFiles(1000, 3000)
	env := newVTEnv(t, 1024, files, 0, 1)
	require.NoError(t, os.Remove(env.path("fileB.bin")))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	b := vtFile(t, res, "fileB.bin")
	require.Equal(t, par2.StateMissing, b.State)
	require.EqualValues(t, 3, b.TotalSlices)
	require.EqualValues(t, 3, b.DamagedCount)
	require.Empty(t, b.Path)
	require.EqualValues(t, 3, res.DamagedSlices)
	require.False(t, res.Repairable()) // 3 > 2 on disk
	require.True(t, res.NeedsRepair())
}

func TestVerify_ObfuscatedFileIsRenamedToItsRealName(t *testing.T) {
	files := testFiles(1000, 40000)
	env := newVTEnv(t, 1024, files, 0)
	require.NoError(t, os.Rename(env.path("fileB.bin"), env.path("a1b2c3d4e5")))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	b := vtFile(t, res, "fileB.bin")
	require.Equal(t, par2.StateOK, b.State)
	require.Equal(t, &par2.Rename{From: "a1b2c3d4e5", To: "fileB.bin"}, b.Renamed)
	require.Equal(t, env.path("fileB.bin"), b.Path)
	require.Equal(t, []string{"fileA.bin", "fileB.bin"}, env.names())
	require.Equal(t, files[1].Data, env.read("fileB.bin"))
}

func TestVerify_ObfuscatedAndDamagedFileIsRenamedAndSliceChecked(t *testing.T) {
	// Damage past the first 16 KiB keeps the 16k hash and the length.
	files := testFiles(40000)
	env := newVTEnv(t, 1024, files, 0)
	data := env.read("fileA.bin")
	data[30000] ^= 1
	require.NoError(t, os.Remove(env.path("fileA.bin")))
	env.write("obf.tmp", data)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	a := vtFile(t, res, "fileA.bin")
	require.Equal(t, par2.StateDamaged, a.State)
	require.Equal(t, []int{29}, a.DamagedSlices)
	require.Equal(t, "obf.tmp", a.Renamed.From)
	require.Equal(t, []string{"fileA.bin"}, env.names())
}

func TestVerify_FileDamagedInTheFirst16KiBIsNotMatchedByContent(t *testing.T) {
	files := testFiles(40000)
	env := newVTEnv(t, 1024, files)
	data := env.read("fileA.bin")
	data[100] ^= 1
	require.NoError(t, os.Remove(env.path("fileA.bin")))
	env.write("obf.tmp", data)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, par2.StateMissing, vtFile(t, res, "fileA.bin").State)
	require.Equal(t, []string{"obf.tmp"}, env.names())
}

func TestVerify_DryRunReportsRenamesAndWritesNothing(t *testing.T) {
	files := testFiles(1000, 40000)
	env := newVTEnv(t, 1024, files, 0)
	require.NoError(t, os.Rename(env.path("fileB.bin"), env.path("obf1")))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set, par2.WithDryRun())

	require.NoError(t, err)
	b := vtFile(t, res, "fileB.bin")
	require.Equal(t, par2.StateOK, b.State)
	require.Equal(t, &par2.Rename{From: "obf1", To: "fileB.bin"}, b.Renamed)
	require.Equal(t, []string{"fileA.bin", "obf1"}, env.names())
}

func TestVerify_SameFirst16KiBButDifferentLengthMatchesByLength(t *testing.T) {
	head := testFiles(20000)[0].Data[:16*1024]
	f1 := fileSpec{Name: "one.bin", Data: append(append([]byte(nil), head...), 1, 2, 3, 4)}
	f2 := fileSpec{Name: "two.bin", Data: append(append([]byte(nil), head...), 9, 8, 7, 6, 5, 4, 3, 2)}
	env := newVTEnv(t, 1024, []fileSpec{f1, f2})
	require.NoError(t, os.Rename(env.path("one.bin"), env.path("zzz")))
	require.NoError(t, os.Rename(env.path("two.bin"), env.path("aaa")))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, par2.StateOK, vtFile(t, res, "one.bin").State)
	require.Equal(t, par2.StateOK, vtFile(t, res, "two.bin").State)
	require.Equal(t, f1.Data, env.read("one.bin"))
	require.Equal(t, f2.Data, env.read("two.bin"))
}

// twins builds two files with identical length and first 16 KiB whose tails
// differ, so only the full MD5 (or the slices) tells them apart.
func twins() (fileSpec, fileSpec) {
	base := testFiles(20000)[0].Data
	a := append([]byte(nil), base...)
	b := append([]byte(nil), base...)
	b[19000] ^= 0xFF
	return fileSpec{Name: "twinA.bin", Data: a}, fileSpec{Name: "twinB.bin", Data: b}
}

func TestVerify_IdenticalKeyIsDisambiguatedByFullMD5(t *testing.T) {
	a, b := twins()
	env := newVTEnv(t, 1024, []fileSpec{a, b})
	// Names sort opposite to set order, so "first candidate" would be wrong.
	require.NoError(t, os.Rename(env.path("twinA.bin"), env.path("zzz")))
	require.NoError(t, os.Rename(env.path("twinB.bin"), env.path("aaa")))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, par2.StateOK, vtFile(t, res, "twinA.bin").State)
	require.Equal(t, par2.StateOK, vtFile(t, res, "twinB.bin").State)
	require.Equal(t, a.Data, env.read("twinA.bin"))
	require.Equal(t, b.Data, env.read("twinB.bin"))
}

func TestVerify_UnresolvableAmbiguityMatchesFirstAndLeavesTheOtherUnmatched(t *testing.T) {
	a, b := twins()
	env := newVTEnv(t, 1024, []fileSpec{a, b})
	// One candidate with the shared key, intact for neither description.
	damaged := append([]byte(nil), a.Data...)
	damaged[17000] ^= 1
	require.NoError(t, os.Remove(env.path("twinA.bin")))
	require.NoError(t, os.Remove(env.path("twinB.bin")))
	env.write("zzz", damaged)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	ra := vtFile(t, res, "twinA.bin")
	require.Equal(t, par2.StateDamaged, ra.State) // first in set order wins
	require.Equal(t, "zzz", ra.Renamed.From)
	require.Equal(t, par2.StateMissing, vtFile(t, res, "twinB.bin").State)
}

func TestVerify_TwoIdenticalCopiesPickTheIntactOne(t *testing.T) {
	files := testFiles(20000)
	env := newVTEnv(t, 1024, files)
	copyData := env.read("fileA.bin")
	require.NoError(t, os.Remove(env.path("fileA.bin")))
	broken := append([]byte(nil), copyData...)
	broken[19999] ^= 1
	env.write("aaa-broken", broken) // sorts first
	env.write("zzz-good", copyData)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, par2.StateOK, vtFile(t, res, "fileA.bin").State)
	require.Equal(t, []string{"aaa-broken", "fileA.bin"}, env.names())
}

func TestVerify_RenameTargetOccupiedByDifferentThingGetsUniqueName(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files)
	require.NoError(t, os.Rename(env.path("fileA.bin"), env.path("obf")))
	require.NoError(t, os.Mkdir(env.path("fileA.bin"), 0o700)) // not a regular file

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	a := vtFile(t, res, "fileA.bin.1") // fsutil.Unique appends the counter to directories
	require.Equal(t, par2.StateOK, a.State)
	require.Equal(t, &par2.Rename{From: "obf", To: "fileA.bin.1"}, a.Renamed)
	require.Equal(t, []string{"fileA.bin", "fileA.bin.1"}, env.names())
	st, err := os.Stat(env.path("fileA.bin"))
	require.NoError(t, err)
	require.True(t, st.IsDir())
}

func TestVerify_DescriptionNamesCollidingAfterSafeNameStayDistinct(t *testing.T) {
	f1 := fileSpec{Name: "a:b.bin", Data: testFiles(2000)[0].Data}
	f2 := fileSpec{Name: "a?b.bin", Data: testFiles(2000, 3000)[1].Data}
	env := newVTEnv(t, 1024, []fileSpec{f1, f2})
	// Both are called a_b.bin after SafeName; the files sit under other names.
	require.NoError(t, os.Remove(env.path("a:b.bin")))
	require.NoError(t, os.Remove(env.path("a?b.bin")))
	env.write("x1", f1.Data)
	env.write("x2", f2.Data)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Len(t, res.Files, 2)
	require.Equal(t, "a_b.bin", res.Files[0].Name)
	require.Equal(t, "a_b.1.bin", res.Files[1].Name)
	require.Equal(t, par2.StateOK, res.Files[0].State)
	require.Equal(t, par2.StateOK, res.Files[1].State)
	require.Equal(t, f1.Data, env.read("a_b.bin"))
	require.Equal(t, f2.Data, env.read("a_b.1.bin"))
}

func TestVerify_DryRunPlansDistinctNamesForCollidingTargets(t *testing.T) {
	f1 := fileSpec{Name: "a:b.bin", Data: testFiles(2000)[0].Data}
	f2 := fileSpec{Name: "a?b.bin", Data: testFiles(2000, 3000)[1].Data}
	env := newVTEnv(t, 1024, []fileSpec{f1, f2})
	require.NoError(t, os.Remove(env.path("a:b.bin")))
	require.NoError(t, os.Remove(env.path("a?b.bin")))
	env.write("x1", f1.Data)
	env.write("x2", f2.Data)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set, par2.WithDryRun())

	require.NoError(t, err)
	require.NotEqual(t, res.Files[0].Renamed.To, res.Files[1].Renamed.To)
	require.Equal(t, []string{"x1", "x2"}, env.names())
}

func TestVerify_HostileDescriptionNameCannotEscapeTheDirectory(t *testing.T) {
	f := fileSpec{Name: "../../evil.bin", Data: testFiles(2000)[0].Data}
	env := newVTEnvBare(t, 1024, []fileSpec{f})
	env.write("obf", f.Data)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, par2.StateOK, res.Files[0].State)
	require.Equal(t, ".._.._evil.bin", res.Files[0].Name)
	require.Equal(t, []string{".._.._evil.bin"}, env.names())
}

func TestVerify_SymlinkIsNeverFollowedMatchedOrRenamed(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files)
	outside := t.TempDir()
	outFile := writeFile(t, outside, "secret", files[0].Data)
	require.NoError(t, os.Remove(env.path("fileA.bin")))
	// One link under the expected name, one under another name: both point at
	// a file with exactly the right content.
	require.NoError(t, os.Symlink(outFile, env.path("fileA.bin")))
	require.NoError(t, os.Symlink(outFile, env.path("obf")))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, par2.StateMissing, vtFile(t, res, "fileA.bin").State)
	require.Equal(t, []string{"fileA.bin", "obf"}, env.names())
	for _, n := range env.names() {
		st, err := os.Lstat(env.path(n))
		require.NoError(t, err)
		require.NotZero(t, st.Mode()&os.ModeSymlink, n)
	}
	got, err := os.ReadFile(outFile)
	require.NoError(t, err)
	require.Equal(t, files[0].Data, got)
}

func TestVerify_DirectoriesAreNotFiles(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files)
	require.NoError(t, os.Remove(env.path("fileA.bin")))
	require.NoError(t, os.Mkdir(env.path("fileA.bin"), 0o700))
	writeFile(t, env.path("fileA.bin"), "inner", files[0].Data)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, par2.StateMissing, vtFile(t, res, "fileA.bin").State)
	require.Empty(t, res.Files[0].Renamed)
}

func TestVerify_UnrelatedFilesAreLeftAlone(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files)
	env.write("notes.txt", []byte("hello"))
	env.write("empty", nil)
	env.write("same-length", make([]byte, 3000))

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.False(t, res.NeedsRepair())
	require.Equal(t, []string{"empty", "fileA.bin", "notes.txt", "same-length"}, env.names())
}

func TestVerify_ContextCancelledBeforeStartReturnsItsError(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := par2.Verify(ctx, env.Dir, env.Set)

	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, res)
}

// cancelAfterReads is a context that reports cancellation after Err was
// called n times, which lands the cancellation in the middle of a file.
type cancelAfterReads struct {
	context.Context
	left int
}

func (c *cancelAfterReads) Err() error {
	if c.left--; c.left < 0 {
		return context.Canceled
	}
	return nil
}

func TestVerify_ContextCancelledMidFileReturnsItsError(t *testing.T) {
	files := testFiles(5 << 20) // five 1 MiB chunks in the fast path
	env := newVTEnv(t, 1<<20, files)

	res, err := par2.Verify(&cancelAfterReads{Context: context.Background(), left: 3}, env.Dir, env.Set)

	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, res)
}

func TestVerify_ContextCancelledMidSliceVerificationReturnsItsError(t *testing.T) {
	files := testFiles(5 << 20)
	env := newVTEnv(t, 1<<20, files)
	data := env.read("fileA.bin")
	data[0] ^= 1
	env.write("fileA.bin", data)

	res, err := par2.Verify(&cancelAfterReads{Context: context.Background(), left: 7}, env.Dir, env.Set)

	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, res)
}

func TestVerify_RecoveryArithmeticIncludesDeferredBlocks(t *testing.T) {
	files := testFiles(5 * 1024)
	env := newVTEnv(t, 1024, files, 0, 1) // two recovery slices on disk
	data := env.read("fileA.bin")
	for _, i := range []int{0, 2, 4} { // three damaged slices
		data[i*1024] ^= 1
	}
	env.write("fileA.bin", data)
	ctx := context.Background()

	res, err := par2.Verify(ctx, env.Dir, env.Set)
	require.NoError(t, err)
	require.Equal(t, []int{0, 2, 4}, res.Files[0].DamagedSlices)
	require.Equal(t, 3, res.RecoverySlicesNeeded())
	require.Equal(t, 2, res.RecoverySlicesOnDisk)
	require.Zero(t, res.RecoverySlicesDeferred)
	require.False(t, res.Repairable())

	res, err = par2.Verify(ctx, env.Dir, env.Set, par2.WithDeferredRecoveryBlocks(1))
	require.NoError(t, err)
	require.Equal(t, 1, res.RecoverySlicesDeferred)
	require.True(t, res.Repairable())
	require.True(t, res.NeedsRepair())

	res, err = par2.Verify(ctx, env.Dir, env.Set, par2.WithDeferredRecoveryBlocks(-5))
	require.NoError(t, err)
	require.Zero(t, res.RecoverySlicesDeferred)
}

func TestVerify_ZeroLengthFileInSet(t *testing.T) {
	files := []fileSpec{{Name: "empty.txt"}, testFiles(1000)[0]}
	env := newVTEnv(t, 1024, files)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)
	require.NoError(t, err)
	require.False(t, res.NeedsRepair())
	e := vtFile(t, res, "empty.txt")
	require.Equal(t, par2.StateOK, e.State)
	require.Zero(t, e.TotalSlices)

	// Gone: Missing, nothing to recover (repair creates the empty file).
	require.NoError(t, os.Remove(env.path("empty.txt")))
	res, err = par2.Verify(context.Background(), env.Dir, env.Set)
	require.NoError(t, err)
	e = vtFile(t, res, "empty.txt")
	require.Equal(t, par2.StateMissing, e.State)
	require.Zero(t, e.DamagedCount)
	require.True(t, res.NeedsRepair())
	require.True(t, res.Repairable())

	// Not empty any more.
	env.write("empty.txt", []byte("x"))
	res, err = par2.Verify(context.Background(), env.Dir, env.Set)
	require.NoError(t, err)
	require.Equal(t, par2.StateDamaged, vtFile(t, res, "empty.txt").State)
}

func TestVerify_ZeroLengthFileWithAnotherNameIsRenamed(t *testing.T) {
	files := []fileSpec{{Name: "empty.txt"}}
	env := newVTEnvBare(t, 1024, files)
	env.write("whatever", nil)

	res, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, par2.StateOK, res.Files[0].State)
	require.Equal(t, []string{"empty.txt"}, env.names())
}

func TestVerify_FileWithoutIFSCIsUnverifiableUnlessItsMD5Matches(t *testing.T) {
	files := testFiles(5000)
	env := newVTEnvNoIFSC(t, 1024, files)
	ctx := context.Background()

	res, err := par2.Verify(ctx, env.Dir, env.Set)
	require.NoError(t, err)
	require.Equal(t, par2.StateOK, res.Files[0].State)

	data := env.read("fileA.bin")
	data[10] ^= 1
	env.write("fileA.bin", data)
	res, err = par2.Verify(ctx, env.Dir, env.Set)
	require.NoError(t, err)
	a := res.Files[0]
	require.Equal(t, par2.StateUnverifiable, a.State)
	require.Empty(t, a.DamagedSlices)
	require.EqualValues(t, 5, a.DamagedCount)
	require.EqualValues(t, 5, res.DamagedSlices)
	require.True(t, res.NeedsRepair())
}

func TestVerify_HostileClaimedLengthsAreNotAllocated(t *testing.T) {
	var ids []par2.ID
	descs := map[par2.ID]*par2.FileDesc{}
	for i, name := range []string{"huge1.bin", "huge2.bin", "huge3.bin"} {
		id := par2.ID{byte(i + 1)}
		ids = append(ids, id)
		descs[id] = &par2.FileDesc{FileID: id, Name: name, Length: math.MaxUint64}
	}
	set := &par2.Set{SliceSize: 4, FileIDs: ids, FileDescs: descs, Recovery: map[uint32]par2.RecoverySlice{}}
	dir := t.TempDir()
	writeFile(t, dir, "huge3.bin", []byte("tiny")) // present, no IFSC

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res, err := par2.Verify(context.Background(), dir, set)
	runtime.ReadMemStats(&after)

	require.NoError(t, err)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<20))
	require.Equal(t, par2.StateMissing, res.Files[0].State)
	require.Nil(t, res.Files[0].DamagedSlices)
	require.Equal(t, par2.StateUnverifiable, res.Files[2].State)
	require.EqualValues(t, uint64(math.MaxUint64/4+1), res.Files[0].TotalSlices)
	require.Equal(t, math.MaxInt, res.RecoverySlicesNeeded())
	require.False(t, res.Repairable())
}

func TestVerify_RejectsUnusableSliceSizes(t *testing.T) {
	dir := t.TempDir()
	for _, size := range []uint64{0, 6} {
		_, err := par2.Verify(context.Background(), dir, &par2.Set{SliceSize: size})
		require.ErrorIs(t, err, par2.ErrInvalidSliceSize)
	}
	_, err := par2.Verify(context.Background(), dir, &par2.Set{SliceSize: par2.MaxSliceSize + 4})
	require.ErrorIs(t, err, par2.ErrSliceSizeTooLarge)
	_, err = par2.Verify(context.Background(), dir, nil)
	require.Error(t, err)
}

func TestVerify_HugeUnrelatedSparseFileIsNotRead(t *testing.T) {
	files := testFiles(3000)
	env := newVTEnv(t, 1024, files)
	f, err := os.Create(env.path("huge.sparse"))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1<<40))
	require.NoError(t, f.Close())

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	res, err := par2.Verify(context.Background(), env.Dir, env.Set)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	require.NoError(t, err)
	require.False(t, res.NeedsRepair())
	require.Less(t, elapsed, 5*time.Second)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<20))
}

func TestVerify_NeverChangesAnythingWithoutARename(t *testing.T) {
	files := testFiles(5000, 3000)
	env := newVTEnv(t, 1024, files, 0)
	data := env.read("fileA.bin")
	data[1] ^= 1
	env.write("fileA.bin", data)
	require.NoError(t, os.Remove(env.path("fileB.bin")))
	before := snapshot(t, env.Dir)

	_, err := par2.Verify(context.Background(), env.Dir, env.Set)

	require.NoError(t, err)
	require.Equal(t, before, snapshot(t, env.Dir))
}

// snapshot maps every directory entry to its mode, size, mtime and content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		info, err := e.Info()
		require.NoError(t, err)
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		out[e.Name()] = fmt.Sprintf("%v %d %d %x", info.Mode(), info.Size(), info.ModTime().UnixNano(), md5.Sum(data))
	}
	return out
}

// FuzzVerifySlices checks Verify's slice verdicts against a byte-comparison
// oracle for arbitrary contents of a file whose description is fixed.
func FuzzVerifySlices(f *testing.F) {
	orig := testFiles(300)[0]
	ts := newSet(16, orig)
	parDir := f.TempDir()
	writeFile(f, parDir, "set.par2", concat(ts.Index(), ts.Volume(0)))
	set, err := par2.LoadDir(parDir)
	require.NoError(f, err)
	dir := f.TempDir()

	f.Add(orig.Data)
	f.Add([]byte(nil))
	f.Add(orig.Data[:151])
	f.Add(append(bytes.Clone(orig.Data), "tail"...))
	flipped := bytes.Clone(orig.Data)
	flipped[299] ^= 1
	f.Add(flipped)

	f.Fuzz(func(t *testing.T, data []byte) {
		writeFile(t, dir, orig.Name, data)
		res, err := par2.Verify(context.Background(), dir, set)
		require.NoError(t, err)
		fr := res.Files[0]

		var want []int
		for i := range 19 { // ceil(300/16) slices
			start, end := i*16, min(i*16+16, 300)
			if len(data) < end || !bytes.Equal(data[start:end], orig.Data[start:end]) {
				want = append(want, i)
			}
		}
		if bytes.Equal(data, orig.Data) {
			require.Equal(t, par2.StateOK, fr.State)
			return
		}
		require.Equal(t, par2.StateDamaged, fr.State)
		require.Equal(t, want, fr.DamagedSlices)
		require.EqualValues(t, len(want), fr.DamagedCount)
	})
}
