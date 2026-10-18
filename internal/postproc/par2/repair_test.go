package par2_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func TestRepair_SingleDamagedSliceIsRestoredByteExactly(t *testing.T) {
	files := testFiles(5000)
	env := newRepairEnv(t, 1024, files, exps(0, 3)...)
	env.corruptSlice("fileA.bin", 2)

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 1, res.RepairedSlices)
	require.False(t, res.Verify.NeedsRepair())
	env.requireIntact(files)
}

// damageSlices corrupts n slices spread evenly over every slice of the set.
func damageSlices(env *vtEnv, files []fileSpec, n int) int {
	type loc struct {
		name  string
		slice int
	}
	var all []loc
	for _, f := range files {
		for s := 0; s*env.TS.SliceSize < len(f.Data); s++ {
			all = append(all, loc{f.Name, s})
		}
	}
	for i := range n {
		l := all[i*len(all)/n]
		env.corruptSlice(l.name, l.slice)
	}
	return n
}

func TestRepair_DamageAtOnePercentHalfAndFullCapacityIsRestoredByteExactly(t *testing.T) {
	const capacity = 100
	for _, damage := range []int{1, 50, 100} {
		t.Run(fmt.Sprintf("%d%%", damage), func(t *testing.T) {
			files := testFiles(40_000, 33_333, 100_000, 7_000, 51_200)
			env := newRepairEnv(t, 2048, files, exps(0, capacity)...)

			n := damageSlices(env, files, damage*capacity/100)
			res, err := env.repair()

			require.NoError(t, err)
			require.Equal(t, n, res.RepairedSlices)
			require.False(t, res.Verify.NeedsRepair())
			env.requireIntact(files)
		})
	}
}

func TestRepair_DamageSpanningFilesAndSliceBoundariesIsRestored(t *testing.T) {
	files := testFiles(1500, 1024, 2049, 100)
	env := newRepairEnv(t, 1024, files, exps(0, 8)...)
	// Last slice of A, all of B, first and last slice of C, D (one short slice).
	env.corruptSlice("fileA.bin", 1)
	env.corruptSlice("fileB.bin", 0)
	env.corruptSlice("fileC.bin", 0)
	env.corruptSlice("fileC.bin", 2)
	env.corruptSlice("fileD.bin", 0)

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 5, res.RepairedSlices)
	env.requireIntact(files)
}

func TestRepair_DamagedLastPartialSliceIsTrimmedToTheFileLength(t *testing.T) {
	files := testFiles(2500) // slices of 1024: 1024, 1024, 452
	env := newRepairEnv(t, 1024, files, exps(0, 2)...)
	env.corruptSlice("fileA.bin", 2)

	_, err := env.repair()

	require.NoError(t, err)
	require.Len(t, env.read("fileA.bin"), 2500)
	env.requireIntact(files)
}

func TestRepair_DamageBeyondCapacityFailsWithCountsAndChangesNothing(t *testing.T) {
	files := testFiles(20_000, 10_000)
	env := newRepairEnv(t, 1024, files, exps(0, 5)...)
	damageSlices(env, files, 6)
	before := dirDigests(t, env.Dir)

	res, err := env.repair()

	require.Nil(t, res)
	require.ErrorIs(t, err, par2.ErrInsufficientRecovery)
	var ie *par2.InsufficientError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, 6, ie.Needed)
	require.Equal(t, 5, ie.Available)
	require.Equal(t, before, dirDigests(t, env.Dir))
}

func TestRepair_MissingFileCountsAllItsSlicesAgainstCapacityForDeferredFetching(t *testing.T) {
	files := testFiles(10_000, 3_000)
	env := newRepairEnv(t, 1024, files, exps(0, 3)...)
	require.NoError(t, os.Remove(env.path("fileA.bin"))) // 10 slices

	_, err := env.repair()

	var ie *par2.InsufficientError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, 10, ie.Needed)
	require.Equal(t, 3, ie.Available)
	require.NoFileExists(t, env.path("fileA.bin"))
}

func TestRepair_MissingFileIsRecreatedByteExactly(t *testing.T) {
	files := testFiles(5000, 3000, 1000)
	env := newRepairEnv(t, 1024, files, exps(0, 10)...)
	require.NoError(t, os.Remove(env.path("fileB.bin"))) // 3 slices, the last one short

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 3, res.RepairedSlices)
	require.Equal(t, []string{env.path("fileB.bin")}, res.FilesCreated)
	require.Contains(t, res.FilesTouched, env.path("fileB.bin"))
	env.requireIntact(files)
}

