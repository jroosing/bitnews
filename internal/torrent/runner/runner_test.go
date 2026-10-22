package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/torrent/runner"
)

func TestActivatesQueuedTorrentsUpToTheLimitAndAddsThemToTheEngine(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 2 })
	a, b, c := h.addTorrent(), h.addTorrent(), h.addTorrent()

	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.waitState(b.ID, core.StateFetching)

	assert.Equal(t, core.StateQueued, h.get(c.ID).State, "the limit is 2")
	adds := h.calls("Add")
	require.Len(t, adds, 2)
	assert.Equal(t, torrentBytes(a.ID), adds[0].Spec.Metainfo)
	assert.Equal(t, filepath.Join(h.incomplete, string(a.ID)), adds[0].Spec.StorageDir, "the directory of the stored .torrent")
	assert.False(t, adds[0].Spec.Paused)
}

func TestHigherPriorityTakesTheSlotFirst(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 1 })
	first := h.addTorrent(withPosition(0))
	second := h.addTorrent(withPosition(1))
	urgent := h.addTorrent(withPosition(2), withPriority(core.DownloadPriorityHigh))

	h.start()
	h.waitState(urgent.ID, core.StateFetching)

	assert.Equal(t, []core.State{core.StateQueued, core.StateQueued}, h.states(first.ID, second.ID))
	require.Len(t, h.calls("Add"), 1)
}

func TestForceIgnoresTheLimit(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 1 })
	a := h.addTorrent()
	b := h.addTorrent(withPriority(core.DownloadPriorityForce))

	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.waitState(b.ID, core.StateFetching)
}

func TestSeedingTorrentsDoNotCountAgainstTheLimit(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 1 })
	h.addTorrent(withState(core.StateFetched))
	h.addTorrent(withState(core.StatePostProcessing))
	h.addTorrent(withState(core.StateReady))
	queued := h.addTorrent()

	h.start()

	h.waitState(queued.ID, core.StateFetching)
}

func TestLoweringTheLimitPausesTheSurplusAndRequeuesIt(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 2 })
	a, b := h.addTorrent(withPosition(0)), h.addTorrent(withPosition(1))
	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.waitState(b.ID, core.StateFetching)

	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 1 })
	h.bus.Publish(core.SettingsChanged{})

	h.waitState(b.ID, core.StateQueued)
	assert.Equal(t, core.StateFetching, h.get(a.ID).State)
	require.Len(t, h.callsFor("Pause", b.ID), 1)
	assert.Empty(t, h.callsFor("Pause", a.ID))
}

func TestPausingADownloadPausesItsTorrentAndResumingContinuesIt(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)

	d := h.get(a.ID)
	d.Paused = true
	require.NoError(t, h.downloads.Update(t.Context(), d))
	h.bus.Publish(core.DownloadUpdated{Download: d})
	h.waitState(a.ID, core.StateQueued)
	require.Len(t, h.callsFor("Pause", a.ID), 1)

	d = h.get(a.ID)
	d.Paused = false
	require.NoError(t, h.downloads.Update(t.Context(), d))
	h.bus.Publish(core.DownloadUpdated{Download: d})
	h.waitState(a.ID, core.StateFetching)

	assert.Len(t, h.callsFor("Resume", a.ID), 1, "the torrent is resumed, not added again")
	assert.Len(t, h.callsFor("Add", a.ID), 1)
}

func TestRecoverMovesFetchingDownloadsBackToQueued(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent(withState(core.StateFetching))
	b := h.addTorrent(withState(core.StateQueued))
	h.build()

	require.NoError(t, h.runner.Recover(t.Context()))

	assert.Equal(t, []core.State{core.StateQueued, core.StateQueued}, h.states(a.ID, b.ID))
}

