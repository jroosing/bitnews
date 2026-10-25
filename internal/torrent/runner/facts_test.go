package runner_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// registered stores a seeding Download with rec as its record and lets the
// runner add it to the engine.
func (h *harness) registered(rec core.TorrentRecord, opts ...dlOpt) core.Download {
	h.t.Helper()
	d := h.addSeeding(opts...)
	rec.DownloadID = d.ID
	require.NoError(h.t, h.records.Upsert(h.t.Context(), rec))
	h.build()
	require.NoError(h.t, h.runner.Recover(h.t.Context()))
	require.True(h.t, h.runner.Managed(d.ID))
	return d
}

func TestFactsOfARegisteredTorrentAddTheEngineCountersToTheStoredBases(t *testing.T) {
	h := newHarness(t)
	d := h.registered(core.TorrentRecord{Uploaded: 400, SeedingTime: 100 * time.Second, RatioLimit: ptr(3.0)})
	files := []core.TorrentFile{{Path: "a/b.mkv", Size: 1500}, {Path: "a/c.nfo", Size: 500}}
	h.engine.SetStats(hashOf(d.ID), core.TorrentStats{
		Name: "From the engine", Private: true, Size: 2000, DoneBytes: 1800, DownRate: 7, UpRate: 9,
		Peers: 4, Seeds: 2, Uploaded: 600, SeedingTime: 50 * time.Second, Files: files,
	})

	f, ok, err := h.runner.Facts(t.Context(), d.ID)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, hashOf(d.ID), f.Hash)
	assert.True(t, f.Registered)
	assert.True(t, f.Live)
	assert.True(t, f.Fetched)
	assert.Equal(t, "From the engine", f.Name)
	assert.Equal(t, int64(2000), f.Size)
	assert.Equal(t, int64(1800), f.DoneBytes)
	assert.Equal(t, [2]int64{7, 9}, [2]int64{f.DownRate, f.UpRate})
	assert.Equal(t, [2]int{4, 2}, [2]int{f.Peers, f.Seeds})
	assert.True(t, f.Private)
	assert.Equal(t, int64(1000), f.Uploaded, "earlier runs + this one")
	assert.Equal(t, 150*time.Second, f.SeedingTime)
	assert.InDelta(t, 0.5, f.Ratio, 1e-9, "uploaded over the engine's size")
	require.NotNil(t, f.OwnRatioLimit)
	assert.InDelta(t, 3.0, *f.OwnRatioLimit, 1e-9)
	assert.Nil(t, f.OwnSeedingTimeLimit)
	assert.Equal(t, h.runner.Dir(d), f.Dir)
	assert.Equal(t, files, f.Files)
}

func TestFactsOfARegisteredTorrentWhoseStatsFailAreTheBasesOnly(t *testing.T) {
	h := newHarness(t)
	d := h.registered(core.TorrentRecord{Uploaded: 400, SeedingTime: time.Minute}, withTotal(800))
	require.NoError(t, h.engine.Remove(t.Context(), hashOf(d.ID))) // Stats now fails

	f, ok, err := h.runner.Facts(t.Context(), d.ID)

	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, f.Registered)
	assert.False(t, f.Live, "no live numbers")
	assert.Equal(t, d.Name, f.Name)
	assert.Equal(t, int64(800), f.Size, "the Download's size")
	assert.Equal(t, int64(400), f.Uploaded)
	assert.Equal(t, time.Minute, f.SeedingTime)
	assert.InDelta(t, 0.5, f.Ratio, 1e-9)
	assert.Zero(t, f.UpRate+f.DownRate)
	assert.Empty(t, f.Files)
}

func TestFactsFallBackToTheDownloadBeforeTheEngineKnowsTheSize(t *testing.T) {
	h := newHarness(t)
	d := h.registered(core.TorrentRecord{Uploaded: 250}, withTotal(1000))
	h.engine.SetStats(hashOf(d.ID), core.TorrentStats{Uploaded: 250, UpRate: 3})

	f, ok, err := h.runner.Facts(t.Context(), d.ID)

	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, f.Live)
	assert.Equal(t, d.Name, f.Name, "the engine has no name yet")
	assert.Equal(t, int64(1000), f.Size)
	assert.Equal(t, int64(500), f.Uploaded)
	assert.InDelta(t, 0.5, f.Ratio, 1e-9)
	assert.Equal(t, int64(3), f.UpRate)
}

func TestFactsOfAnUnregisteredTorrentComeFromItsStoredRecord(t *testing.T) {
	h := newHarness(t)
	d := h.addSeeding(withTotal(2000))
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{
		DownloadID: d.ID, Uploaded: 1000, SeedingTime: time.Hour,
		RatioLimit: ptr(2.0), SeedingTimeLimit: ptr(30 * time.Minute), Location: "/complete/x", Moved: true,
	}))
	h.build() // not recovered: the engine does not have it

	f, ok, err := h.runner.Facts(t.Context(), d.ID)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, hashOf(d.ID), f.Hash)
	assert.False(t, f.Registered)
	assert.False(t, f.Live)
	assert.True(t, f.Fetched)
	assert.Equal(t, d.Name, f.Name)
	assert.Equal(t, int64(2000), f.Size)
	assert.Equal(t, int64(1000), f.Uploaded)
	assert.Equal(t, time.Hour, f.SeedingTime)
	assert.InDelta(t, 0.5, f.Ratio, 1e-9)
	require.NotNil(t, f.OwnRatioLimit)
	assert.InDelta(t, 2.0, *f.OwnRatioLimit, 1e-9)
	require.NotNil(t, f.OwnSeedingTimeLimit)
	assert.Equal(t, 30*time.Minute, *f.OwnSeedingTimeLimit)
	assert.Equal(t, "/complete/x", f.Dir, "where the move to complete put it")
}

func TestFactsOfAnUnregisteredTorrentWithoutARecordHaveNoCounters(t *testing.T) {
	h := newHarness(t)
	d := h.addTorrent(withTotal(0))
	h.build()

	f, ok, err := h.runner.Facts(t.Context(), d.ID)

	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, f.Fetched)
	assert.Zero(t, f.Uploaded)
	assert.Zero(t, f.Ratio, "no size, no ratio")
	assert.Empty(t, f.Dir, "not moved")
	assert.Nil(t, f.OwnRatioLimit)
}

func TestFactsOfADownloadWithoutATorrentAreNotOK(t *testing.T) {
	h := newHarness(t)
	h.build()

	_, ok, err := h.runner.Facts(t.Context(), core.NewID())

	require.NoError(t, err)
	assert.False(t, ok)
}

func TestUploadRateSumsEveryTorrentInTheEngine(t *testing.T) {
	h := newHarness(t)
	a, b := h.addSeeding(), h.addSeeding()
	h.addSeeding(withState(core.StateQueued)) // not in the engine
	h.build()
	require.NoError(t, h.runner.Recover(t.Context()))
	h.engine.SetStats(hashOf(a.ID), core.TorrentStats{UpRate: 10})
	h.engine.SetStats(hashOf(b.ID), core.TorrentStats{UpRate: 5})

	assert.Equal(t, int64(15), h.runner.UploadRate())
}