func TestRepair_TrailingGarbageIsTruncatedWithoutUsingRecoverySlices(t *testing.T) {
	files := testFiles(3000, 2000)
	env := newRepairEnv(t, 1024, files) // no recovery slices at all
	env.write("fileA.bin", append(env.read("fileA.bin"), []byte("trailing garbage")...))

	res, err := env.repair()

	require.NoError(t, err)
	require.Zero(t, res.RepairedSlices)
	require.Equal(t, []string{env.path("fileA.bin")}, res.FilesTruncated)
	require.Equal(t, []string{env.path("fileA.bin")}, res.FilesTouched)
	require.False(t, res.Verify.NeedsRepair())
	env.requireIntact(files)
}

func TestRepair_MissingZeroLengthFileIsCreated(t *testing.T) {
	files := append(testFiles(2000), fileSpec{Name: "empty.txt"})
	env := newRepairEnv(t, 1024, files)
	require.NoError(t, os.Remove(env.path("empty.txt")))

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, []string{env.path("empty.txt")}, res.FilesCreated)
	require.FileExists(t, env.path("empty.txt"))
	require.False(t, res.Verify.NeedsRepair())
}

func TestRepair_NothingToDoLeavesEverythingAndReturnsTheVerifyResult(t *testing.T) {
	files := testFiles(2000)
	env := newRepairEnv(t, 1024, files, exps(0, 2)...)
	before := dirDigests(t, env.Dir)

	res, err := env.repair()

	require.NoError(t, err)
	require.Zero(t, res.RepairedSlices)
	require.Empty(t, res.FilesTouched)
	require.Equal(t, before, dirDigests(t, env.Dir))
}

func TestRepair_WrongLengthFileWithDamagedSlicesIsRepairedAndTrimmed(t *testing.T) {
	files := testFiles(3000)
	env := newRepairEnv(t, 1024, files, exps(0, 2)...)
	env.corruptSlice("fileA.bin", 1)
	env.write("fileA.bin", append(env.read("fileA.bin"), make([]byte, 700)...))

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 1, res.RepairedSlices)
	require.Equal(t, []string{env.path("fileA.bin")}, res.FilesTruncated)
	env.requireIntact(files)
}

func TestRepair_FileCutShortIsExtendedAndItsLostSlicesRecovered(t *testing.T) {
	files := testFiles(5000)
	env := newRepairEnv(t, 1024, files, exps(0, 4)...)
	env.write("fileA.bin", env.read("fileA.bin")[:2500]) // loses slices 2, 3 and 4

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 3, res.RepairedSlices)
	require.Empty(t, res.FilesTruncated)
	env.requireIntact(files)
}

func TestRepair_FileWithoutSliceChecksumsIsRebuiltWhole(t *testing.T) {
	files := testFiles(3000, 2000)
	ts := newSet(1024, files...)
	idx := ts.creatorPacket("test")
	idx = append(idx, ts.mainPacket()...)
	for i := range files {
		idx = append(idx, ts.fileDescPacket(i)...)
		if i == 1 { // only file B has checksums
			idx = append(idx, ts.ifscPacket(i)...)
		}
	}
	env := finishVTEnv(t, ts, concat(idx, realVolume(t, ts, exps(0, 4)...)), files)
	env.corruptSlice("fileA.bin", 1)

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 3, res.RepairedSlices)
	env.requireIntact(files)
}

func TestRepair_ObfuscatedAndDamagedFileIsRepairedUnderItsRealName(t *testing.T) {
	files := testFiles(4000, 40_000)
	env := newRepairEnv(t, 1024, files, exps(0, 3)...)
	require.NoError(t, os.Rename(env.path("fileB.bin"), env.path("a1b2c3")))
	data := env.read("a1b2c3")
	data[30_000] ^= 0xFF // the first 16 KiB must stay intact for the content match
	env.write("a1b2c3", data)

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 1, res.RepairedSlices)
	env.requireIntact(files)
	require.NoFileExists(t, env.path("a1b2c3"))
}

func TestRepair_DryRunVerifyResultWithOldNameIsStillRepairable(t *testing.T) {
	files := testFiles(40_000)
	env := newRepairEnv(t, 1024, files, exps(0, 3)...)
	require.NoError(t, os.Rename(env.path("fileA.bin"), env.path("zz9")))
	data := env.read("zz9")
	data[30_000] ^= 0xFF
	env.write("zz9", data)
	vr, err := par2.Verify(context.Background(), env.Dir, env.Set, par2.WithDryRun())
	require.NoError(t, err)

	res, err := par2.Repair(context.Background(), env.Dir, env.Set, vr)

	require.NoError(t, err)
	require.Equal(t, 1, res.RepairedSlices)
	env.requireIntact(files)
}

