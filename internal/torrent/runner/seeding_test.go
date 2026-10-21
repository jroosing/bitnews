package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/torrent/runner"
)

// seedingStats is what the engine says about a torrent that has been seeding.
func seedingStats(uploaded int64, seeding time.Duration) core.TorrentStats {
	return core.TorrentStats{State: core.TorrentSeeding, Size: 1000, DoneBytes: 1000, Uploaded: uploaded, SeedingTime: seeding}
}

// addSeeding stores a Ready torrent and returns it once the runner has it.
func (h *harness) addSeeding(opts ...dlOpt) core.Download {
	h.t.Helper()
	return h.addTorrent(append([]dlOpt{withState(core.StateReady), withTotal(1000)}, opts...)...)
}

func (h *harness) waitInEngine(ids ...core.ID) {
	h.t.Helper()
	for _, id := range ids {
		h.eventually(func() bool { return len(h.callsFor("Add", id)) > 0 }, "torrent %s is in the engine", id)
	}
}

func (h *harness) paused(id core.ID) bool {
	st, err := h.engine.Stats(hashOf(id))
	return err == nil && st.State == core.TorrentPaused
}

func TestSeedingCountersArePersistedPeriodically(t *testing.T) {
	h := newHarness(t)
	a := h.addSeeding()
	h.start()
	h.waitInEngine(a.ID)

	h.engine.SetStats(hashOf(a.ID), seedingStats(500, 40*time.Second))

	h.eventually(func() bool {
		rec, err := h.records.Get(t.Context(), a.ID)
		return err == nil && rec.Uploaded == 500 && rec.SeedingTime == 40*time.Second
	}, "counters persisted")
}

func TestSeedingCountersAreAddedToThoseOfEarlierRuns(t *testing.T) {
	h := newHarness(t)
	a := h.addSeeding()
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, Uploaded: 1000, SeedingTime: 100 * time.Second}))
	h.start()
	h.waitInEngine(a.ID)

	h.engine.SetStats(hashOf(a.ID), seedingStats(200, 20*time.Second))

	h.eventually(func() bool {
		rec, _ := h.records.Get(t.Context(), a.ID)
		return rec.Uploaded == 1200 && rec.SeedingTime == 120*time.Second
	}, "the engine counts from zero after a restart, the record carries on")
}

func TestSeedingCountersAreWrittenOnShutdown(t *testing.T) {
	h := newHarness(t)
	h.cfg.PersistInterval = time.Hour
	a := h.addSeeding()
	h.start()
	h.waitInEngine(a.ID)
	h.engine.SetStats(hashOf(a.ID), seedingStats(777, 9*time.Second))

	h.stop()

	rec, err := h.records.Get(t.Context(), a.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 777, rec.Uploaded)
	assert.Equal(t, 9*time.Second, rec.SeedingTime)
}

func TestSeedingCountersAreWrittenWhenTheTorrentIsFetched(t *testing.T) {
	h := newHarness(t)
	h.cfg.PersistInterval = time.Hour
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)

	h.engine.SetStats(hashOf(a.ID), seedingStats(30, 0))
	h.waitState(a.ID, core.StateFetched)

	assert.EqualValues(t, 30, h.record(a.ID).Uploaded)
}

func TestRatioLimitPausesTheTorrentUsingTheCountersOfEarlierRuns(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit = 1 })
	a := h.addSeeding()
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, Uploaded: 600}))
	sub := h.bus.Subscribe(t.Context())
	h.start()
	h.waitInEngine(a.ID)

	h.engine.SetStats(hashOf(a.ID), seedingStats(300, time.Hour)) // 900 / 1000: not yet
	time.Sleep(100 * time.Millisecond)
	assert.False(t, h.paused(a.ID))

	h.engine.SetStats(hashOf(a.ID), seedingStats(500, time.Hour)) // 1100 / 1000
	h.eventually(func() bool { return h.paused(a.ID) }, "paused at the ratio")

	ev := waitEventOf[runner.ShareLimitReached](t, sub)
	assert.Equal(t, runner.ShareLimitReached{ID: a.ID, Action: core.SeedingActionPause}, ev)
	assert.Empty(t, h.remover.Calls())
	assert.Empty(t, h.callsFor("Remove", a.ID))
	assert.Equal(t, core.StateReady, h.get(a.ID).State, "the Download is untouched")
}

func TestSeedingTimeLimitUsesTheTimeOfEarlierRuns(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.SeedingTimeMinutes = 1 })
	a := h.addSeeding()
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, SeedingTime: 30 * time.Second}))
	h.start()
	h.waitInEngine(a.ID)

	h.engine.SetStats(hashOf(a.ID), seedingStats(0, 20*time.Second))
	time.Sleep(100 * time.Millisecond)
	assert.False(t, h.paused(a.ID), "50 s of 60 s")

	h.engine.SetStats(hashOf(a.ID), seedingStats(0, 31*time.Second))
	h.eventually(func() bool { return h.paused(a.ID) }, "paused at the time")
}

func TestNoDefaultLimitMeansSeedingForever(t *testing.T) {
	h := newHarness(t)
	a := h.addSeeding()
	h.start()
	h.waitInEngine(a.ID)

	h.engine.SetStats(hashOf(a.ID), seedingStats(1<<40, 1000*time.Hour))
	time.Sleep(150 * time.Millisecond)

	assert.False(t, h.paused(a.ID))
}

