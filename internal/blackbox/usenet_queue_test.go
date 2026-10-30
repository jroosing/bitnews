package blackbox_test

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// What the Queue does with Downloads that fetch slowly: pause and resume,
// the concurrency limit, priorities, removal, a restart, a Server that cannot
// be used. The fake server holds each article for a while so a Download is
// in the middle of its fetch when the test acts.

const slow = 60 * time.Millisecond

// slowRelease is a release of one file with n articles.
func slowRelease(t testing.TB, name string, seed uint64, n int) release {
	return oneFile(t, name, seed, n)
}

// requestsStop waits until the server has received no BODY command for a
// while and returns the total then.
func requestsStop(t testing.TB, srv *nntpServer) int {
	t.Helper()
	var last int
	eventually(t, longWait, "the server to be left alone", func() bool {
		last = srv.Total()
		return holds(500*time.Millisecond, func() bool { return srv.Total() == last })
	})
	return last
}

func TestUsenetPausingAFetchingDownloadStopsItAndResumingFinishesIt(t *testing.T) {
	t.Parallel()
	rel := slowRelease(t, "pause.bin", 300, 40)
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).SetLatency(slow) })

	id := s.d.addRelease(rel)["id"].(string)
	eventually(t, longWait, "some articles", func() bool { return s.srv.Total() >= 6 })

	r := s.d.api(http.MethodPost, "/api/v1/downloads/"+id+"/pause", "")
	requireOK(t, r)
	assert.Equal(t, true, obj(t, r)["paused"])
	eventually(t, longWait, "the Download to be queued again", func() bool { return s.d.stateOf(id) == "queued" })
	stopped := requestsStop(t, s.srv)
	assert.Less(t, stopped, rel.Segments(), "it was stopped in the middle")
	got := s.d.download(id)
	assert.Equal(t, "queued", got["state"])
	assert.Equal(t, true, got["paused"])
	assert.Positive(t, num(got, "done_bytes"), "what it has fetched is kept")
	assert.Equal(t, rel.Total, num(got, "total_bytes"))
	// The announcements: paused first (download_updated paused=true), then
	// the state back to queued.
	evs := s.ev.of(id)
	iPaused := slices.IndexFunc(evs, func(f frame) bool {
		return f.name == "download_updated" && fjson(t, f)["paused"] == true
	})
	require.GreaterOrEqual(t, iPaused, 0)
	assert.Equal(t, 0, s.ev.count("fetched", id))

	r = s.d.api(http.MethodPost, "/api/v1/downloads/"+id+"/resume", "")
	requireOK(t, r)
	assert.Equal(t, false, obj(t, r)["paused"])
	waitFetched(t, s.ev, id)

	s.u.requireFiles(t, id, rel)
	requireNotFailed(t, settle(s.d, id))
	for _, a := range rel.IDs() {
		assert.LessOrEqual(t, s.srv.Requests(a), 2, "at most an article that was in flight is asked twice: %s", a)
	}
	assert.LessOrEqual(t, s.srv.Total(), rel.Segments()+2, "finished articles are not fetched again (two connections were in flight)")
	states := s.ev.states(id)
	assert.Equal(t, []string{"queued", "fetching", "queued", "fetching", "fetched"}, states[:5])
}

func TestUsenetADownloadAddedPausedWaitsUntilItIsResumed(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "paused.bin", 310, 4)
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	dl := s.d.addRelease(rel, "paused", "true")
	id := dl["id"].(string)
	assert.Equal(t, true, dl["paused"])
	assert.Equal(t, "queued", dl["state"])
	assert.True(t, holds(700*time.Millisecond, func() bool { return s.d.stateOf(id) == "queued" && s.srv.Dials() == 0 }),
		"nothing is fetched and no connection is made")

	requireOK(t, s.d.api(http.MethodPost, "/api/v1/downloads/"+id+"/resume", ""))
	waitFetched(t, s.ev, id)
	s.u.requireFiles(t, id, rel)
}