func TestRepair_ManyFilesAreRepairedWithFewOpenHandles(t *testing.T) {
	sizes := make([]int, 150)
	for i := range sizes {
		sizes[i] = 600 + i
	}
	files := testFiles(sizes...)
	for i := range files {
		files[i].Name = fmt.Sprintf("part%03d.bin", i)
	}
	env := newRepairEnv(t, 512, files, exps(0, 40)...)
	for i := range 40 {
		env.corruptSlice(files[i*3].Name, 1)
	}

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 40, res.RepairedSlices)
	env.requireIntact(files)
}

// Exponents 21845 and 43690 are multiples of 65535/3, so inputs 0 and 2
// (constants 2^1 and 2^4, whose exponents differ by 3) have equal powers for
// both: the 2x2 matrix of those two recovery slices is singular for them.
const sing1, sing2 = 21845, 43690

func TestRepair_SingularFirstChoiceFallsBackToAnotherWindow(t *testing.T) {
	files := testFiles(5000)
	env := newRepairEnv(t, 1024, files, sing1, sing2, 50_000, 60_000)
	env.corruptSlice("fileA.bin", 0)
	env.corruptSlice("fileA.bin", 2)

	res, err := env.repair()

	require.NoError(t, err)
	require.Equal(t, 2, res.RepairedSlices)
	env.requireIntact(files)
}

func TestRepair_SingularWithoutAlternativesFailsAndChangesNothing(t *testing.T) {
	files := testFiles(5000)
	env := newRepairEnv(t, 1024, files, sing1, sing2)
	env.corruptSlice("fileA.bin", 0)
	env.corruptSlice("fileA.bin", 2)
	before := dirDigests(t, env.Dir)

	res, err := env.repair()

	require.Nil(t, res)
	require.ErrorIs(t, err, par2.ErrSingularRecovery)
	require.Equal(t, before, dirDigests(t, env.Dir))
}

func TestRepair_RecoverySlicesThatCannotBeReadAreNotCounted(t *testing.T) {
	files := testFiles(5000)
	env := newRepairEnv(t, 1024, files, exps(0, 4)...)
	env.corruptSlice("fileA.bin", 1)
	env.corruptSlice("fileA.bin", 3)
	env.corruptSlice("fileA.bin", 4)
	// Exponent 0 claims a short slice; exponent 1 lies past the end of its file.
	rs := env.Set.Recovery[0]
	rs.Length = 100
	env.Set.Recovery[0] = rs
	rs = env.Set.Recovery[1]
	rs.Offset += 1 << 20
	env.Set.Recovery[1] = rs

	_, err := env.repair()

	var ie *par2.InsufficientError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, 3, ie.Needed)
	require.Equal(t, 2, ie.Available)
}

func TestRepair_RecoveryFileTruncatedAfterLoadingDropsItsSlices(t *testing.T) {
	files := testFiles(3000)
	ts := newSet(1024, files...)
	parDir := t.TempDir()
	writeFile(t, parDir, "a.par2", concat(ts.Index(), realVolume(t, ts, 0, 1)))
	vol := writeFile(t, parDir, "b.vol.par2", realVolume(t, ts, 2, 3))
	set, err := par2.LoadDir(parDir)
	require.NoError(t, err)
	dir := t.TempDir()
	for _, f := range files {
		writeFile(t, dir, f.Name, f.Data)
	}
	require.NoError(t, os.Truncate(vol, 100)) // after the load
	data := append([]byte(nil), files[0].Data...)
	for _, i := range []int{0, 1100, 2200} { // one byte in each of the three slices
		data[i] ^= 1
	}
	writeFile(t, dir, files[0].Name, data)
	vr, err := par2.Verify(context.Background(), dir, set)
	require.NoError(t, err)

	_, err = par2.Repair(context.Background(), dir, set, vr)

	var ie *par2.InsufficientError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, 3, ie.Needed)
	require.Equal(t, 2, ie.Available)
}

func TestRepair_WrongRecoveryDataLeavesRemainingDamage(t *testing.T) {
	files := testFiles(3000)
	ts := newSet(1024, files...)
	// ts.Volume carries arbitrary bytes instead of real recovery data.
	env := finishVTEnv(t, ts, concat(ts.Index(), ts.Volume(0, 1)), files)
	env.corruptSlice("fileA.bin", 1)

	res, err := env.repair()

	require.Nil(t, res)
	require.ErrorIs(t, err, par2.ErrRepairFailed)
	var rd *par2.RemainingDamageError
	require.ErrorAs(t, err, &rd)
	require.True(t, rd.Result.NeedsRepair())
	require.EqualValues(t, 1, rd.Result.DamagedSlices)
}