func TestPerDownloadLimitOverridesTheDefault(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit = 1 })
	unlimited := h.addSeeding()
	tighter := h.addSeeding()
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: unlimited.ID, RatioLimit: new(-1.0)}))
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: tighter.ID, RatioLimit: new(0.5)}))
	h.start()
	h.waitInEngine(unlimited.ID, tighter.ID)

	h.engine.SetStats(hashOf(unlimited.ID), seedingStats(5000, time.Hour))
	h.engine.SetStats(hashOf(tighter.ID), seedingStats(600, time.Hour)) // 0.6: over its 0.5, under the default 1

	h.eventually(func() bool { return h.paused(tighter.ID) }, "the own limit applies")
	assert.False(t, h.paused(unlimited.ID), "-1 is unlimited whatever the default says")
}

//go:fix inline
func ptr[T any](v T) *T { return new(v) }

func TestOnlyFetchedTorrentsAreSeeding(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit = 0.1 })
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)

	h.engine.SetStats(hashOf(a.ID), core.TorrentStats{State: core.TorrentDownloading, Size: 1000, DoneBytes: 100, Uploaded: 900})
	time.Sleep(150 * time.Millisecond)

	assert.False(t, h.paused(a.ID), "uploading while downloading is not Seeding")
	assert.Equal(t, core.StateFetching, h.get(a.ID).State)
}

func TestLimitActionRemoveDeletesTheDownloadAndKeepsItsData(t *testing.T) {
	for action, wantData := range map[core.SeedingAction]bool{core.SeedingActionRemove: false, core.SeedingActionRemoveData: true} {
		t.Run(string(action), func(t *testing.T) {
			h := newHarness(t)
			h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit, s.Seeding.Action = 1, action })
			a := h.addSeeding()
			h.start()
			h.waitInEngine(a.ID)

			h.engine.SetStats(hashOf(a.ID), seedingStats(2000, time.Hour))

			h.eventually(func() bool { return len(h.remover.Calls()) > 0 }, "the Remover is called")
			assert.Equal(t, removeCall{ID: a.ID, DeleteData: wantData}, h.remover.Calls()[0])
			h.eventually(func() bool { return len(h.callsFor("Remove", a.ID)) == 1 }, "the Removed event drops the torrent")
		})
	}
}

func TestRemoveActionWithoutARemoverPauses(t *testing.T) {
	h := newHarness(t)
	h.cfg.Remover = nil
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit, s.Seeding.Action = 1, core.SeedingActionRemove })
	a := h.addSeeding()
	h.start()
	h.waitInEngine(a.ID)

	h.engine.SetStats(hashOf(a.ID), seedingStats(2000, time.Hour))

	h.eventually(func() bool { return h.paused(a.ID) }, "paused instead")
}

func TestFailedRemovalIsRetried(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit, s.Seeding.Action = 1, core.SeedingActionRemove })
	h.remover.err = context.DeadlineExceeded
	a := h.addSeeding()
	h.start()
	h.waitInEngine(a.ID)

	h.engine.SetStats(hashOf(a.ID), seedingStats(2000, time.Hour))
	h.eventually(func() bool { return len(h.remover.Calls()) >= 2 }, "tried again")
}

func TestSetShareLimitAppliesAtOnceAndRaisingItResumes(t *testing.T) {
	h := newHarness(t)
	a := h.addSeeding()
	h.start()
	h.waitInEngine(a.ID)
	h.engine.SetStats(hashOf(a.ID), seedingStats(600, time.Hour))

	require.NoError(t, h.runner.SetShareLimit(t.Context(), a.ID, core.ShareLimit{Ratio: 0.5, SeedingTime: -1}))
	h.eventually(func() bool { return h.paused(a.ID) }, "paused by the new limit")
	rec := h.record(a.ID)
	assert.Equal(t, new(0.5), rec.RatioLimit)

	require.NoError(t, h.runner.SetShareLimit(t.Context(), a.ID, core.UnlimitedShareLimit()))
	h.eventually(func() bool { return !h.paused(a.ID) }, "resumed when the limit is gone")
	assert.NotEmpty(t, h.callsFor("Resume", a.ID))
}

func TestClearShareLimitFallsBackToTheDefault(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit = 10 })
	a := h.addSeeding()
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, RatioLimit: new(0.1)}))
	h.start()
	h.waitInEngine(a.ID)
	h.engine.SetStats(hashOf(a.ID), seedingStats(500, time.Hour))
	h.eventually(func() bool { return h.paused(a.ID) }, "own limit reached")

	require.NoError(t, h.runner.ClearShareLimit(t.Context(), a.ID))

	h.eventually(func() bool { return !h.paused(a.ID) }, "default of 10 is far away")
	rec := h.record(a.ID)
	assert.Nil(t, rec.RatioLimit)
	assert.Nil(t, rec.SeedingTimeLimit)
}

func TestEffectiveShareLimitCombinesDefaultAndOverride(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit, s.Seeding.SeedingTimeMinutes = 2, 60 })
	a := h.addSeeding()
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, SeedingTimeLimit: ptr(-time.Second)}))
	h.build()

	got, err := h.runner.EffectiveShareLimit(t.Context(), a.ID)

	require.NoError(t, err)
	assert.Equal(t, core.ShareLimit{Ratio: 2, SeedingTime: -time.Second}, got)
}

func TestTorrentThatAlreadyReachedItsLimitIsNotUploadingAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit = 1 })
	a := h.addSeeding()
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, Uploaded: 1500}))
	h.build()
	require.NoError(t, h.runner.Recover(t.Context()))

	adds := h.calls("Add")
	require.Len(t, adds, 1)
	assert.True(t, adds[0].Spec.Paused, "added paused: no upload window before the first evaluation")
}
