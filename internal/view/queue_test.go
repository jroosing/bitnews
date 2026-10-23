package view_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/view"
)

func TestQueueListsTheInQueueDownloadsOfAKindInQueueOrder(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := store.NewDownloadRepo(db)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	add := func(id core.ID, state core.State, kind core.SourceKind, pos int) {
		require.NoError(t, repo.Create(ctx, core.Download{
			ID: id, Name: string(id), State: state, SourceKind: kind, QueuePosition: pos,
			Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at,
			FetchedAt: map[bool]time.Time{true: at}[state == core.StateReady],
		}))
	}
	add("third", core.StateQueued, core.SourceNZB, 2)
	add("first", core.StateFetching, core.SourceNZB, 0)
	add("second", core.StateQueued, core.SourceNZB, 1)
	add("done", core.StateReady, core.SourceNZB, 3)
	add("torrent", core.StateQueued, core.SourceTorrent, 0)
	m, err := view.New(view.Config{Downloads: repo, Live: fakeLive{}})
	require.NoError(t, err)

	got, err := m.Queue(ctx, core.SourceNZB)

	require.NoError(t, err)
	var ids []core.ID
	for _, v := range got {
		ids = append(ids, v.Download.ID)
	}
	assert.Equal(t, []core.ID{"first", "second", "third"}, ids)
	assert.Equal(t, view.PhaseDownloading, got[0].Phase)
}

func TestQueueNeedsTheDownloadRepo(t *testing.T) {
	m := newModule(t, fakeLive{})

	_, err := m.Queue(context.Background(), core.SourceNZB)

	assert.Error(t, err)
}

func TestLabelsAreTheWordsTheUIShows(t *testing.T) {
	assert.Equal(t, "Downloading", view.PhaseDownloading.Label())
	assert.Equal(t, "Unpacking", view.PhaseUnpacking.Label())
	assert.Equal(t, "Queued", view.PhaseWaiting.Label())
	assert.Equal(t, "Fetching metadata", view.PhaseMetadata.Label())
	assert.Equal(t, "Force", view.PriorityLabel(core.DownloadPriorityForce))
	assert.Equal(t, "Normal", view.PriorityLabel(""))
	assert.Equal(t, "Low", view.PriorityLabel(core.DownloadPriorityLow))
}