func TestUsenetOnlyOneDownloadFetchesAtATimeAndTheNextOneStartsWhenItIsDone(t *testing.T) {
	t.Parallel()
	a := slowRelease(t, "first.bin", 320, 40)
	b := slowRelease(t, "second.bin", 321, 6)
	s := startOneServer(t, func(s *nntpServer) { s.Install(a).Install(b).SetLatency(40 * time.Millisecond) })

	ida := s.d.addRelease(a)["id"].(string)
	idb := s.d.addRelease(b)["id"].(string)
	eventually(t, longWait, "the first to fetch", func() bool { return s.d.stateOf(ida) == "fetching" })
	assert.True(t, holds(250*time.Millisecond, func() bool {
		return s.d.stateOf(ida) == "fetching" && s.d.stateOf(idb) == "queued"
	}), "the limit of the Usenet Queue is 1")
	for _, x := range b.IDs() {
		assert.Zero(t, s.srv.Requests(x), "the queued Download is not touched")
	}

	waitFetched(t, s.ev, ida)
	waitFetched(t, s.ev, idb)
	s.u.requireFiles(t, ida, a)
	s.u.requireFiles(t, idb, b)

	// Replay the events: never two Downloads fetching at once, and the
	// second starts after the first has left fetching.
	fetching := map[string]bool{}
	for _, f := range s.ev.all() {
		if f.name != "download_updated" && f.name != "download_added" {
			continue
		}
		m := fjson(t, f)
		fetching[m["id"].(string)] = m["state"] == "fetching"
		n := 0
		for _, v := range fetching {
			if v {
				n++
			}
		}
		assert.LessOrEqual(t, n, 1, "at most one fetching Download at any time")
	}
	assert.Less(t, s.ev.firstIndexAll(func(f frame) bool { return f.name == "fetched" && idOf(t, f) == ida }),
		s.ev.firstIndexAll(stateIsID(t, idb, "fetching")), "the second starts after the first is fetched")
}

func TestUsenetAForcedDownloadFetchesBesideTheRunningOneWithoutCountingAgainstTheLimit(t *testing.T) {
	t.Parallel()
	a := slowRelease(t, "normal.bin", 330, 30)
	b := slowRelease(t, "forced.bin", 331, 8)
	s := startOneServer(t, func(s *nntpServer) { s.Install(a).Install(b).SetLatency(50 * time.Millisecond) })

	ida := s.d.addRelease(a)["id"].(string)
	eventually(t, longWait, "the first to fetch", func() bool { return s.d.stateOf(ida) == "fetching" })
	idb := s.d.addRelease(b, "priority", "force")["id"].(string)

	eventually(t, longWait, "both fetching", func() bool {
		return s.d.stateOf(ida) == "fetching" && s.d.stateOf(idb) == "fetching"
	})
	waitFetched(t, s.ev, ida)
	waitFetched(t, s.ev, idb)
	s.u.requireFiles(t, ida, a)
	s.u.requireFiles(t, idb, b)
	assert.Equal(t, a.Segments()+b.Segments(), s.srv.Total(), "the first Download was not interrupted")
}

func TestUsenetAHigherPriorityDownloadTakesThePlaceOfTheRunningOne(t *testing.T) {
	t.Parallel()
	a := slowRelease(t, "low.bin", 340, 40)
	b := slowRelease(t, "high.bin", 341, 6)
	s := startOneServer(t, func(s *nntpServer) { s.Install(a).Install(b).SetLatency(50 * time.Millisecond) })

	ida := s.d.addRelease(a)["id"].(string)
	eventually(t, longWait, "some of the first", func() bool { return s.d.stateOf(ida) == "fetching" && s.srv.Total() >= 4 })
	idb := s.d.addRelease(b, "priority", "high")["id"].(string)

	eventually(t, longWait, "the high priority one to fetch", func() bool { return s.d.stateOf(idb) == "fetching" })
	assert.Equal(t, "queued", s.d.stateOf(ida), "the running Download was put back in the queue")
	waitFetched(t, s.ev, idb)
	waitFetched(t, s.ev, ida)

	s.u.requireFiles(t, ida, a)
	s.u.requireFiles(t, idb, b)
	assert.Equal(t, []string{"queued", "fetching", "queued", "fetching", "fetched"}, s.ev.states(ida)[:5])
	assert.Less(t, s.ev.firstIndexAll(func(f frame) bool { return f.name == "fetched" && idOf(t, f) == idb }),
		s.ev.firstIndexAll(func(f frame) bool { return f.name == "fetched" && idOf(t, f) == ida }))
	assert.LessOrEqual(t, s.srv.Total(), a.Segments()+b.Segments()+2, "the pre-empted Download continued where it was")
}

