package runner_test

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
)

func TestFetchesAnNZBToFetchedWithByteExactFiles(t *testing.T) {
	h := newHarness(t)
	files := map[string][]byte{"movie.mkv": payload(300_000, 1), "movie.nfo": payload(5_000, 2)}
	fx := nntptest.BuildFixture(t, files, 50_000)
	fx.Install(h.addServer("main", 0, 4))
	rec := h.record()
	d := h.addDownload("movie", fx, func(d *core.Download) { d.TotalBytes = 400_000 })
	h.start()

	got := h.waitState(d.ID, core.StateFetched)

	assert.False(t, got.FetchedAt.IsZero())
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
	assert.Equal(t, int64(305_000), got.DoneBytes)
	var kinds []core.EventKind
	for _, ev := range rec.all() {
		if ev.Kind() != core.KindDownloadProgress && ev.Kind() != core.KindDownloadAdded {
			kinds = append(kinds, ev.Kind())
		}
	}
	require.Equal(t, []core.EventKind{core.KindDownloadUpdated, core.KindDownloadUpdated, core.KindFetched}, kinds)
}

// indexOf returns the position of the first event matching f, or -1.
func indexOf(evs []core.Event, f func(core.Event) bool) int {
	for i, ev := range evs {
		if f(ev) {
			return i
		}
	}
	return -1
}

func fetchedEvent(id core.ID) func(core.Event) bool {
	return func(ev core.Event) bool { f, ok := ev.(core.Fetched); return ok && f.ID == id }
}

func fetchingUpdate(id core.ID) func(core.Event) bool {
	return func(ev core.Event) bool {
		u, ok := ev.(core.DownloadUpdated)
		return ok && u.Download.ID == id && u.Download.State == core.StateFetching
	}
}

func TestWithLimitOneTwoDownloadsRunOneAfterTheOther(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	fa := nntptest.BuildFixture(t, map[string][]byte{"a.bin": payload(60_000, 1)}, 10_000)
	fb := nntptest.BuildFixture(t, map[string][]byte{"b.bin": payload(60_000, 2)}, 10_000)
	fa.Install(srv)
	fb.Install(srv)
	rec := h.record()
	a := h.addDownload("a", fa)
	b := h.addDownload("b", fb)
	h.start()

	h.waitState(a.ID, core.StateFetched)
	h.waitState(b.ID, core.StateFetched)

	evs := rec.all()
	aDone, bStart := indexOf(evs, fetchedEvent(a.ID)), indexOf(evs, fetchingUpdate(b.ID))
	require.NotEqual(t, -1, aDone)
	require.NotEqual(t, -1, bStart)
	assert.Less(t, aDone, bStart, "b must start only after a is Fetched")
}

func TestRaisingTheLimitAtRuntimeActivatesTheSecondDownload(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(10 * time.Millisecond)
	fa := nntptest.BuildFixture(t, map[string][]byte{"a.bin": payload(400_000, 1)}, 10_000)
	fb := nntptest.BuildFixture(t, map[string][]byte{"b.bin": payload(10_000, 2)}, 10_000)
	fa.Install(srv)
	fb.Install(srv)
	a := h.addDownload("a", fa)
	b := h.addDownload("b", fb)
	h.start()
	h.waitState(a.ID, core.StateFetching)
	require.Equal(t, core.StateQueued, h.get(b.ID).State)

	h.setSettings(func(s *core.Settings) { s.Queues.Usenet = 2 })

	h.waitState(b.ID, core.StateFetching)
	assert.Equal(t, core.StateFetching, h.get(a.ID).State, "a keeps running")
}

func TestHigherPriorityDownloadRunsFirst(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	fa := nntptest.BuildFixture(t, map[string][]byte{"a.bin": payload(30_000, 1)}, 10_000)
	fb := nntptest.BuildFixture(t, map[string][]byte{"b.bin": payload(30_000, 2)}, 10_000)
	fa.Install(srv)
	fb.Install(srv)
	rec := h.record()
	low := h.addDownload("low", fa, func(d *core.Download) { d.Priority = core.DownloadPriorityLow })
	high := h.addDownload("high", fb, func(d *core.Download) { d.Priority = core.DownloadPriorityHigh })
	h.start()

	h.waitState(low.ID, core.StateFetched)
	h.waitState(high.ID, core.StateFetched)

	evs := rec.all()
	assert.Less(t, indexOf(evs, fetchedEvent(high.ID)), indexOf(evs, fetchingUpdate(low.ID)))
}

