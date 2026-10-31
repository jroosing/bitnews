package blackbox_test

import (
	"net/http"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// Post-processing over time: how many Downloads at once, a daemon that stops
// in the middle of it, and what the API allows meanwhile. The tests hold a
// Download inside the verifying stage with a par2 shim (see toolShim), so
// they never depend on how long a stage takes.

// withPar2Set is a small release with a clean PAR2 set, so that par2 runs.
func cleanPar2Release(t testing.TB, seed uint64) (release, []byte) {
	t.Helper()
	data := blocks(seed, 3)
	set := makePar2(t, "rel", []relSpec{{Name: "a.bin", Data: data}}, blockSize, 2, 1)
	return newRelease(t, append([]relSpec{{Name: "a.bin", Data: data}}, set.all()...)), data
}

func TestPostProcConcurrencyOneProcessesOneDownloadAtATime(t *testing.T) {
	t.Parallel()
	shim := newToolShim(t, "par2")
	r := newPPRig(t, ppStart(shim.onPath()), ppTune(func(s *core.Settings) { s.Queues.Usenet = 2 }))
	relA, dataA := cleanPar2Release(t, 60)
	relB, dataB := cleanPar2Release(t, 61)
	r.serve(relA)
	r.serve(relB)

	idA := r.add(relA, "First")
	idB := r.add(relB, "Second")
	shim.waitStarted(1)
	eventually(t, longWait, "both downloads to be fetched", func() bool {
		states := []string{r.d.stateOf(idA), r.d.stateOf(idB)}
		return states[0] != "queued" && states[0] != "fetching" && states[1] != "queued" && states[1] != "fetching"
	})

	// One is held in post-processing; the other waits fetched. It does not
	// start, however long it has.
	ok := holds(1500*time.Millisecond, func() bool {
		a, b := r.d.stateOf(idA), r.d.stateOf(idB)
		return len(shim.started()) == 1 && ((a == "post_processing" && b == "fetched") || (a == "fetched" && b == "post_processing"))
	})
	assert.True(t, ok, "with postproc_concurrency 1 the second download waits: %s and %s", r.d.stateOf(idA), r.d.stateOf(idB))

	shim.open()
	outA, outB := r.ready(idA), r.ready(idB)
	requireOutput(t, outA, idA, tree{"a.bin": ppFile(dataA)})
	requireOutput(t, outB, idB, tree{"a.bin": ppFile(dataB)})
	assert.Len(t, shim.started(), 2)
}

func TestPostProcConcurrencyTwoProcessesTwoDownloadsAtOnce(t *testing.T) {
	t.Parallel()
	shim := newToolShim(t, "par2")
	r := newPPRig(t, ppStart(shim.onPath()), ppTune(func(s *core.Settings) {
		s.Queues.Usenet = 2
		s.Queues.PostprocConcurrency = 2
	}))
	relA, dataA := cleanPar2Release(t, 62)
	relB, dataB := cleanPar2Release(t, 63)
	r.serve(relA)
	r.serve(relB)

	idA := r.add(relA, "First")
	idB := r.add(relB, "Second")

	shim.waitStarted(2)
	assert.Equal(t, "post_processing", r.d.stateOf(idA))
	assert.Equal(t, "post_processing", r.d.stateOf(idB))
	shim.open()
	requireOutput(t, r.ready(idA), idA, tree{"a.bin": ppFile(dataA)})
	requireOutput(t, r.ready(idB), idB, tree{"a.bin": ppFile(dataB)})
}

func TestPostProcADaemonStoppedInTheMiddleProcessesTheDownloadAgainAndEndsReadyOnce(t *testing.T) {
	t.Parallel()
	shim := newToolShim(t, "par2")
	r := newPPRig(t, ppStart(shim.onPath()))
	rel, data := cleanPar2Release(t, 64)
	r.serve(rel)
	id := r.add(rel, "Interrupted")
	shim.waitStarted(1)
	r.waitState(id, "post_processing")

	r.d.stop() // SIGTERM: the tool is killed, the exit is clean
	require.Equal(t, core.StatePostProcessing, mustStored(t, r, id).State, "post-processing progress is not stored: the Download is where it was")
	r.startDaemon()
	// The new daemon takes the Download up again by itself; the shim holds it
	// until the event log is open, so no event is missed.
	shim.waitStarted(2)
	shim.open()

	out := r.ready(id)
	assert.Equal(t, filepath.Join(r.roots.complete, "Interrupted"), out)
	requireOutput(t, out, id, tree{"a.bin": ppFile(data)})
	assert.Equal(t, []string{"Interrupted"}, dirNames(t, r.roots.complete), "exactly one output")
	assert.Len(t, shim.started(), 2, "the stage started again from the beginning")
	tr := r.ev.traceUntil(id, "ready")
	assert.Equal(t, 1, r.ev.count("ready", id), "ready once")
	// A resumed Download is not announced as post_processing again (it still
	// is), and its first stage event may be sent before the log is open (the
	// trace starts with it or with finishing).
	require.GreaterOrEqual(t, len(tr), 3)
	assert.Equal(t, []string{"stage:finishing", "state:ready", "ready"}, tr[len(tr)-3:])
	assert.NotContains(t, tr, "fetched", "it is not fetched again")
	assert.Equal(t, core.StateReady, mustStored(t, r, id).State)
}

func TestPostProcACrashedDaemonLeavesTheDownloadToBeProcessedAgain(t *testing.T) {
	t.Parallel()
	shim := newToolShim(t, "par2")
	r := newPPRig(t, ppStart(shim.onPath()))
	rel, data := cleanPar2Release(t, 65)
	r.serve(rel)
	id := r.add(rel, "Crashed")
	shim.waitStarted(1)
	r.waitState(id, "post_processing")

	// SIGKILL gives the daemon no chance to stop its tool: do it here.
	require.NoError(t, r.d.cmd.Process.Kill())
	<-r.d.done
	for _, p := range shim.started() {
		pid, err := strconv.Atoi(p)
		require.NoError(t, err)
		_ = syscall.Kill(-pid, syscall.SIGKILL) // the tool's process group
	}
	r.startDaemon()
	shim.waitStarted(2)
	shim.open()

	out := r.ready(id)
	requireOutput(t, out, id, tree{"a.bin": ppFile(data)})
	assert.Equal(t, []string{"Crashed"}, dirNames(t, r.roots.complete))
	r.ev.traceUntil(id, "ready")
	assert.Equal(t, 1, r.ev.count("ready", id))
}

func mustStored(t *testing.T, r *ppRig, id string) core.Download {
	t.Helper()
	d, err := storedDownload(t, r.cfg, core.ID(id))
	require.NoError(t, err)
	return d
}

func TestPostProcADownloadBeingPostProcessedCannotBeRemoved(t *testing.T) {
	t.Parallel()
	shim := newToolShim(t, "par2")
	r := newPPRig(t, ppStart(shim.onPath()))
	rel, data := cleanPar2Release(t, 66)
	r.serve(rel)
	id := r.add(rel, "Busy")
	shim.waitStarted(1)
	r.waitState(id, "post_processing")
	const msg = "This download is being post-processed (repaired or unpacked) and its files are in use. Remove it once it is ready or has failed."

	plain := r.d.api(http.MethodDelete, "/api/v1/downloads/"+id, "")
	withData := r.d.api(http.MethodDelete, "/api/v1/downloads/"+id+"?delete_data=true", "")

	requireErrorMessage(t, plain, http.StatusConflict, "download_busy", msg)
	requireErrorMessage(t, withData, http.StatusConflict, "download_busy", msg)
	assert.FileExists(t, filepath.Join(r.work(id), "a.bin"), "nothing was touched")
	assert.Equal(t, "post_processing", r.d.stateOf(id))
	shim.open()
	out := r.ready(id)
	requireOutput(t, out, id, tree{"a.bin": ppFile(data)})
	assert.Equal(t, 0, r.ev.count("removed", id))
}

func TestPostProcRemovingAReadyDownloadWithItsDataDeletesTheOutput(t *testing.T) {
	t.Parallel()
	var cat core.ID
	r := newPPRig(t, ppBefore(func(r *ppRig) { cat = addCategoryDirs(t, r.cfg, "series", "", "") }))
	data := blocks(67, 3)
	rel := r.serve(newRelease(t, []relSpec{{Name: "a.bin", Data: data}}))
	id := r.add(rel, "Gone.Soon", "category_id", string(cat))
	out := r.ready(id)
	require.Equal(t, filepath.Join(r.roots.complete, "series", "Gone.Soon"), out)
	keep := r.serve(newRelease(t, []relSpec{{Name: "b.bin", Data: blocks(68, 2)}}))
	idKeep := r.add(keep, "Keep.Me", "category_id", string(cat))
	outKeep := r.ready(idKeep)

	// Without delete_data the Download goes and its output stays.
	requireNoContent(t, r.d.api(http.MethodDelete, "/api/v1/downloads/"+idKeep, ""))
	assert.DirExists(t, outKeep)
	// With it, the output goes, and only the output.
	requireNoContent(t, r.d.api(http.MethodDelete, "/api/v1/downloads/"+id+"?delete_data=true", ""))

	assert.NoDirExists(t, out)
	assert.DirExists(t, filepath.Join(r.roots.complete, "series"), "the Category folder stays")
	assert.DirExists(t, r.roots.complete)
	assert.NoDirExists(t, r.work(id))
	assert.Equal(t, http.StatusNotFound, r.d.apiGet("/api/v1/downloads/"+id).status)
	eventually(t, longWait, "the removed event", func() bool { return r.ev.count("removed", id) == 1 })
}