func TestRepair_MemoryLimitShrinksTheChunkAndStillRepairs(t *testing.T) {
	files := testFiles(200_000)
	env := newRepairEnv(t, 64*1024, files, exps(0, 3)...)
	env.corruptSlice("fileA.bin", 1)
	env.corruptSlice("fileA.bin", 3)
	env.corruptSlice("fileA.bin", 0)
	chunks := 0

	// 3 damaged slices: the decoder needs ~54 bytes; 4 buffers of 8 KiB fit, 16 KiB do not.
	res, err := env.repair(par2.WithMemoryLimit(40*1024), par2.WithAfterChunk(func(int) { chunks++ }))

	require.NoError(t, err)
	require.Equal(t, 3, res.RepairedSlices)
	require.Equal(t, 8, chunks) // 64 KiB slices in 8 KiB chunks
	env.requireIntact(files)
}

func TestRepair_MemoryLimitTooSmallFailsBeforeTouchingAnything(t *testing.T) {
	files := testFiles(200_000)
	env := newRepairEnv(t, 64*1024, files, exps(0, 3)...)
	env.corruptSlice("fileA.bin", 1)
	before := dirDigests(t, env.Dir)

	_, err := env.repair(par2.WithMemoryLimit(8 * 1024)) // 2 buffers of 4 KiB do not fit

	require.ErrorIs(t, err, par2.ErrTooLarge)
	require.Equal(t, before, dirDigests(t, env.Dir))
}

func TestRepair_CancelledMidRepairLeavesGoodSlicesUntouched(t *testing.T) {
	files := testFiles(40_000, 9_000)
	env := newRepairEnv(t, 4096, files, exps(0, 4)...)
	env.corruptSlice("fileA.bin", 3)
	env.corruptSlice("fileA.bin", 5)
	damaged := env.read("fileA.bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res, err := par2.Repair(ctx, env.Dir, env.Set, env.verify(), par2.WithChunkSize(1024),
		par2.WithAfterChunk(func(i int) {
			if i == 1 {
				cancel()
			}
		}))

	require.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)
	got := env.read("fileA.bin")
	require.Len(t, got, len(damaged))
	for s := 0; s*4096 < len(got); s++ {
		if s == 3 || s == 5 {
			continue
		}
		lo, hi := s*4096, min((s+1)*4096, len(got))
		require.Equal(t, files[0].Data[lo:hi], got[lo:hi], "good slice %d was modified", s)
	}
	require.Equal(t, files[1].Data, env.read("fileB.bin"))
}

func TestRepair_CancelledMissingFileIsNotLeftBehind(t *testing.T) {
	files := testFiles(8000)
	env := newRepairEnv(t, 4096, files, exps(0, 2)...)
	require.NoError(t, os.Remove(env.path("fileA.bin")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := par2.Repair(ctx, env.Dir, env.Set, env.verify(), par2.WithChunkSize(1024),
		par2.WithAfterChunk(func(int) { cancel() }))

	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, env.path("fileA.bin"))
}

func TestRepair_AlreadyCancelledContextDoesNothing(t *testing.T) {
	files := testFiles(3000)
	env := newRepairEnv(t, 1024, files, exps(0, 2)...)
	env.corruptSlice("fileA.bin", 1)
	vr := env.verify()
	before := dirDigests(t, env.Dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := par2.Repair(ctx, env.Dir, env.Set, vr)

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, dirDigests(t, env.Dir))
}

func TestRepair_HostileClaimedSliceCountsFailCheaply(t *testing.T) {
	var ids []par2.ID
	descs := map[par2.ID]*par2.FileDesc{}
	for i, name := range []string{"huge1.bin", "huge2.bin"} {
		id := par2.ID{byte(i + 1)}
		ids = append(ids, id)
		descs[id] = &par2.FileDesc{FileID: id, Name: name, Length: math.MaxUint64}
	}
	set := &par2.Set{SliceSize: 4, FileIDs: ids, FileDescs: descs, Recovery: map[uint32]par2.RecoverySlice{}}
	dir := t.TempDir()
	vr, err := par2.Verify(context.Background(), dir, set)
	require.NoError(t, err)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = par2.Repair(context.Background(), dir, set, vr)
	runtime.ReadMemStats(&after)

	var ie *par2.InsufficientError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, math.MaxInt, ie.Needed)
	require.Zero(t, ie.Available)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(4<<20))
	require.Empty(t, dirDigests(t, dir))
}