func TestUsenetRaisingThePriorityOfAQueuedDownloadToForceStartsIt(t *testing.T) {
	t.Parallel()
	a := slowRelease(t, "running.bin", 350, 30)
	b := slowRelease(t, "waiting.bin", 351, 5)
	s := startOneServer(t, func(s *nntpServer) { s.Install(a).Install(b).SetLatency(50 * time.Millisecond) })

	ida := s.d.addRelease(a)["id"].(string)
	idb := s.d.addRelease(b)["id"].(string)
	eventually(t, longWait, "the first to fetch", func() bool { return s.d.stateOf(ida) == "fetching" })
	require.Equal(t, "queued", s.d.stateOf(idb))

	requireOK(t, s.d.api(http.MethodPatch, "/api/v1/downloads/"+idb, `{"priority":"force"}`))

	eventually(t, longWait, "the forced one to fetch", func() bool { return s.d.stateOf(idb) == "fetching" || s.ev.count("fetched", idb) > 0 })
	assert.NotEqual(t, "queued", s.d.stateOf(ida), "the first one kept its place")
	waitFetched(t, s.ev, idb)
	waitFetched(t, s.ev, ida)
}

func TestUsenetRaisingTheQueueLimitStartsTheWaitingDownloads(t *testing.T) {
	t.Parallel()
	a := slowRelease(t, "one.bin", 360, 30)
	b := slowRelease(t, "two.bin", 361, 10)
	s := startOneServer(t, func(s *nntpServer) { s.Install(a).Install(b).SetLatency(50 * time.Millisecond) })

	ida := s.d.addRelease(a)["id"].(string)
	idb := s.d.addRelease(b)["id"].(string)
	eventually(t, longWait, "the first to fetch", func() bool { return s.d.stateOf(ida) == "fetching" })
	require.Equal(t, "queued", s.d.stateOf(idb))

	var settings core.Settings
	require.NoError(t, jsonUnmarshal(s.d.apiGet("/api/v1/settings").body, &settings))
	settings.Queues.Usenet = 2
	r := s.d.api(http.MethodPut, "/api/v1/settings", jsonOf(t, settings))
	requireOK(t, r)

	eventually(t, longWait, "both to fetch", func() bool {
		return s.d.stateOf(ida) == "fetching" && (s.d.stateOf(idb) == "fetching" || s.ev.count("fetched", idb) > 0)
	})
	waitFetched(t, s.ev, ida)
	waitFetched(t, s.ev, idb)
}

func TestUsenetAQueueLimitOfZeroStartsNothingButForcedDownloads(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	a := slowRelease(t, "normal.bin", 370, 4)
	b := slowRelease(t, "forced.bin", 371, 4)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(a).Install(b)
	u := newUsenetSetup(t, ca, []serverSpec{specFor(srv)}, func(s *core.Settings) { s.Queues.Usenet = 0 })
	d := u.start()
	ev := d.watch()

	ida := d.addRelease(a)["id"].(string)
	idb := d.addRelease(b, "priority", "force")["id"].(string)

	waitFetched(t, ev, idb)
	assert.True(t, holds(500*time.Millisecond, func() bool { return d.stateOf(ida) == "queued" }), "a limit of 0 starts no normal Download")
	for _, x := range a.IDs() {
		assert.Zero(t, srv.Requests(x))
	}
}

func TestUsenetRemovingAFetchingDownloadStopsItAndKeepsItsFiles(t *testing.T) {
	t.Parallel()
	rel := slowRelease(t, "remove.bin", 380, 40)
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).SetLatency(slow) })

	id := s.d.addRelease(rel)["id"].(string)
	eventually(t, longWait, "some articles", func() bool { return s.srv.Total() >= 6 && s.d.stateOf(id) == "fetching" })

	requireNoContent(t, s.d.api(http.MethodDelete, "/api/v1/downloads/"+id, ""))

	assert.Equal(t, http.StatusNotFound, s.d.apiGet("/api/v1/downloads/"+id).status)
	assert.Equal(t, 1, s.ev.count("removed", id))
	stopped := requestsStop(t, s.srv)
	assert.Less(t, stopped, rel.Segments(), "the fetch stopped")
	assert.DirExists(t, s.u.workDir(id), "without delete_data the files stay")
	assert.Contains(t, s.u.workFiles(id), "remove.bin")
	assert.Equal(t, 0, s.ev.count("fetched", id))
}

func TestUsenetRemovingAFetchingDownloadWithItsDataDeletesTheWorkDir(t *testing.T) {
	t.Parallel()
	rel := slowRelease(t, "wipe.bin", 390, 40)
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).SetLatency(slow) })

	id := s.d.addRelease(rel)["id"].(string)
	eventually(t, longWait, "some articles", func() bool { return s.srv.Total() >= 6 && s.d.stateOf(id) == "fetching" })

	requireNoContent(t, s.d.api(http.MethodDelete, "/api/v1/downloads/"+id+"?delete_data=true", ""))

	assert.Equal(t, http.StatusNotFound, s.d.apiGet("/api/v1/downloads/"+id).status)
	stopped := requestsStop(t, s.srv)
	assert.Less(t, stopped, rel.Segments())
	assert.NoDirExists(t, s.u.workDir(id))
	assert.DirExists(t, s.u.roots.usenetIncomplete, "never the root")
	// Whatever was still running did not bring the directory back.
	assert.True(t, holds(500*time.Millisecond, func() bool {
		_, err := os.Stat(s.u.workDir(id))
		return os.IsNotExist(err)
	}))
}

