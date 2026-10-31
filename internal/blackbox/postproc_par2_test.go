package blackbox_test

import (
	"bytes"
	"crypto/md5"
	binenc "encoding/binary"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PAR2 verify and repair with the real par2, deferred recovery volumes, and
// the failures of a release that cannot be repaired.

// blockSize is the block size of the sets: the size of one article, so a lost
// or damaged article is exactly one block of the file.
const blockSize = 3000

// data makes a file of n blocks (the last one one byte short) from the seed.
func blocks(seed uint64, n int) []byte { return randBytes(seed, n*blockSize-1) }

func TestPostProcPar2RepairsLostAndDamagedArticles(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a, b := blocks(20, 8), blocks(21, 4)
	nfo := []byte("nfo\n")
	set := makePar2(t, "rel", []relSpec{{Name: "a.bin", Data: a}, {Name: "b.bin", Data: b}}, blockSize, 6, 1)
	rel := r.serve(newRelease(t, []relSpec{
		{Name: "a.bin", Data: a}, {Name: "b.bin", Data: b}, {Name: "rel.nfo", Data: nfo},
		set.OneFile(t, "rel"), // index and recovery blocks in one file: nothing is deferred
	}))
	fa, fb := fileOfRel(t, rel, "a.bin"), fileOfRel(t, rel, "b.bin")
	r.srv.Missing(fa.IDs[2], fb.IDs[1]).Corrupt(fa.IDs[5])

	id := r.add(rel, "Repaired.Release")
	out := r.ready(id)

	// Three damaged blocks, six recovery blocks: repaired in place. The
	// damaged copy par2 sets aside (a.bin.1) is deleted, and so is the set.
	requireOutput(t, out, id, tree{"a.bin": ppFile(a), "b.bin": ppFile(b), "rel.nfo": ppFile(nfo)})
	assert.Equal(t, append(ppTrace("verifying", "finishing"), "state:ready", "ready"), r.ev.traceUntil(id, "ready"),
		"repairing is part of the verifying stage: no stage of its own")
}

func TestPostProcPar2RebuildsAFileWhoseArticlesAreAllMissing(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a, b := blocks(22, 6), blocks(23, 3)
	set := makePar2(t, "rel", []relSpec{{Name: "a.bin", Data: a}, {Name: "b.bin", Data: b}}, blockSize, 4, 1)
	rel := r.serve(newRelease(t, []relSpec{
		{Name: "a.bin", Data: a}, {Name: "b.bin", Data: b}, set.OneFile(t, "rel"),
	}))
	r.srv.Missing(fileOfRel(t, rel, "b.bin").IDs...)

	id := r.add(rel, "Rebuilt.File")
	out := r.ready(id)

	// The fetch side never creates a file none of whose articles arrived;
	// par2 makes it from the recovery blocks.
	requireOutput(t, out, id, tree{"a.bin": ppFile(a), "b.bin": ppFile(b)})
}

func TestPostProcRecoveryVolumesAreFetchedOnlyWhenARepairNeedsThem(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a, b := blocks(24, 8), blocks(25, 4)
	// 15 recovery blocks in four volumes: +4, +4, +4 and +3.
	set := makePar2(t, "rel", []relSpec{{Name: "a.bin", Data: a}, {Name: "b.bin", Data: b}}, blockSize, 15, 4)
	require.Equal(t, []int{4, 4, 4, 3}, set.Blocks, "par2 made other volumes than this test counts on")
	rel := r.serve(newRelease(t, append([]relSpec{{Name: "a.bin", Data: a}, {Name: "b.bin", Data: b}}, set.all()...)))
	fa, fb := fileOfRel(t, rel, "a.bin"), fileOfRel(t, rel, "b.bin")
	// Five lost blocks: the volumes must hold at least six (the damage plus
	// a margin of one). The cheapest choice by bytes is +3 and one +4 (7
	// blocks).
	r.srv.Missing(fa.IDs[0], fa.IDs[3], fa.IDs[6], fb.IDs[1], fb.IDs[2])

	id := r.add(rel, "Deferred.Volumes")
	out := r.ready(id)

	requireOutput(t, out, id, tree{"a.bin": ppFile(a), "b.bin": ppFile(b)})
	tr := r.ev.traceUntil(id, "ready")
	assert.Equal(t, []string{
		"state:queued", "state:fetching", "state:fetched", "fetched", "state:post_processing", "stage:verifying",
		"state:fetching", "state:fetched", "fetched", "state:post_processing", "stage:verifying", "stage:finishing",
		"state:ready", "ready",
	}, tr, "the Download goes back to fetching for the volumes and is post-processed again from the first stage")

	// Exactly the +3 volume and one of the equally cheap +4 volumes were
	// fetched, each article once. (Which of the three +4 volumes the Go
	// planner takes is not part of the contract: it takes the second.)
	var fetchedVols []string
	for _, v := range set.Volumes {
		ids := idsOfNames(rel, v.Name)
		if requestedAny(r.srv, ids) {
			fetchedVols = append(fetchedVols, v.Name)
			for _, id := range ids {
				assert.Equal(t, 1, r.srv.Requests(id), "an article of a fetched volume once: %s", id)
			}
		}
	}
	require.Len(t, fetchedVols, 2, "fetched volumes: %v", fetchedVols)
	assert.Contains(t, fetchedVols, set.Volumes[3].Name, "the +3 volume is cheaper than any +4 one")
	// Every article of the data and the index was asked for before the first
	// article of a volume: the volumes are fetched only when the repair needs them.
	cmds := r.srv.Commands()
	volIDs := idsOfNames(rel, volumeNames(set)...)
	firstVol, lastData := -1, -1
	for i, c := range cmds {
		id, ok := strings.CutPrefix(c, "BODY <")
		if !ok {
			continue
		}
		id = strings.TrimSuffix(id, ">")
		if slices.Contains(volIDs, id) {
			if firstVol < 0 {
				firstVol = i
			}
		} else {
			lastData = i
		}
	}
	require.GreaterOrEqual(t, firstVol, 0)
	assert.Less(t, lastData, firstVol, "every article that is not a recovery volume came first")

	// The progress totals describe the first fetch only.
	var upFront int64
	for _, f := range rel.Files {
		if !slices.Contains(volumeNames(set), f.Name) {
			for _, e := range f.Encoded {
				upFront += int64(e)
			}
		}
	}
	m := r.d.download(id)
	assert.Equal(t, upFront, num(m, "total_bytes"), "total_bytes excludes the recovery volumes, also after they were fetched")
}

func TestPostProcNotEnoughRecoveryBlocksFailsTheDownload(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a := blocks(26, 8)
	set := makePar2(t, "rel", []relSpec{{Name: "a.bin", Data: a}}, blockSize, 3, 1)
	rel := r.serve(newRelease(t, []relSpec{{Name: "a.bin", Data: a}, set.OneFile(t, "rel")}))
	fa := fileOfRel(t, rel, "a.bin")
	r.srv.Missing(fa.IDs[0], fa.IDs[2], fa.IDs[4], fa.IDs[6], fa.IDs[7])

	id := r.add(rel, "Too.Damaged")
	m := r.failed(id, "repair_insufficient")

	assert.Equal(t, "repairing: not enough recovery data: need 5 blocks, have 3", m["failure_detail"])
	tr := r.ev.traceUntil(id, "failed:repair_insufficient")
	assert.Equal(t, append(ppTrace("verifying"), "state:failed", "failed:repair_insufficient"), tr)
	for _, f := range r.ev.of(id) {
		if f.name == "failed" {
			assert.Equal(t, map[string]any{"id": id, "reason": "repair_insufficient", "detail": m["failure_detail"]}, fjson(t, f))
		}
	}
	assert.NotNil(t, m["fetched_at"], "it was fetched")
	// Nothing is moved or deleted on a failure: the files wait in the work dir.
	assert.Empty(t, dirNames(t, r.roots.complete))
	assert.FileExists(t, filepath.Join(r.work(id), "a.bin"))
}

func TestPostProcRecoveryVolumesThatCannotCoverTheDamageAreNotFetched(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a := blocks(27, 8)
	set := makePar2(t, "rel", []relSpec{{Name: "a.bin", Data: a}}, blockSize, 4, 2)
	require.Equal(t, []int{2, 2}, set.Blocks)
	rel := r.serve(newRelease(t, append([]relSpec{{Name: "a.bin", Data: a}}, set.all()...)))
	fa := fileOfRel(t, rel, "a.bin")
	r.srv.Missing(fa.IDs[0], fa.IDs[1], fa.IDs[2], fa.IDs[3], fa.IDs[4])

	id := r.add(rel, "Hopeless")
	m := r.failed(id, "repair_insufficient")

	assert.Equal(t, "repairing: not enough recovery data: need 5 blocks, have 0", m["failure_detail"])
	assert.False(t, requestedAny(r.srv, idsOfNames(rel, volumeNames(set)...)),
		"when all volumes together fall short, none is fetched")
	assert.Equal(t, append(ppTrace("verifying"), "state:failed", "failed:repair_insufficient"), r.ev.traceUntil(id, "failed:repair_insufficient"))
}

func TestPostProcMissingArticlesWithoutAPar2SetFailTheDownload(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	rel := r.serve(oneFile(t, "lossy.bin", 28, 6))
	f := rel.Files[0]
	r.srv.Missing(f.IDs[1], f.IDs[2]).Corrupt(f.IDs[4])

	id := r.add(rel, "No.Par2")
	m := r.failed(id, "missing_articles")

	assert.Equal(t, "verifying: missing articles: 2 missing, 1 damaged and no PAR2 set", m["failure_detail"])
	tr := r.ev.traceUntil(id, "failed:missing_articles")
	assert.Equal(t, append(ppTrace("verifying"), "state:failed", "failed:missing_articles"), tr)
	assert.Empty(t, dirNames(t, r.roots.complete), "nothing reaches the complete dir")
	assert.FileExists(t, filepath.Join(r.work(id), "lossy.bin"))
}

func TestPostProcAMissingArticleOfAFileThatIsNeverCreatedStillFailsWithoutPar2(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	rel := r.serve(newRelease(t, []relSpec{{Name: "good.bin", Data: blocks(29, 3)}, {Name: "gone.bin", Data: blocks(30, 3)}}))
	r.srv.Missing(fileOfRel(t, rel, "gone.bin").IDs...)

	id := r.add(rel, "One.Gone")
	m := r.failed(id, "missing_articles")

	assert.Equal(t, "verifying: missing articles: 3 missing, 0 damaged and no PAR2 set", m["failure_detail"])
}

func TestPostProcObfuscatedFileNamesAreFixedByPar2(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a, b := blocks(31, 5), blocks(32, 2)
	set := makePar2(t, "rel", []relSpec{{Name: "Movie.2024.mkv", Data: a}, {Name: "Movie.2024.nfo", Data: b}}, blockSize, 6, 2)
	rel := r.serve(newRelease(t, append([]relSpec{
		{Name: "8d6f0a8c3e2b4f17a95d1c0e7b3a2f64", Data: a}, // posted under random names
		{Name: "0c4b7e1a9d2f", Data: b},
	}, set.all()...)))

	id := r.add(rel, "Obfuscated.Release")
	out := r.ready(id)

	// par2 recognises the files by their hashes and renames them to the
	// names of the set; the set's own files are removed.
	requireOutput(t, out, id, tree{"Movie.2024.mkv": ppFile(a), "Movie.2024.nfo": ppFile(b)})
	assert.False(t, requestedAny(r.srv, idsOfNames(rel, volumeNames(set)...)), "nothing was damaged: no volume was fetched")
}

func TestPostProcAFullyObfuscatedPostIsRepairedAndRenamedByContent(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a, b := blocks(33, 6), blocks(34, 3)
	set := makePar2(t, "rel", []relSpec{{Name: "Show.S01E01.mkv", Data: a}, {Name: "Show.S01E01.srt", Data: b}}, blockSize, 6, 2)
	// Every file, the PAR2 files too, has a random hex name without extension.
	hex := func(i int) string { return fmt.Sprintf("%032x", 0xabcdef01234567+i) }
	specs := []relSpec{{Name: hex(1), Data: a}, {Name: hex(2), Data: b}}
	for i, s := range set.all() {
		specs = append(specs, relSpec{Name: hex(10 + i), Data: s.Data})
	}
	rel := r.serve(newRelease(t, specs))
	// One block of the video is lost and one of the subtitles is damaged.
	r.srv.Missing(fileOfRel(t, rel, hex(1)).IDs[3]).Corrupt(fileOfRel(t, rel, hex(2)).IDs[0])

	id := r.add(rel, "Fully.Obfuscated")
	out := r.ready(id)

	requireOutput(t, out, id, tree{"Show.S01E01.mkv": ppFile(a), "Show.S01E01.srt": ppFile(b)})
}

func TestPostProcAnUnsafeFileNameInAPar2SetFailsBeforePar2IsStarted(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../evil.bin", "ABS/evil.bin", "sub/../../evil.bin", `..\evil.bin`, "evil\x01.bin"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			t.Parallel()
			r := newPPRig(t)
			name := strings.Replace(name, "ABS", r.roots.outside, 1) // an absolute path
			a := blocks(35, 2)
			set := makePar2(t, "rel", []relSpec{{Name: "good.bin", Data: a}}, blockSize, 1, 1)
			evil := renameInPar2(t, set.Index.Data, "good.bin", name)
			rel := r.serve(newRelease(t, []relSpec{
				{Name: "good.bin", Data: a}, {Name: "rel.par2", Data: evil},
			}))

			id := r.add(rel, "Hostile.Set")
			m := r.failed(id, "path_violation")

			assert.Contains(t, m["failure_detail"], "verifying: ", "the stage that refused it")
			assert.Equal(t, append(ppTrace("verifying"), "state:failed", "failed:path_violation"), r.ev.traceUntil(id, "failed:path_violation"))
			// Nothing was written outside the work dir, and the work dir
			// still has what was fetched.
			assert.Empty(t, findNamed(t, filepath.Dir(r.roots.complete), "evil"), "no file of that name anywhere in the test's tree")
			assert.Empty(t, dirNames(t, r.roots.complete))
			assert.FileExists(t, filepath.Join(r.work(id), "good.bin"))
		})
	}
}

