package daemon_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
)

func skipHeavy(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("slow end-to-end test")
	}
}

func TestE2ECleanNZBReachesReadyInTheCategoryDirWithoutFetchingRecoveryVolumes(t *testing.T) {
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	g := loadGolden(t, "explicit_uniform")
	files := g.files()
	files["release.nfo"] = payload(1500, 9)
	fx := nntptest.BuildFixture(t, files, 8192)
	fx.Install(srv.Server)
	tv := e.addCategory("tv")

	added := e.addNZB("My.Show.S01E01", fx.NZB, tv)
	got := e.waitState(added.ID, core.StateReady)

	want := filepath.Join(e.complete, "tv", "My.Show.S01E01")
	assert.Equal(t, want, got.OutputPath)
	tree := readTree(t, want)
	delete(tree, ".bitnews-done") // the finisher's restart marker
	assert.Equal(t, map[string][]byte{
		"u1.bin": g.Data["u1.bin"], "u2.bin": g.Data["u2.bin"], "release.nfo": files["release.nfo"],
	}, tree, "byte-exact data; the PAR2 files and the NZB are cleaned up")
	assert.NoDirExists(t, filepath.Join(e.incomplete, string(added.ID)), "the working dir moved")
	for name := range g.Par2 {
		if strings.Contains(name, ".vol") {
			assert.Zero(t, requestsOf(srv, fixtureIDs(t, fx, name)), "%s: a clean download fetches no recovery volume", name)
		}
	}
	assert.Equal(t, encodedBytes(fx, "u1.bin", "u2.bin", "release.nfo", "set.par2"), got.TotalBytes,
		"the total excludes the deferred volumes")

	evs := e.sse.names(added.ID)
	assert.True(t, subsequence(evs, []string{
		"download_added", "download_updated", "fetched", "download_updated",
		"postprocess_stage:verifying", "postprocess_stage:finishing", "download_updated", "ready",
	}), "events seen: %v", evs)
	assert.Equal(t, "ready", evs[len(evs)-1])
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func TestE2EDamagedReleaseIsRepairedWithDeferredRecoveryVolumes(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	g := loadGolden(t, "explicit_uniform")
	// One Article per PAR2 slice, so a missing Article damages one slice.
	fx := nntptest.BuildFixture(t, g.files(), g.Manifest.SliceSize)
	fx.Install(srv.Server)
	u1 := fixtureIDs(t, fx, "u1.bin")
	srv.Missing(u1[2], u1[6])

	added := e.addNZB("Damaged.Release", fx.NZB, "")
	got := e.waitState(added.ID, core.StateReady)

	tree := readTree(t, got.OutputPath)
	for _, f := range g.Manifest.Files {
		assert.Equal(t, f.MD5, md5hex(tree[f.Name]), "%s must be byte-exact after the repair", f.Name)
	}
	// Two damaged slices plus the margin of one: a single volume of five.
	fetched := 0
	for name := range g.Par2 {
		if !strings.Contains(name, ".vol") {
			continue
		}
		ids := fixtureIDs(t, fx, name)
		n := requestsOf(srv, ids)
		if n > 0 {
			fetched++
			assert.Equal(t, len(ids), n, "%s: every Article of a fetched volume exactly once", name)
		}
	}
	assert.Equal(t, 1, fetched, "only the volumes needed are fetched")
	for _, id := range append(u1[:2:2], u1[3:6]...) {
		assert.Equal(t, 1, srv.Requests(id), "data Articles are not fetched again after the hand-off")
	}
	evs := e.sse.names(added.ID)
	assert.True(t, subsequence(evs, []string{
		"fetched", "postprocess_stage:verifying", "postprocess_stage:repairing", "download_updated",
		"fetched", "postprocess_stage:verifying", "postprocess_stage:repairing", "postprocess_stage:finishing", "ready",
	}), "events seen: %v", evs)
	assert.NoDirExists(t, filepath.Join(e.incomplete, string(added.ID)))
}

func TestE2EDamageBeyondRecoveryCapacityFailsWithoutFetchingVolumes(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	g := loadGolden(t, "explicit_uniform")
	fx := nntptest.BuildFixture(t, g.files(), g.Manifest.SliceSize)
	fx.Install(srv.Server)
	u1 := fixtureIDs(t, fx, "u1.bin")
	srv.Missing(u1[3:25]...) // 22 slices, the volumes hold 20

	added := e.addNZB("Hopeless.Release", fx.NZB, "")
	got := e.waitState(added.ID, core.StateFailed)

	assert.Equal(t, "repair_insufficient", got.FailureReason)
	assert.NotEmpty(t, got.FailureDetail)
	assert.NotContains(t, got.FailureDetail, e.root, "no host paths in the reason")
	for name := range g.Par2 {
		if strings.Contains(name, ".vol") {
			assert.Zero(t, requestsOf(srv, fixtureIDs(t, fx, name)), "%s: nothing is fetched for a lost cause", name)
		}
	}
	evs := e.sse.names(added.ID)
	assert.Equal(t, "failed", evs[len(evs)-1], "events seen: %v", evs)
}

func TestE2EArchiveReleaseIsUnpackedAndTheArchiveRemoved(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	archive, err := os.ReadFile(filepath.Join("..", "postproc", "unpack", "testdata", "7z", "t0.7z"))
	require.NoError(t, err)
	fx := nntptest.BuildFixture(t, map[string][]byte{"release.7z": archive, "release.nfo": []byte("hello\n")}, 64)
	fx.Install(srv.Server)

	added := e.addNZB("Archived.Release", fx.NZB, "")
	got := e.waitState(added.ID, core.StateReady)

	tree := readTree(t, got.OutputPath)
	delete(tree, ".bitnews-done")
	assert.Equal(t, map[string][]byte{"foo": []byte("foo\n"), "bar": []byte("bar\n"), "release.nfo": []byte("hello\n")}, tree,
		"the extracted content and the .nfo; the .7z is gone")
	evs := e.sse.names(added.ID)
	assert.True(t, subsequence(evs, []string{"postprocess_stage:verifying", "postprocess_stage:unpacking", "postprocess_stage:finishing", "ready"}),
		"events seen: %v", evs)
}

func TestE2EDeleteWithDataRemovesFilesAndWithoutLeavesThem(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	fxKeep := nntptest.BuildFixture(t, map[string][]byte{"keep.bin": payload(30_000, 1)}, 10_000)
	fxGone := nntptest.BuildFixture(t, map[string][]byte{"gone.bin": payload(30_000, 2)}, 10_000)
	fxKeep.Install(srv.Server)
	fxGone.Install(srv.Server)
	keep := e.waitState(e.addNZB("Keep.Release", fxKeep.NZB, "").ID, core.StateReady)
	gone := e.waitState(e.addNZB("Gone.Release", fxGone.NZB, "").ID, core.StateReady)
	require.DirExists(t, keep.OutputPath)
	require.DirExists(t, gone.OutputPath)

	e.call("DELETE", "/api/v1/downloads/"+string(keep.ID), nil, http.StatusNoContent, nil)
	e.call("DELETE", "/api/v1/downloads/"+string(gone.ID)+"?delete_data=true", nil, http.StatusNoContent, nil)
	e.call("DELETE", "/api/v1/downloads/"+string(gone.ID)+"?delete_data=true", nil, http.StatusNotFound, nil)

	assert.FileExists(t, filepath.Join(keep.OutputPath, "keep.bin"), "without delete_data the files stay")
	assert.NoDirExists(t, gone.OutputPath)
	assert.DirExists(t, filepath.Dir(gone.OutputPath), "the complete root stays")
	assert.Contains(t, e.sse.names(gone.ID), "removed")
}

func TestE2EDeleteWithDataMidFetchStopsTheFetchAndRemovesTheWorkingDir(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	srv.SetLatency(5 * time.Millisecond)
	fx := nntptest.BuildFixture(t, map[string][]byte{"big.bin": payload(600_000, 3)}, 5_000) // 120 Articles
	fx.Install(srv.Server)
	added := e.addNZB("Big.Release", fx.NZB, "")
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 10 }, 10*time.Second, 5*time.Millisecond)
	working := filepath.Join(e.incomplete, string(added.ID))
	require.DirExists(t, working)

	e.call("DELETE", "/api/v1/downloads/"+string(added.ID)+"?delete_data=true", nil, http.StatusNoContent, nil)

	assert.NoDirExists(t, working)
	atDelete := srv.TotalRequests()
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, atDelete, srv.TotalRequests(), "no request after the delete")
	assert.NoDirExists(t, working, "nothing recreated the directory")
	e.call("GET", "/api/v1/downloads/"+string(added.ID), nil, http.StatusNotFound, nil)
}