func TestRepair_MoreInputSlicesThanPAR2AllowsIsRefused(t *testing.T) {
	const n = 32769
	id := par2.ID{1}
	set := &par2.Set{
		SliceSize: 4,
		FileIDs:   []par2.ID{id},
		FileDescs: map[par2.ID]*par2.FileDesc{id: {FileID: id, Name: "big.bin", Length: 4 * n}},
		Recovery:  map[uint32]par2.RecoverySlice{},
	}
	dir := t.TempDir()
	rec := writeFile(t, dir, "rec.bin", make([]byte, 4))
	for e := range uint32(n) { // every exponent claims the same four bytes
		set.Recovery[e] = par2.RecoverySlice{Path: rec, Length: 4}
	}
	vr, err := par2.Verify(context.Background(), dir, set)
	require.NoError(t, err)

	_, err = par2.Repair(context.Background(), dir, set, vr)

	require.ErrorIs(t, err, par2.ErrTooManySlices)
	require.NoFileExists(t, filepath.Join(dir, "big.bin"))
}

func TestRepair_ResultThatDoesNotDescribeTheSetIsRefused(t *testing.T) {
	files := testFiles(3000)
	env := newRepairEnv(t, 1024, files, exps(0, 2)...)
	env.corruptSlice("fileA.bin", 1)
	good := env.verify()
	before := dirDigests(t, env.Dir)

	clone := func(mutate func(r *par2.Result)) *par2.Result {
		r := *good
		r.Files = append([]par2.FileResult(nil), good.Files...)
		mutate(&r)
		return &r
	}
	cases := map[string]*par2.Result{
		"wrong slice total":  clone(func(r *par2.Result) { r.Files[0].TotalSlices = 99 }),
		"slice out of range": clone(func(r *par2.Result) { r.Files[0].DamagedSlices = []int{7}; r.Files[0].DamagedCount = 1 }),
		"count mismatch":     clone(func(r *par2.Result) { r.Files[0].DamagedCount = 2 }),
		"unsorted list":      clone(func(r *par2.Result) { r.Files[0].DamagedSlices = []int{2, 1}; r.Files[0].DamagedCount = 2 }),
		"file missing":       clone(func(r *par2.Result) { r.Files = nil }),
		"duplicate file":     clone(func(r *par2.Result) { r.Files = append(r.Files, r.Files[0]) }),
	}
	for name, vr := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := par2.Repair(context.Background(), env.Dir, env.Set, vr)
			require.ErrorIs(t, err, par2.ErrInvalidResult)
			require.Equal(t, before, dirDigests(t, env.Dir))
		})
	}
	_, err := par2.Repair(context.Background(), env.Dir, env.Set, nil)
	require.ErrorIs(t, err, par2.ErrInvalidResult)
}

func TestRepair_HostileFileNameCannotEscapeTheDirectory(t *testing.T) {
	files := testFiles(3000)
	files[0].Name = "../../escape.bin"
	root := t.TempDir()
	parDir := filepath.Join(root, "par")
	dir := filepath.Join(root, "a", "b", "dl")
	require.NoError(t, os.MkdirAll(parDir, 0o755))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	ts := newSet(1024, files...)
	writeFile(t, parDir, "s.par2", concat(ts.Index(), realVolume(t, ts, exps(0, 3)...)))
	set, err := par2.LoadDir(parDir)
	require.NoError(t, err)
	vr, err := par2.Verify(context.Background(), dir, set)
	require.NoError(t, err)

	res, err := par2.Repair(context.Background(), dir, set, vr)

	require.NoError(t, err)
	require.Len(t, res.FilesCreated, 1)
	require.Equal(t, dir, filepath.Dir(res.FilesCreated[0]))
	require.NoFileExists(t, filepath.Join(root, "a", "escape.bin"))
	require.NoFileExists(t, filepath.Join(root, "escape.bin"))
	got, err := os.ReadFile(res.FilesCreated[0])
	require.NoError(t, err)
	require.Equal(t, files[0].Data, got)
}

func TestRepair_ResultPathOutsideTheDirectoryIsRefused(t *testing.T) {
	files := testFiles(3000)
	env := newRepairEnv(t, 1024, files, exps(0, 2)...)
	env.corruptSlice("fileA.bin", 1)
	vr := env.verify()
	outside := t.TempDir()
	victim := writeFile(t, outside, "victim.bin", []byte("do not touch"))
	vr.Files[0].Path = victim

	_, err := par2.Repair(context.Background(), env.Dir, env.Set, vr)

	require.ErrorIs(t, err, fsutil.ErrUnsafePath)
	require.Equal(t, []byte("do not touch"), mustRead(t, victim))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