// renameInPar2 rewrites the name of the one file a PAR2 index describes: the
// File Description packet gets the new name (with its length, padding and MD5
// recomputed), every other packet stays.
func renameInPar2(t testing.TB, par2 []byte, oldName, newName string) []byte {
	t.Helper()
	magic := []byte("PAR2\x00PKT")
	fileDesc := append([]byte("PAR 2.0\x00FileDesc"), 0)[:16]
	var out []byte
	rest := par2
	for len(rest) > 0 {
		require.True(t, bytes.HasPrefix(rest, magic), "a PAR2 packet starts here")
		n := int(binenc.LittleEndian.Uint64(rest[8:16]))
		pkt := rest[:n]
		rest = rest[n:]
		if !bytes.Equal(pkt[48:64], fileDesc) {
			out = append(out, pkt...)
			continue
		}
		body := slices.Clone(pkt[64:])
		fixed := body[:56]                         // file id, hashes and length
		name := bytes.TrimRight(body[56:], "\x00") // padded to 4 with NULs
		require.Equal(t, oldName, string(name))
		newBody := append(slices.Clone(fixed), newName...)
		for len(newBody)%4 != 0 {
			newBody = append(newBody, 0)
		}
		hdr := slices.Clone(pkt[:64])
		binenc.LittleEndian.PutUint64(hdr[8:16], uint64(64+len(newBody)))
		sum := md5.Sum(append(slices.Clone(hdr[32:64]), newBody...))
		copy(hdr[16:32], sum[:])
		out = append(append(out, hdr...), newBody...)
	}
	return out
}

// findNamed lists the paths below root whose base name contains substr.
func findNamed(t testing.TB, root, substr string) []string {
	t.Helper()
	var out []string
	for p := range readTree(t, root) {
		if strings.Contains(filepath.Base(p), substr) {
			out = append(out, p)
		}
	}
	return out
}