func TestE2ERestartResumesAndRefetchesOnlyWhatWasNotSaved(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	srv.SetLatency(3 * time.Millisecond)
	data := payload(500_000, 4)
	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": data}, 5_000) // 100 Articles
	fx.Install(srv.Server)
	added := e.addNZB("Resume.Release", fx.NZB, "")
	progress := store.NewUsenetProgressRepo(e.db)
	require.Eventually(t, func() bool {
		_, err := progress.Load(context.Background(), added.ID)
		return err == nil && srv.TotalRequests() >= 35
	}, 15*time.Second, 2*time.Millisecond, "a periodic save after some Articles")

	e.stop() // the daemon dies mid-fetch
	firstRun := srv.TotalRequests()
	require.Less(t, firstRun, 100, "the first run must not have finished")
	stored, err := store.NewDownloadRepo(e.db).Get(context.Background(), added.ID)
	require.NoError(t, err)
	require.NotEqual(t, core.StateReady, stored.State)

	e.start() // a new daemon on the same database and directories
	got := e.waitState(added.ID, core.StateReady)

	assert.Equal(t, data, readTree(t, got.OutputPath)["movie.mkv"])
	refetched := 0
	for _, id := range fx.MessageIDs() {
		assert.Positive(t, srv.Requests(id))
		if srv.Requests(id) > 1 {
			refetched++
		}
	}
	assert.Less(t, refetched, firstRun-10, "most Articles of the first run were not fetched again (%d refetched, first run %d)", refetched, firstRun)
	assert.Less(t, srv.TotalRequests(), 100+firstRun-10, "fewer requests than a fetch from scratch would take")
}