func TestUnparseableNZBFailsWithSourceInvalid(t *testing.T) {
	h := newHarness(t)
	h.addServer("main", 0, 1)
	rec := h.record()
	d := h.addDownload("junk", nntptest.Fixture{NZB: []byte("this is not xml")})
	h.start()

	got := h.waitState(d.ID, core.StateFailed)

	assert.Equal(t, core.FailureSourceInvalid, got.FailureReason)
	require.Eventually(t, func() bool {
		return indexOf(rec.all(), func(ev core.Event) bool {
			f, ok := ev.(core.Failed)
			return ok && f.ID == d.ID && f.Reason == core.FailureSourceInvalid
		}) >= 0
	}, 5*time.Second, 5*time.Millisecond)
}

func TestNZBWithOnlyRecoveryVolumesFailsWithSourceInvalid(t *testing.T) {
	h := newHarness(t)
	h.addServer("main", 0, 1)
	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.vol000+02.par2": payload(5_000, 1)}, 10_000)
	d := h.addDownload("only-par2", fx)
	h.start()

	got := h.waitState(d.ID, core.StateFailed)

	assert.Equal(t, core.FailureSourceInvalid, got.FailureReason)
}

func TestRecoveryVolumesAreDeferredButTheIndexPar2IsFetched(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	files := map[string][]byte{
		"movie.mkv":            payload(40_000, 1),
		"movie.par2":           payload(3_000, 2),
		"movie.vol000+02.par2": payload(20_000, 3),
		"movie.vol002+04.par2": payload(30_000, 4),
	}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	h.start()

	h.waitState(d.ID, core.StateFetched)

	for _, f := range fx.Files {
		for _, id := range f.MessageIDs {
			if f.Name == "movie.mkv" || f.Name == "movie.par2" {
				assert.Equal(t, 1, srv.Requests(id), "%s must be fetched", f.Name)
			} else {
				assert.Zero(t, srv.Requests(id), "%s must be deferred", f.Name)
			}
		}
	}
	got := readDir(t, h.dirFor(d))
	assert.Equal(t, map[string][]byte{"movie.mkv": files["movie.mkv"], "movie.par2": files["movie.par2"]}, got)
}

func TestMissingArticleOfMainServerIsFilledByTheFillServer(t *testing.T) {
	h := newHarness(t)
	main := h.addServer("main", 0, 2)
	fill := h.addServer("fill", 1, 2)
	files := map[string][]byte{"movie.mkv": payload(200_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000) // 20 articles
	fx.Install(main)
	fx.Install(fill)
	lost := fx.Files[0].MessageIDs[7]
	main.Missing(lost)
	d := h.addDownload("movie", fx)
	h.start()

	h.waitState(d.ID, core.StateFetched)

	assert.Equal(t, files, readDir(t, h.dirFor(d)))
	rep, ok := h.runner.Report(d.ID)
	require.True(t, ok)
	assert.Equal(t, core.FetchReport{}, rep)
	assert.Equal(t, 1, fill.TotalRequests(), "the fill Server is asked only for the Article the main Server lacks")
	assert.Equal(t, 1, fill.Requests(lost))
}

func TestArticlesMissingEverywhereStillReachFetchedWithAReport(t *testing.T) {
	h := newHarness(t)
	main := h.addServer("main", 0, 2)
	fill := h.addServer("fill", 1, 2)
	files := map[string][]byte{"movie.mkv": payload(100_000, 1), "extra.bin": payload(20_000, 2)}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(main)
	fx.Install(fill)
	var movie, extra nntptest.FixtureFile
	for _, f := range fx.Files {
		if f.Name == "movie.mkv" {
			movie = f
		} else {
			extra = f
		}
	}
	// One Article of movie.mkv and every Article of extra.bin are gone.
	gone := append([]string{movie.MessageIDs[3]}, extra.MessageIDs...)
	main.Missing(gone...)
	fill.Missing(gone...)
	d := h.addDownload("movie", fx)
	h.start()

	h.waitState(d.ID, core.StateFetched)

	rep, ok := h.runner.Report(d.ID)
	require.True(t, ok)
	assert.Equal(t, core.FetchReport{MissingArticles: 3}, rep)
	detail, _ := h.runner.Detail(d.ID)
	assert.Equal(t, 1, detail.MissingFiles, "extra.bin never got created")
	assert.Positive(t, detail.MissingBytes)
	got := readDir(t, h.dirFor(d))
	require.Len(t, got, 1)
	require.Len(t, got["movie.mkv"], len(files["movie.mkv"]))
	assert.Equal(t, files["movie.mkv"][:30_000], got["movie.mkv"][:30_000])
	assert.Equal(t, files["movie.mkv"][40_000:], got["movie.mkv"][40_000:])
	assert.Equal(t, make([]byte, 10_000), got["movie.mkv"][30_000:40_000], "the lost range stays sparse")
	frep, err := h.runner.FetchReport(context.Background(), h.get(d.ID))
	require.NoError(t, err)
	assert.Equal(t, rep, frep)
}

func TestPausingMidDownloadStopsFetchingAndResumingContinuesWithoutRefetching(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(5 * time.Millisecond)
	files := map[string][]byte{"movie.mkv": payload(400_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000) // 40 articles
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	h.start()
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 8 }, 10*time.Second, time.Millisecond)

	h.update(d.ID, func(d *core.Download) { d.Paused = true })

	h.waitState(d.ID, core.StateQueued)
	atPause := stable(t, srv.TotalRequests)
	assert.Less(t, atPause, 40, "fetching stopped")

	h.update(d.ID, func(d *core.Download) { d.Paused = false })

	h.waitState(d.ID, core.StateFetched)
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
	refetched := 0
	for _, id := range fx.Files[0].MessageIDs {
		if srv.Requests(id) > 1 {
			refetched++
		}
	}
	assert.LessOrEqual(t, refetched, 1, "only the Article in flight at the pause may be fetched twice")
}