func TestRecoverAddsSeedingTorrentsBackToTheEngine(t *testing.T) {
	h := newHarness(t)
	fetched := h.addTorrent(withState(core.StateFetched))
	post := h.addTorrent(withState(core.StatePostProcessing))
	ready := h.addTorrent(withState(core.StateReady))
	failedAfterFetch := h.addTorrent(withState(core.StateFailed), withFetched(time.Now()))
	failedBefore := h.addTorrent(withState(core.StateFailed))
	queued := h.addTorrent()
	h.build()

	require.NoError(t, h.runner.Recover(t.Context()))

	added := map[core.InfoHash]bool{}
	for _, c := range h.calls("Add") {
		added[c.Hash] = true
		assert.False(t, c.Spec.Paused, "seeding goes on")
	}
	for _, d := range []core.Download{fetched, post, ready, failedAfterFetch} {
		assert.True(t, added[hashOf(d.ID)], "%s is seeding again", d.Name)
	}
	assert.False(t, added[hashOf(failedBefore.ID)], "a Download that failed before it was Fetched has nothing to seed")
	assert.False(t, added[hashOf(queued.ID)], "the Queue adds it when it is activated")
}

func TestRecoverSkipsTorrentsWhoseDataIsGone(t *testing.T) {
	h := newHarness(t)
	gone := h.addTorrent(withState(core.StateReady))
	require.NoError(t, os.RemoveAll(filepath.Join(h.incomplete, string(gone.ID))))
	h.build()

	require.NoError(t, h.runner.Recover(t.Context()))

	assert.Empty(t, h.calls("Add"))
}

func TestPausingTheQueuePausesRunningTorrentsAndResumingStartsThemAgain(t *testing.T) {
	h := newHarness(t)
	a, b := h.addTorrent(withPosition(0)), h.addTorrent(withPosition(1))
	sub := h.bus.Subscribe(t.Context())
	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.waitState(b.ID, core.StateFetching)

	h.runner.PauseQueue()
	h.waitState(a.ID, core.StateQueued)
	h.waitState(b.ID, core.StateQueued)

	paused, reason := h.runner.QueuePaused()
	assert.True(t, paused)
	assert.Equal(t, runner.ReasonUser, reason)
	assert.Len(t, h.callsFor("Pause", a.ID), 1)
	assert.Equal(t, runner.QueuePaused{Reason: runner.ReasonUser}, waitEventOf[runner.QueuePaused](t, sub))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, []core.State{core.StateQueued, core.StateQueued}, h.states(a.ID, b.ID), "nothing is activated while paused")

	h.runner.ResumeQueue()
	h.waitState(a.ID, core.StateFetching)
	h.waitState(b.ID, core.StateFetching)
	waitEventOf[runner.QueueResumed](t, sub)
	paused, _ = h.runner.QueuePaused()
	assert.False(t, paused)
}

func TestSpeedLimitsGoToTheEngineAtStartAndWhenChanged(t *testing.T) {
	h := newHarness(t)
	h.build()
	h.runner.SetRateLimits(1000, 500)
	h.startRunnerBuilt()

	h.eventually(func() bool {
		cs := h.calls("SetRateLimits")
		return len(cs) > 0 && cs[len(cs)-1].Down == 1000 && cs[len(cs)-1].Up == 500
	}, "the limits reach the engine")

	h.runner.SetRateLimits(0, 0)
	h.eventually(func() bool {
		cs := h.calls("SetRateLimits")
		return cs[len(cs)-1].Down == 0 && cs[len(cs)-1].Up == 0
	}, "unlimited reaches the engine")
}

