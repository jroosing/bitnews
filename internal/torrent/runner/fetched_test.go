package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// destUnder returns a Destination that puts every Download in
// <complete>/<name>.
func (h *harness) destUnder(root string) {
	h.dest = func(_ context.Context, d core.Download) (string, error) { return filepath.Join(root, d.Name), nil }
}

func (h *harness) fetchedEvent(id core.ID) {
	h.engine.SetStats(hashOf(id), core.TorrentStats{State: core.TorrentSeeding, Size: 1000, DoneBytes: 1000})
	h.engine.Emit(core.TorrentEvent{Kind: core.TorrentFetched, Hash: hashOf(id)})
}

func (h *harness) record(id core.ID) core.TorrentRecord {
	h.t.Helper()
	rec, err := h.records.Get(context.Background(), id)
	require.NoError(h.t, err)
	return rec
}

func TestFetchedPersistsTheStateKeepsSeedingAndMovesToTheCompleteLocation(t *testing.T) {
	h := newHarness(t)
	h.destUnder(h.complete)
	a := h.addTorrent(withName("Show.S01"))
	sub := h.bus.Subscribe(t.Context())
	h.start()
	h.waitState(a.ID, core.StateFetching)

	h.fetchedEvent(a.ID)
	h.waitState(a.ID, core.StateFetched)

	d := h.get(a.ID)
	assert.False(t, d.FetchedAt.IsZero())
	assert.EqualValues(t, 1000, d.DoneBytes)
	assert.Equal(t, core.Fetched{ID: a.ID}, waitEventOf[core.Fetched](t, sub))
	target := filepath.Join(h.complete, "Show.S01")
	relocates := h.callsFor("Relocate", a.ID)
	require.Len(t, relocates, 1)
	assert.Equal(t, target, relocates[0].Dir)
	assert.Equal(t, target, h.engine.StorageDir(hashOf(a.ID)))
	assert.Empty(t, h.callsFor("Remove", a.ID), "seeding goes on")
	assert.Empty(t, h.callsFor("Pause", a.ID))
	rec := h.record(a.ID)
	assert.True(t, rec.Moved)
	assert.Equal(t, target, rec.Location)
	src, err := h.sources.Get(t.Context(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(target, "source.torrent"), src.TorrentPath, "the .torrent file travelled with the directory")
}

func TestTheDownloadIsFetchedOnlyAfterTheMoveSoPostProcessingNeverRacesIt(t *testing.T) {
	h := newHarness(t)
	h.destUnder(h.complete)
	a := h.addTorrent(withName("Slow"))
	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.engine.SetBlock("Relocate")

	h.fetchedEvent(a.ID)
	h.eventually(func() bool { return len(h.callsFor("Relocate", a.ID)) == 1 }, "move started")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, core.StateFetching, h.get(a.ID).State, "not Fetched while the data is moving")

	h.engine.Unblock("Relocate")
	h.waitState(a.ID, core.StateFetched)
}

func TestNoMoveWhenTheDirectoryIsTheDestination(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent(withName("Same"))
	h.dest = func(context.Context, core.Download) (string, error) {
		return filepath.Join(h.incomplete, string(a.ID)), nil
	}
	h.start()
	h.waitState(a.ID, core.StateFetching)

	h.fetchedEvent(a.ID)
	h.waitState(a.ID, core.StateFetched)

	assert.Empty(t, h.calls("Relocate"))
}

func TestNoDestinationMeansNoMove(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)

	h.fetchedEvent(a.ID)
	h.waitState(a.ID, core.StateFetched)

	assert.Empty(t, h.calls("Relocate"))
}

func TestMoveTargetGetsAUniqueNameWhenTheDestinationIsTaken(t *testing.T) {
	h := newHarness(t)
	h.destUnder(h.complete)
	a := h.addTorrent(withName("Taken"))
	require.NoError(t, os.MkdirAll(filepath.Join(h.complete, "Taken"), 0o755))
	h.start()
	h.waitState(a.ID, core.StateFetching)

	h.fetchedEvent(a.ID)
	h.waitState(a.ID, core.StateFetched)

	require.Len(t, h.calls("Relocate"), 1)
	assert.Equal(t, filepath.Join(h.complete, "Taken.1"), h.calls("Relocate")[0].Dir)
}