func TestRemovingADownloadMidwayCancelsItsWorkAndFreesTheSlot(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(5 * time.Millisecond)
	big := nntptest.BuildFixture(t, map[string][]byte{"big.bin": payload(400_000, 1)}, 10_000)
	small := nntptest.BuildFixture(t, map[string][]byte{"small.bin": payload(20_000, 2)}, 10_000)
	big.Install(srv)
	small.Install(srv)
	a := h.addDownload("big", big)
	h.start()
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 5 }, 10*time.Second, time.Millisecond)

	require.NoError(t, h.repo.Delete(context.Background(), a.ID))
	h.bus.Publish(core.Removed{ID: a.ID})

	atRemove := stable(t, srv.TotalRequests)
	assert.Less(t, atRemove, 40, "fetching stopped")
	b := h.addDownload("small", small)
	h.waitState(b.ID, core.StateFetched)
}

func TestDownloadGoesBackToQueuedWhenEveryServerRejectsTheLogin(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	srv.FailAuth(true)
	h.cfg.FailureBackoff = 300 * time.Millisecond
	files := map[string][]byte{"movie.mkv": payload(50_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	h.start()

	require.Eventually(t, func() bool { return srv.AuthAttempts() >= 1 }, 10*time.Second, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, core.StateQueued, h.get(d.ID).State, "not marked Fetched with everything Missing")
	_, ok := h.runner.Report(d.ID)
	assert.False(t, ok)

	srv.FailAuth(false) // the password was corrected
	h.waitState(d.ID, core.StateFetched)
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
}

func TestDiskGuardHoldsTheQueueUntilSpaceIsBack(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 2)
	files := map[string][]byte{"movie.mkv": payload(60_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(srv)
	h.setFree(1 << 30) // less than the 5 GiB default min_free
	rec := h.record()
	d := h.addDownload("movie", fx, func(d *core.Download) { d.TotalBytes = 60_000 })
	r := h.start()

	require.Eventually(t, func() bool { p, _ := r.QueuePaused(); return p }, 10*time.Second, time.Millisecond)
	paused, reason := r.QueuePaused()
	assert.True(t, paused)
	assert.Equal(t, runner.ReasonDiskFull, reason)
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, core.StateQueued, h.get(d.ID).State)
	assert.Zero(t, srv.TotalRequests())
	assert.GreaterOrEqual(t, indexOf(rec.all(), func(ev core.Event) bool {
		p, ok := ev.(runner.QueuePaused)
		return ok && p.Reason == runner.ReasonDiskFull
	}), 0)

	h.setFree(10 << 30)

	h.waitState(d.ID, core.StateFetched)
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
	paused, _ = r.QueuePaused()
	assert.False(t, paused)
	assert.GreaterOrEqual(t, indexOf(rec.all(), func(ev core.Event) bool { _, ok := ev.(runner.QueueResumed); return ok }), 0)
}

func TestDiskGuardPausesARunningDownloadAndResumesItLater(t *testing.T) {
	h := newHarness(t)
	h.cfg.DiskCheckInterval = 20 * time.Millisecond
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(5 * time.Millisecond)
	files := map[string][]byte{"movie.mkv": payload(400_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(srv)
	d := h.addDownload("movie", fx, func(d *core.Download) { d.TotalBytes = 400_000 })
	r := h.start()
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 5 }, 10*time.Second, time.Millisecond)

	h.setFree(1 << 20)

	h.waitState(d.ID, core.StateQueued)
	paused, reason := r.QueuePaused()
	assert.True(t, paused)
	assert.Equal(t, runner.ReasonDiskFull, reason)
	atPause := stable(t, srv.TotalRequests)
	assert.Less(t, atPause, 40)

	h.setFree(10 << 30)

	h.waitState(d.ID, core.StateFetched)
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
}

func TestPauseQueueAndResumeQueue(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(5 * time.Millisecond)
	files := map[string][]byte{"movie.mkv": payload(300_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	r := h.start()
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 5 }, 10*time.Second, time.Millisecond)

	r.PauseQueue()

	h.waitState(d.ID, core.StateQueued)
	paused, reason := r.QueuePaused()
	assert.True(t, paused)
	assert.Equal(t, runner.ReasonUser, reason)
	stable(t, srv.TotalRequests)
	assert.Equal(t, core.StateQueued, h.get(d.ID).State)

	r.ResumeQueue()

	h.waitState(d.ID, core.StateFetched)
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
}

func TestRecoverMovesFetchingDownloadsBackToQueued(t *testing.T) {
	h := newHarness(t)
	h.addServer("main", 0, 1)
	fx := nntptest.BuildFixture(t, map[string][]byte{"a.bin": payload(1_000, 1)}, 10_000)
	interrupted := h.addDownload("interrupted", fx, func(d *core.Download) { d.State = core.StateFetching })
	queued := h.addDownload("queued", fx)
	other := h.addDownload("torrent", fx, func(d *core.Download) {
		d.State = core.StateFetching
		d.SourceKind = core.SourceTorrent
	})
	r, err := runner.New(h.cfg)
	require.NoError(t, err)
	rec := h.record()

	require.NoError(t, r.Recover(context.Background()))

	assert.Equal(t, core.StateQueued, h.get(interrupted.ID).State)
	assert.Equal(t, core.StateQueued, h.get(queued.ID).State)
	assert.Equal(t, core.StateFetching, h.get(other.ID).State, "torrents are not ours")
	require.Eventually(t, func() bool {
		return indexOf(rec.all(), func(ev core.Event) bool {
			u, ok := ev.(core.DownloadUpdated)
			return ok && u.Download.ID == interrupted.ID && u.Download.State == core.StateQueued
		}) >= 0
	}, 5*time.Second, time.Millisecond)
}

func progressOf(rec *recorder, id core.ID) []core.DownloadProgress {
	var out []core.DownloadProgress
	for _, ev := range rec.all() {
		if p, ok := ev.(core.DownloadProgress); ok && p.ID == id {
			out = append(out, p)
		}
	}
	return out
}

func TestProgressEventsFollowTheThrottle(t *testing.T) {
	files := map[string][]byte{"movie.mkv": payload(400_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000) // 40 articles

	run := func(step time.Duration) []core.DownloadProgress {
		h := newHarness(t)
		clk := newFakeClock()
		clk.step = step
		h.cfg.Clock = clk
		fx.Install(h.addServer("main", 0, 2))
		rec := h.record()
		d := h.addDownload("movie", fx, func(d *core.Download) { d.TotalBytes = 400_000 })
		h.start()
		h.waitState(d.ID, core.StateFetched)
		h.stop()
		return progressOf(rec, d.ID)
	}

	frozen := run(0)
	moving := run(2 * time.Second)

	assert.LessOrEqual(t, len(frozen), 2, "a frozen clock passes the first report and the final one")
	assert.Greater(t, len(moving), 10, "reports pass once the interval has elapsed")
	last := moving[len(moving)-1]
	assert.Equal(t, int64(400_000), last.DoneBytes)
	assert.Equal(t, int64(400_000), last.TotalBytes)
	for i := 1; i < len(moving); i++ {
		assert.GreaterOrEqual(t, moving[i].DoneBytes, moving[i-1].DoneBytes)
	}
}

func TestDoneBytesAreStoredWhileFetching(t *testing.T) {
	h := newHarness(t)
	h.cfg.DoneBytesInterval = 20 * time.Millisecond
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(5 * time.Millisecond)
	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": payload(400_000, 1)}, 10_000)
	fx.Install(srv)
	d := h.addDownload("movie", fx, func(d *core.Download) { d.TotalBytes = 400_000 })
	h.start()

	require.Eventually(t, func() bool {
		got := h.get(d.ID)
		return got.State == core.StateFetching && got.DoneBytes > 0 && got.DoneBytes < 400_000
	}, 10*time.Second, 5*time.Millisecond)

	assert.Equal(t, int64(400_000), h.waitState(d.ID, core.StateFetched).DoneBytes)
}

func TestSpeedLimitHoldsTheRateWithinFifteenPercent(t *testing.T) {
	const limit = 200_000 // bytes per second on the wire
	h := newHarness(t)
	clk := newFakeClock()
	clk.auto = true // sleeping advances virtual time
	h.cfg.Clock = clk
	srv := h.addServer("main", 0, 4)
	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": payload(1_000_000, 1)}, 20_000)
	fx.Install(srv)
	var wire int64
	for _, id := range fx.MessageIDs() {
		for _, l := range fx.Articles[id] {
			wire += int64(len(l)) + 2
		}
	}
	d := h.addDownload("movie", fx, func(d *core.Download) { d.TotalBytes = wire })
	r, err := runner.New(h.cfg)
	require.NoError(t, err)
	h.runner = r
	r.SetSpeedLimit(limit)
	t0 := clk.now
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	h.waitState(d.ID, core.StateFetched)

	elapsed := clk.elapsed(t0).Seconds()
	got := float64(wire) / elapsed
	assert.InEpsilon(t, float64(limit), got, 0.15, "%d wire bytes in %.2f virtual seconds", wire, elapsed)
}

func TestSpeedLimitCanBeRemovedAtRuntime(t *testing.T) {
	h := newHarness(t)
	clk := newFakeClock()
	clk.auto = true
	h.cfg.Clock = clk
	srv := h.addServer("main", 0, 2)
	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": payload(200_000, 1)}, 20_000)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	t0 := clk.now
	r := h.start()
	r.SetSpeedLimit(1_000) // would take minutes of virtual time
	r.SetSpeedLimit(0)

	h.waitState(d.ID, core.StateFetched)

	assert.Less(t, clk.elapsed(t0), 5*time.Second)
}

// killMidRun starts the runner, waits for periodic saves after at least
// minRequests requests and then stops it without a graceful flush.
func killMidRun(t *testing.T, h *harness, store *fakeProgress, srv *nntptest.Server, minRequests int) {
	t.Helper()
	h.start()
	require.Eventually(t, func() bool {
		return srv.TotalRequests() >= minRequests && store.saveCount() >= 2
	}, 15*time.Second, time.Millisecond)
	store.freeze()
	h.stop()
	store.thaw()
}

func resumeHarness(t *testing.T) (*harness, *nntptest.Server, *fakeProgress, nntptest.Fixture, map[string][]byte) {
	h := newHarness(t)
	store := newFakeProgress()
	h.cfg.Progress = store
	h.cfg.SaveInterval = 30 * time.Millisecond
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(3 * time.Millisecond)
	files := map[string][]byte{"movie.mkv": payload(1_000_000, 1), "movie.nfo": payload(20_000, 2)}
	fx := nntptest.BuildFixture(t, files, 10_000) // 102 articles
	fx.Install(srv)
	return h, srv, store, fx, files
}

func TestResumeAfterACrashRefetchesOnlyWhatWasNotSaved(t *testing.T) {
	h, srv, store, fx, files := resumeHarness(t)
	d := h.addDownload("movie", fx)
	killMidRun(t, h, store, srv, 40)
	require.Equal(t, core.StateFetching, h.get(d.ID).State, "the crash leaves it Fetching")
	firstRun := srv.TotalRequests()
	require.Less(t, firstRun, 102)

	r := h.start()
	require.NoError(t, r.Recover(context.Background()))
	h.waitState(d.ID, core.StateFetched)

	assert.Equal(t, files, readDir(t, h.dirFor(d)))
	refetched := 0
	for _, id := range fx.MessageIDs() {
		assert.Positive(t, srv.Requests(id))
		if srv.Requests(id) > 1 {
			refetched++
		}
	}
	assert.Less(t, refetched, firstRun-10, "most of the first run's Articles were skipped")
	assert.Less(t, srv.TotalRequests(), firstRun+102-10)
	assert.False(t, store.has(d.ID), "the blob is deleted once Fetched")
}

func TestResumeStartsOverWhenTheSavedFilesAreGone(t *testing.T) {
	h, srv, store, fx, files := resumeHarness(t)
	d := h.addDownload("movie", fx)
	killMidRun(t, h, store, srv, 40)
	for name := range readDir(t, h.dirFor(d)) {
		require.NoError(t, os.Remove(filepath.Join(h.dirFor(d), name)))
	}

	h.start()
	h.waitState(d.ID, core.StateFetched) // Fetching without a job is picked up again

	assert.Equal(t, files, readDir(t, h.dirFor(d)))
}

func TestWithoutAProgressStoreARestartFetchesEverythingAgainWithoutDuplicates(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(3 * time.Millisecond)
	files := map[string][]byte{"movie.mkv": payload(400_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(srv)
	d := h.addDownload("movie", fx)
	h.start()
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 15 }, 10*time.Second, time.Millisecond)
	h.stop()
	require.Equal(t, core.StateFetching, h.get(d.ID).State)

	h.start() // a new Runner: its in-memory resume state is empty
	h.waitState(d.ID, core.StateFetched)

	got := readDir(t, h.dirFor(d))
	assert.Equal(t, files, got, "no movie.mkv.1 next to movie.mkv")
	_, err := os.Stat(filepath.Join(h.dirFor(d), "movie.nzb"))
	assert.NoError(t, err, "the stored NZB is kept")
}

func TestServerAddedWhileFetchingJoinsTheRunningDownload(t *testing.T) {
	h := newHarness(t)
	main := h.addServer("main", 0, 1)
	main.SetLatency(8 * time.Millisecond)
	files := map[string][]byte{"movie.mkv": payload(500_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(main)
	d := h.addDownload("movie", fx)
	h.start()
	require.Eventually(t, func() bool { return main.TotalRequests() >= 5 }, 10*time.Second, time.Millisecond)

	extra := nntptest.NewServer("extra")
	fx.Install(extra)
	h.nntp["extra"] = extra
	h.servers.mu.Lock()
	h.servers.specs = append(h.servers.specs, runner.ServerSpec{
		Name: "extra", Host: "extra.example", Port: 563, Priority: 0, MaxConnections: 4, Enabled: true,
	})
	h.servers.mu.Unlock()
	h.bus.Publish(core.SettingsChanged{})

	h.waitState(d.ID, core.StateFetched)
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
	assert.Positive(t, extra.TotalRequests(), "the new Server took part")
}

func TestNoEnabledServerLeavesDownloadsQueued(t *testing.T) {
	h := newHarness(t)
	fx := nntptest.BuildFixture(t, map[string][]byte{"a.bin": payload(5_000, 1)}, 10_000)
	srv := h.addServer("main", 0, 1)
	fx.Install(srv)
	h.servers.mu.Lock()
	h.servers.specs[0].Enabled = false
	h.servers.mu.Unlock()
	d := h.addDownload("a", fx)
	h.start()

	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, core.StateQueued, h.get(d.ID).State)

	h.servers.mu.Lock()
	h.servers.specs[0].Enabled = true
	h.servers.mu.Unlock()
	h.waitState(d.ID, core.StateFetched)
}

func TestStressThreeDownloadsOverThreeServers(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Usenet = 3 })
	servers := []*nntptest.Server{h.addServer("a", 0, 3), h.addServer("b", 0, 3), h.addServer("fill", 1, 4)}
	for _, s := range servers {
		s.SetLatency(time.Millisecond)
	}
	type dl struct {
		d     core.Download
		files map[string][]byte
	}
	var dls []dl
	for i := range 3 {
		files := map[string][]byte{
			fmt.Sprintf("movie%d.mkv", i): payload(300_000+i*1000, byte(10+i)),
			fmt.Sprintf("movie%d.nfo", i): payload(3_000, byte(20+i)),
		}
		fx := nntptest.BuildFixture(t, files, 7_000)
		for _, s := range servers {
			fx.Install(s)
		}
		// The two main servers each lack a different subset; fill has all.
		for j, id := range fx.MessageIDs() {
			switch j % 5 {
			case 0:
				servers[0].Missing(id)
			case 1:
				servers[1].Missing(id)
			case 2:
				servers[0].Missing(id)
				servers[1].Missing(id)
			}
		}
		dls = append(dls, dl{h.addDownload(fmt.Sprintf("dl%d", i), fx), files})
	}
	h.start()

	for _, x := range dls {
		h.waitState(x.d.ID, core.StateFetched)
		detail, ok := h.runner.Detail(x.d.ID)
		assert.True(t, ok)
		assert.Equal(t, runner.FetchDetail{}, detail)
		got := readDir(t, h.dirFor(x.d))
		for name, want := range x.files {
			if !bytes.Equal(want, got[name]) {
				first := -1
				for i := range want {
					if i >= len(got[name]) || want[i] != got[name][i] {
						first = i
						break
					}
				}
				t.Errorf("%s: len %d want %d, first diff at %d; names %v", name, len(got[name]), len(want), first, slices.Collect(maps.Keys(got)))
			}
		}
	}
}

// noUpdateRepo fails the test when the runner writes a whole row with Update:
// that read-modify-write can overwrite a concurrent API edit.
type noUpdateRepo struct {
	core.DownloadRepo
	t       *testing.T
	updates atomic.Int32
}

func (r *noUpdateRepo) Update(ctx context.Context, d core.Download) error {
	r.updates.Add(1)
	r.t.Errorf("runner called DownloadRepo.Update for %s: progress must use UpdateProgress", d.ID)
	return r.DownloadRepo.Update(ctx, d)
}

func TestProgressIsStoredWithoutRewritingTheWholeDownloadRow(t *testing.T) {
	h := newHarness(t)
	guard := &noUpdateRepo{DownloadRepo: h.repo, t: t}
	h.cfg.Downloads = guard
	h.cfg.DoneBytesInterval = 20 * time.Millisecond
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(5 * time.Millisecond)
	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": payload(400_000, 1)}, 10_000)
	fx.Install(srv)
	d := h.addDownload("movie", fx, func(d *core.Download) { d.TotalBytes = 400_000 })
	h.start()

	got := h.waitState(d.ID, core.StateFetched)

	assert.EqualValues(t, 400_000, got.DoneBytes)
	assert.Zero(t, guard.updates.Load())
}

func TestHoldStopsTheFetchAndKeepsTheDownloadFromRestartingUntilReleased(t *testing.T) {
	h := newHarness(t)
	srv := h.addServer("main", 0, 1)
	srv.SetLatency(5 * time.Millisecond)
	files := map[string][]byte{"big.bin": payload(400_000, 1)}
	fx := nntptest.BuildFixture(t, files, 10_000)
	fx.Install(srv)
	d := h.addDownload("big", fx)
	r := h.start()
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 5 }, 10*time.Second, time.Millisecond)

	require.NoError(t, r.Hold(context.Background(), d.ID))

	atHold := srv.TotalRequests()
	time.Sleep(200 * time.Millisecond) // several control-loop passes
	assert.Equal(t, atHold, srv.TotalRequests(), "no request after Hold returned")
	assert.NotEqual(t, core.StateFetched, h.get(d.ID).State)

	r.Release(d.ID)
	h.waitState(d.ID, core.StateFetched)
	assert.Equal(t, files, readDir(t, h.dirFor(d)))
}

func TestHoldOfADownloadWithoutAJobReturnsAtOnce(t *testing.T) {
	h := newHarness(t)
	r := h.start()

	require.NoError(t, r.Hold(context.Background(), core.NewID()))
}