func TestE2ELoginRejectedByEveryServerStaysQueuedAndRecoversWithTheRightPassword(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	srv.setPass("rotated") // the provider changed the password
	data := payload(60_000, 5)
	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": data}, 10_000)
	fx.Install(srv.Server)
	added := e.addNZB("Locked.Out", fx.NZB, "")

	// Several retries after the 150 ms failure backoff: the Download is
	// Queued while it waits and Fetching for the moment of each retry, but
	// never Fetched, Failed or Ready.
	seenQueued := false
	for end := time.Now().Add(600 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		st := e.get(added.ID).State
		require.Contains(t, []core.State{core.StateQueued, core.StateFetching}, st, "visible and waiting, not Fetched with everything missing")
		seenQueued = seenQueued || st == core.StateQueued
	}
	assert.True(t, seenQueued)
	assert.Zero(t, srv.TotalRequests())

	e.call("PATCH", "/api/v1/servers/"+string(e.ids["main"]), map[string]any{"password": "rotated"}, http.StatusOK, nil)
	done := e.waitState(added.ID, core.StateReady)

	assert.Equal(t, data, readTree(t, done.OutputPath)["movie.mkv"])
}

// encodedBytes is the size on the wire of the named files of fx, which is
// what an NZB's segment sizes (and so a Download's TotalBytes) count.
func encodedBytes(fx nntptest.Fixture, names ...string) int64 {
	var n int64
	for _, name := range names {
		for _, f := range fx.Files {
			if f.Name != name {
				continue
			}
			for _, id := range f.MessageIDs {
				for _, l := range fx.Articles[id] {
					n += int64(len(l)) + 2
				}
			}
		}
	}
	return n
}