func TestDiskGuardKeepsTheQueueFromStartingAndResumesWhenSpaceIsBack(t *testing.T) {
	h := newHarness(t)
	h.cfg.MinFree = func(context.Context) (uint64, error) { return 100, nil }
	h.setFree(500) // needs 1000 remaining + 100
	a := h.addTorrent(withTotal(1000))
	sub := h.bus.Subscribe(t.Context())
	h.start()

	ev := waitEventOf[runner.QueuePaused](t, sub)
	assert.Equal(t, runner.ReasonDiskFull, ev.Reason)
	paused, reason := h.runner.QueuePaused()
	assert.True(t, paused)
	assert.Equal(t, runner.ReasonDiskFull, reason)
	assert.Equal(t, core.StateQueued, h.get(a.ID).State)
	assert.Empty(t, h.calls("Add"))

	h.setFree(5000)
	h.waitState(a.ID, core.StateFetching)
	waitEventOf[runner.QueueResumed](t, sub)
	paused, _ = h.runner.QueuePaused()
	assert.False(t, paused)
}

func TestDiskGuardPausesRunningTorrentsWhenTheDiskFills(t *testing.T) {
	h := newHarness(t)
	h.cfg.MinFree = func(context.Context) (uint64, error) { return 100, nil }
	a := h.addTorrent(withTotal(1000))
	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.engine.SetStats(hashOf(a.ID), core.TorrentStats{State: core.TorrentDownloading, Size: 1000})

	h.setFree(10)

	h.waitState(a.ID, core.StateQueued)
	paused, reason := h.runner.QueuePaused()
	assert.True(t, paused)
	assert.Equal(t, runner.ReasonDiskFull, reason)
	assert.NotEmpty(t, h.callsFor("Pause", a.ID))
}

// waitEventOf returns the next event of type E on sub.
func waitEventOf[E core.Event](t *testing.T, sub *core.Subscription) E {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			require.True(t, ok, "bus closed")
			if e, ok := ev.(E); ok {
				return e
			}
		case <-timeout:
			var zero E
			require.FailNow(t, "timed out waiting for an event", "%T", zero)
		}
	}
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
}

func TestProgressEventsAreThrottledAndDoneBytesAreStoredAtMostEveryInterval(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	sub := h.bus.Subscribe(t.Context())
	h.start()
	h.waitState(a.ID, core.StateFetching)

	h.engine.SetStats(hashOf(a.ID), core.TorrentStats{State: core.TorrentDownloading, Size: 1000, DoneBytes: 300, DownRate: 50})
	first := waitEventOf[core.DownloadProgress](t, sub)
	assert.Equal(t, core.DownloadProgress{ID: a.ID, DoneBytes: 300, TotalBytes: 1000, BytesPerSecond: 50}, first)
	h.eventually(func() bool { return h.get(a.ID).DoneBytes == 300 }, "done bytes stored")

	h.engine.SetStats(hashOf(a.ID), core.TorrentStats{State: core.TorrentDownloading, Size: 1000, DoneBytes: 400, DownRate: 60})
	time.Sleep(100 * time.Millisecond)
	assert.EqualValues(t, 300, h.get(a.ID).DoneBytes, "not before the interval has passed")
	select {
	case ev := <-sub.Events():
		if p, ok := ev.(core.DownloadProgress); ok {
			t.Fatalf("progress event inside the interval: %+v", p)
		}
	default:
	}

	h.advance(3 * time.Second)
	second := waitEventOf[core.DownloadProgress](t, sub)
	assert.EqualValues(t, 400, second.DoneBytes)
	h.eventually(func() bool { return h.get(a.ID).DoneBytes == 400 }, "done bytes stored again")
}

func TestMagnetIsAddedToTheEngineAndWaitsInFetchingMetadata(t *testing.T) {
	h := newHarness(t)
	m := h.addMagnet()

	h.start()
	h.eventually(func() bool { return len(h.callsFor("Add", m.ID)) == 1 }, "magnet added to the engine")

	add := h.callsFor("Add", m.ID)[0]
	assert.Equal(t, "magnet:?xt=urn:btih:"+hashOf(m.ID).String()+"&dn=x", add.Spec.Magnet)
	assert.Equal(t, filepath.Join(h.incomplete, string(m.ID)), add.Spec.StorageDir)
	assert.False(t, add.Spec.Paused, "metadata needs peers")
	assert.Empty(t, add.Spec.Metainfo)
	assert.Equal(t, core.StateFetchingMetadata, h.get(m.ID).State)
}