func TestUsenetARestartInTheMiddleOfAFetchResumesWithoutFetchingFinishedArticlesAgain(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := slowRelease(t, "restart.bin", 400, 40)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel).SetLatency(slow)
	u := newUsenetSetup(t, ca, []serverSpec{specFor(srv, specConns(2))})
	d := u.start()
	id := d.addRelease(rel)["id"].(string)
	eventually(t, longWait, "a third of the articles", func() bool { return srv.Total() >= 14 })
	before := srv.Total()

	d.stop() // SIGTERM, a clean exit

	afterStop := srv.Total()
	assert.Less(t, afterStop, rel.Segments(), "it was stopped in the middle")
	assert.GreaterOrEqual(t, afterStop, before)
	stored := storedDownloads(t, u.cfg)
	require.Len(t, stored, 1)
	assert.Contains(t, []core.State{core.StateQueued, core.StateFetching}, stored[0].State, "interrupted, not failed")
	assert.NotEqual(t, core.StateFailed, stored[0].State)

	d2 := u.start()
	waitFetchedState(t, d2, id)

	u.requireFiles(t, id, rel)
	requireNotFailed(t, settle(d2, id))
	for _, a := range rel.IDs() {
		assert.LessOrEqual(t, srv.Requests(a), 2, a)
	}
	assert.LessOrEqual(t, srv.Total(), rel.Segments()+2, "only the articles in flight at the signal are fetched twice (two connections)")
	final := d2.download(id)
	assert.Equal(t, rel.Decoded(), num(final, "done_bytes"))
}

func TestUsenetARestartKeepsAQueuedDownloadQueuedAndFetchesItAfterwards(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := slowRelease(t, "late.bin", 410, 5)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel)
	// No Server at first: the Download is added and waits.
	u := newUsenetSetup(t, ca, nil)
	d := u.start()
	id := d.addRelease(rel)["id"].(string)
	assert.True(t, holds(500*time.Millisecond, func() bool { return d.stateOf(id) == "queued" }), "nothing can fetch without a Server")
	d.stop()
	assert.Equal(t, 0, srv.Dials())

	// Add the Server to the stored config and start again.
	addServerTo(t, u, specFor(srv))
	d2 := u.start()
	waitFetchedState(t, d2, id)
	u.requireFiles(t, id, rel)
}

func TestUsenetAServerRejectingThePasswordLeavesTheDownloadWaitingInTheQueue(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := slowRelease(t, "noauth.bin", 420, 5)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, "the-real-password").Install(rel)
	u := newUsenetSetup(t, ca, []serverSpec{specFor(srv, specPassword("wrong-password-9"))})
	d := u.start()
	ev := d.watch()

	id := d.addRelease(rel)["id"].(string)
	eventually(t, longWait, "the first attempt", func() bool { return srv.Auths() >= 1 })
	// The fetch started, found no usable Server and put the Download back:
	// it waits (30 s in the Go daemon) instead of failing or being marked
	// fetched with everything missing.
	eventually(t, longWait, "fetching, then queued again", func() bool {
		st := ev.states(id)
		return len(st) >= 3 && st[len(st)-1] == "queued"
	})
	assert.Equal(t, []string{"queued", "fetching", "queued"}, ev.states(id))
	assert.True(t, holds(1200*time.Millisecond, func() bool {
		st := d.stateOf(id)
		return st == "queued" || st == "fetching"
	}), "neither failed nor fetched")
	assert.Zero(t, srv.Total(), "no article was asked for")
	assert.Equal(t, 0, ev.count("fetched", id))
	assert.Equal(t, 0, ev.count("failed", id))
	got := d.download(id)
	assert.Zero(t, num(got, "done_bytes"))
	assert.Empty(t, got["failure_reason"])
	for _, f := range ev.all() {
		assert.NotContains(t, f.data, "wrong-password-9")
	}
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "wrong-password-9", "the password is not logged")
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "the-real-password")
	assert.False(t, strings.Contains(strings.Join(srv.Commands(), "\n"), "BODY"), "no BODY command was sent")
}