func TestFailedRelocateIsNotFatalAndTheTorrentKeepsSeedingWhereItWas(t *testing.T) {
	h := newHarness(t)
	h.destUnder(h.complete)
	a := h.addTorrent(withName("Stuck"))
	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.engine.FailNext("Relocate", errors.New("device busy"))

	h.fetchedEvent(a.ID)
	h.waitState(a.ID, core.StateFetched)

	assert.Equal(t, filepath.Join(h.incomplete, string(a.ID)), h.engine.StorageDir(hashOf(a.ID)))
	assert.Empty(t, h.callsFor("Remove", a.ID))
	rec, err := h.records.Get(t.Context(), a.ID)
	if err == nil {
		assert.False(t, rec.Moved)
		assert.Empty(t, rec.Location, "no pending move is left behind")
	}
	assert.Contains(t, h.logs.String(), "device busy")
}

func TestFetchedIsDetectedFromTheStatsWhenTheEventWasLost(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)

	h.engine.SetStats(hashOf(a.ID), core.TorrentStats{State: core.TorrentSeeding, Size: 1000, DoneBytes: 1000})

	h.waitState(a.ID, core.StateFetched)
}

func TestFetchedWhileQueuedStillEndsFetched(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Queues.Torrent = 1 })
	a := h.addTorrent(withPosition(0))
	b := h.addTorrent(withPosition(1))
	h.start()
	h.waitState(a.ID, core.StateFetching)
	// b is Queued; its torrent is not in the engine, so nothing to lose here.

	h.fetchedEvent(a.ID)
	h.waitState(a.ID, core.StateFetched)
	h.waitState(b.ID, core.StateFetching) // the slot is free: seeding does not count
}

func TestRestartAfterTheMoveAddsTheTorrentAtItsNewLocationAndDoesNotMoveAgain(t *testing.T) {
	h := newHarness(t)
	h.destUnder(h.complete)
	a := h.addTorrent(withName("Moved"), withState(core.StateFetching))
	target := filepath.Join(h.complete, "Moved")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(target, "source.torrent"), torrentBytes(a.ID), 0o600))
	require.NoError(t, os.RemoveAll(filepath.Join(h.incomplete, string(a.ID))))
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, Moved: true, Location: target}))
	require.NoError(t, h.sources.SetTorrentPath(t.Context(), a.ID, filepath.Join(target, "source.torrent")))
	h.build()
	require.NoError(t, h.runner.Recover(t.Context()))
	h.startRunnerBuilt()
	h.waitState(a.ID, core.StateFetching)

	h.fetchedEvent(a.ID)
	h.waitState(a.ID, core.StateFetched)

	assert.Equal(t, target, h.calls("Add")[0].Spec.StorageDir)
	assert.Empty(t, h.calls("Relocate"))
}

func TestRestartInTheMiddleOfAMoveFindsTheDataWhereverItIs(t *testing.T) {
	for name, tc := range map[string]struct {
		dataAtOld bool
		wantDir   func(h *harness, id core.ID, target string) string
		wantMoved bool
	}{
		"the move did not happen": {true, func(h *harness, id core.ID, _ string) string { return filepath.Join(h.incomplete, string(id)) }, false},
		"the move completed":      {false, func(_ *harness, _ core.ID, target string) string { return target }, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			a := h.addTorrent(withName("Pending"), withState(core.StateFetching))
			old := filepath.Join(h.incomplete, string(a.ID))
			target := filepath.Join(h.complete, "Pending")
			if !tc.dataAtOld {
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
				require.NoError(t, os.Rename(old, target))
			}
			require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, Location: target}))
			h.build()
			require.NoError(t, h.runner.Recover(t.Context()))
			h.startRunnerBuilt()
			h.waitState(a.ID, core.StateFetching)

			assert.Equal(t, tc.wantDir(h, a.ID, target), h.calls("Add")[0].Spec.StorageDir)
			h.eventually(func() bool { return h.record(a.ID).Moved == tc.wantMoved }, "the record is settled")
			if tc.wantMoved {
				src, err := h.sources.Get(t.Context(), a.ID)
				require.NoError(t, err)
				assert.Equal(t, filepath.Join(target, "source.torrent"), src.TorrentPath)
			} else {
				assert.Empty(t, h.record(a.ID).Location, "the unfinished move is forgotten")
			}
		})
	}
}