func TestMagnetMetadataIsSavedAndTheDownloadIsQueuedThenActivated(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 0 }) // keep it Queued so we can look
	m := h.addMagnet()
	h.start()
	h.eventually(func() bool { return len(h.callsFor("Add", m.ID)) == 1 }, "magnet added")
	hash := hashOf(m.ID)
	h.engine.SetMetainfo(hash, []byte("the real .torrent"))
	h.engine.SetStats(hash, core.TorrentStats{State: core.TorrentDownloading, Name: "Real.Name.S01", Size: 4096})

	h.engine.Emit(core.TorrentEvent{Kind: core.TorrentMetadataReceived, Hash: hash})
	h.waitState(m.ID, core.StateQueued)

	d := h.get(m.ID)
	assert.Equal(t, "Real.Name.S01", d.Name)
	assert.EqualValues(t, 4096, d.TotalBytes)
	src, err := h.sources.Get(t.Context(), m.ID)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(h.incomplete, string(m.ID), "Real.Name.S01.torrent"), src.TorrentPath)
	raw, err := os.ReadFile(src.TorrentPath)
	require.NoError(t, err)
	assert.Equal(t, "the real .torrent", string(raw))
	fi, err := os.Stat(src.TorrentPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "tracker passkeys stay private")
	h.eventually(func() bool { return len(h.callsFor("Pause", m.ID)) > 0 }, "no data flows before the Queue activates it")

	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 1 })
	h.bus.Publish(core.SettingsChanged{})
	h.waitState(m.ID, core.StateFetching)
	assert.NotEmpty(t, h.callsFor("Resume", m.ID))
	assert.Len(t, h.callsFor("Add", m.ID), 1)
}

func TestMagnetThatTheQueueHasRoomForKeepsRunningWithoutAPauseAndResume(t *testing.T) {
	h := newHarness(t)
	m := h.addMagnet()
	h.start()
	h.eventually(func() bool { return len(h.callsFor("Add", m.ID)) == 1 }, "magnet added")
	hash := hashOf(m.ID)
	h.engine.SetMetainfo(hash, []byte("x"))
	h.engine.SetStats(hash, core.TorrentStats{State: core.TorrentDownloading, Name: "Room", Size: 10})

	h.engine.Emit(core.TorrentEvent{Kind: core.TorrentMetadataReceived, Hash: hash})
	h.waitState(m.ID, core.StateFetching)

	assert.Empty(t, h.callsFor("Pause", m.ID), "a pause would drop the peers that delivered the metadata")
}

func TestMagnetTorrentFileNeverTakesTheNameOfTheTorrentsOwnData(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 0 })
	m := h.addMagnet()
	h.start()
	h.eventually(func() bool { return len(h.callsFor("Add", m.ID)) == 1 }, "magnet added")
	hash := hashOf(m.ID)
	h.engine.SetMetainfo(hash, []byte("x"))
	h.engine.SetStats(hash, core.TorrentStats{State: core.TorrentDownloading, Name: "evil", Size: 10,
		Files: []core.TorrentFile{{Path: "EVIL.torrent", Size: 10}}})

	h.engine.Emit(core.TorrentEvent{Kind: core.TorrentMetadataReceived, Hash: hash})
	h.waitState(m.ID, core.StateQueued)

	src, err := h.sources.Get(t.Context(), m.ID)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(h.incomplete, string(m.ID), ".bitnews-metainfo.torrent"), src.TorrentPath)
}

func TestPausedMagnetStaysOutOfTheEngineUntilResumed(t *testing.T) {
	h := newHarness(t)
	m := h.addMagnet(withPaused())
	h.start()
	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, h.calls("Add"))

	d := h.get(m.ID)
	d.Paused = false
	require.NoError(t, h.downloads.Update(t.Context(), d))
	h.bus.Publish(core.DownloadUpdated{Download: d})

	h.eventually(func() bool { return len(h.callsFor("Add", m.ID)) == 1 }, "added once resumed")
}

