package runner_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Many torrents finish while the limit changes, the Queue pauses and
// Downloads are paused, held and removed. Run with -race.
func TestStressManyTorrentsWhileTheQueueIsPokedFromAllSides(t *testing.T) {
	h := newHarness(t)
	h.destUnder(h.complete)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 5; s.Seeding.RatioLimit = 100 })
	const n = 40
	var all []core.Download
	for range n {
		all = append(all, h.addTorrent())
	}
	h.start()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() { // finishes whatever is running
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, d := range all {
				if h.get(d.ID).State == core.StateFetching {
					h.engine.SetStats(hashOf(d.ID), core.TorrentStats{State: core.TorrentSeeding, Size: 1000, DoneBytes: 1000})
					h.engine.Emit(core.TorrentEvent{Kind: core.TorrentFetched, Hash: hashOf(d.ID)})
				}
			}
			time.Sleep(time.Millisecond)
		}
	})
	wg.Go(func() { // pokes the Queue
		for i := range 30 {
			h.runner.PauseQueue()
			time.Sleep(2 * time.Millisecond)
			h.runner.ResumeQueue()
			h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 1 + i%7 })
			h.bus.Publish(core.SettingsChanged{})
			h.runner.SetRateLimits(int64(i)*100, 0)
		}
		h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 8 })
		h.bus.Publish(core.SettingsChanged{})
	})
	wg.Go(func() { // holds and releases
		for i := range 60 {
			id := all[i%n].ID
			_ = h.runner.Hold(t.Context(), id)
			h.runner.Release(id)
		}
	})
	wg.Go(func() { // sets limits
		for i := range 60 {
			_ = h.runner.SetShareLimit(t.Context(), all[i%n].ID, core.ShareLimit{Ratio: 100, SeedingTime: -1})
		}
	})

	h.eventually(func() bool {
		for _, d := range all {
			if h.get(d.ID).State != core.StateFetched {
				return false
			}
		}
		return true
	}, "every torrent ends Fetched")
	close(stop)
	wg.Wait()

	for _, d := range all {
		assert.True(t, h.runner.Managed(d.ID), "%s keeps seeding", d.Name)
		rec, err := h.records.Get(t.Context(), d.ID)
		require.NoError(t, err)
		assert.True(t, rec.Moved, "%s was moved", d.Name)
	}
}
