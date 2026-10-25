package torrent_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/torrent"
)

// The limiters start with a burst of up to the limit itself (never below
// 64 KiB), so the bytes beyond it set the pace.
func expectedSeconds(size, rate int64) float64 {
	burst := min(max(rate, 64<<10), 1<<20)
	return float64(size-burst) / float64(rate)
}

func TestDownloadRateLimitIsHonouredWithinAQuarter(t *testing.T) {
	t.Parallel()
	const size, rate = 500_000, 100_000
	c := makeContent(t, "Rate.Down", map[string]int{"f": size})
	seeder := seed(t, c)
	leech := newEngine(t)
	require.NoError(t, leech.SetRateLimits(rate, 0))
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: t.TempDir()})
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)
	took := time.Since(start).Seconds()

	want := expectedSeconds(size, rate)
	assert.InDelta(t, want, took, want*0.25+0.5, "took %.1fs, expected about %.1fs", took, want)
}

func TestUploadRateLimitIsHonouredWithinAQuarter(t *testing.T) {
	t.Parallel()
	const size, rate = 500_000, 100_000
	c := makeContent(t, "Rate.Up", map[string]int{"f": size})
	seeder := seed(t, c)
	require.NoError(t, seeder.SetRateLimits(0, rate))
	leech := newEngine(t)
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: t.TempDir()})
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)
	took := time.Since(start).Seconds()

	want := expectedSeconds(size, rate)
	assert.InDelta(t, want, took, want*0.25+0.5, "took %.1fs, expected about %.1fs", took, want)
}

func TestRateLimitsCanBeLiftedAtRuntime(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Rate.Lift", map[string]int{"f": 400_000})
	seeder := seed(t, c)
	leech := newEngine(t)
	require.NoError(t, leech.SetRateLimits(20_000, 20_000))
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	time.Sleep(500 * time.Millisecond)
	st, _ := leech.Stats(h)
	assert.Less(t, st.DoneBytes, int64(300_000), "still crawling")

	require.NoError(t, leech.SetRateLimits(0, -1)) // zero or less: unlimited

	waitEvent(t, leech, core.TorrentFetched, h)
}

// clock is a fake time source.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *clock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// seedWithLeech seeds c on an engine using clk and downloads it once from
// there, so the seeder has uploaded about one copy.
func seedWithLeech(t *testing.T, c content, clk *clock) *torrent.Engine {
	t.Helper()
	seeder := seed(t, c, func(cfg *torrent.Config) { cfg.Now = clk.Now })
	leech := newEngine(t)
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)
	return seeder
}

func TestStatsRatioCountsWhatWasUploaded(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Ratio", map[string]int{"f": 100_000})
	seeder := seedWithLeech(t, c, newClock())

	st, err := seeder.Stats(c.hash)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, st.Ratio, 1.0)
	assert.Equal(t, core.TorrentSeeding, st.State)
}

func TestStatsSeedingTimeUsesTheClockAndOnlyCountsWhileSeeding(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Time", map[string]int{"f": 10_000})
	clk := newClock()
	seeder := seed(t, c, func(cfg *torrent.Config) { cfg.Now = clk.Now })

	clk.Advance(59 * time.Minute)
	st, _ := seeder.Stats(c.hash)
	assert.Equal(t, 59*time.Minute, st.SeedingTime.Round(time.Second))

	// A paused hour does not count.
	require.NoError(t, seeder.Pause(c.hash))
	clk.Advance(3 * time.Hour)
	require.NoError(t, seeder.Resume(c.hash))

	clk.Advance(2 * time.Minute)
	st, _ = seeder.Stats(c.hash)
	assert.Equal(t, 61*time.Minute, st.SeedingTime.Round(time.Second))
}