func TestMagnetWhoseTorrentFileWasSavedBeforeACrashIsQueuedWithoutTheEvent(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 0 })
	m := h.addMagnet()
	dir := filepath.Join(h.incomplete, string(m.ID))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "saved.torrent")
	require.NoError(t, os.WriteFile(path, torrentBytes(m.ID), 0o600))
	require.NoError(t, h.sources.SetTorrentPath(t.Context(), m.ID, path))

	h.start()
	h.eventually(func() bool { return len(h.calls("Add")) == 1 }, "added from the file")
	h.engine.SetStats(hashOf(m.ID), core.TorrentStats{Name: "From.File", Size: 77})

	h.waitState(m.ID, core.StateQueued)
	assert.Equal(t, torrentBytes(m.ID), h.calls("Add")[0].Spec.Metainfo, "the file is used, not the magnet")
	assert.Equal(t, "From.File", h.get(m.ID).Name)
}

func TestRemovedEventDropsTheTorrentFromTheEngineAndKeepsItsData(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	b := h.addTorrent(withState(core.StateReady))
	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.eventually(func() bool { return len(h.callsFor("Add", b.ID)) == 1 }, "the seeding torrent is added")

	require.NoError(t, h.downloads.Delete(t.Context(), a.ID))
	h.bus.Publish(core.Removed{ID: a.ID})
	require.NoError(t, h.downloads.Delete(t.Context(), b.ID))
	h.bus.Publish(core.Removed{ID: b.ID})

	h.eventually(func() bool { return len(h.callsFor("Remove", a.ID)) == 1 && len(h.callsFor("Remove", b.ID)) == 1 }, "removed from the engine")
	assert.False(t, h.callsFor("Remove", a.ID)[0].DeleteData, "the daemon's data remover deletes files")
	assert.False(t, h.callsFor("Remove", b.ID)[0].DeleteData)
}

func TestADownloadThatVanishedWithoutAnEventIsDroppedOnTheNextPoll(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)

	require.NoError(t, h.downloads.Delete(t.Context(), a.ID))

	h.eventually(func() bool { return len(h.callsFor("Remove", a.ID)) == 1 }, "dropped")
}

func TestHoldStopsTheTorrentAndKeepsItOutUntilReleased(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	b := h.addTorrent(withState(core.StateReady))
	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.eventually(func() bool { return len(h.callsFor("Add", b.ID)) == 1 }, "the seeding torrent is added")

	require.NoError(t, h.runner.Hold(t.Context(), a.ID))
	require.NoError(t, h.runner.Hold(t.Context(), b.ID))

	require.Len(t, h.callsFor("Remove", a.ID), 1, "the engine let go of the files before Hold returned")
	require.Len(t, h.callsFor("Remove", b.ID), 1)
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, h.callsFor("Add", a.ID), 1, "not added again while held")
	assert.Len(t, h.callsFor("Add", b.ID), 1)

	h.runner.Release(a.ID)
	h.runner.Release(b.ID)

	h.eventually(func() bool { return len(h.callsFor("Add", a.ID)) == 2 && len(h.callsFor("Add", b.ID)) == 2 }, "added again after Release")
}

func TestHoldOfAnUnknownDownloadReturnsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.start()

	require.NoError(t, h.runner.Hold(t.Context(), core.NewID()))
}

func TestHoldEndsWithTheRemovedEvent(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)
	require.NoError(t, h.runner.Hold(t.Context(), a.ID))

	require.NoError(t, h.downloads.Delete(t.Context(), a.ID))
	h.bus.Publish(core.Removed{ID: a.ID})
	time.Sleep(50 * time.Millisecond)

	// A new Download with the same ID cannot exist; the hold must not leak.
	assert.Len(t, h.callsFor("Remove", a.ID), 1)
}
