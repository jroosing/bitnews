package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// finished stores a Download in the given terminal state that finished at
// epoch + at.
func finished(t *testing.T, repo interface {
	Create(context.Context, core.Download) error
}, name string, st core.State, kind core.SourceKind, at time.Duration) core.Download {
	t.Helper()
	d := newDownload(name, 0)
	d.State, d.SourceKind = st, kind
	switch st {
	case core.StateReady:
		d.ReadyAt, d.OutputPath, d.UpdatedAt = epoch.Add(at), "/out/"+name, epoch.Add(at+time.Hour)
	case core.StateFailed:
		d.UpdatedAt, d.FailureReason = epoch.Add(at), core.FailureInternal
	}
	require.NoError(t, repo.Create(context.Background(), d))
	return d
}

func names(ds []core.Download) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}

func TestHistoryIsNewestFirstByWhenItFinishedAndPagesWithACursor(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	finished(t, repo, "a", core.StateReady, core.SourceNZB, 1*time.Minute)
	finished(t, repo, "b", core.StateFailed, core.SourceNZB, 3*time.Minute)
	finished(t, repo, "c", core.StateReady, core.SourceTorrent, 2*time.Minute) // updated later, finished earlier
	finished(t, repo, "q", core.StateQueued, core.SourceNZB, 9*time.Minute)

	first, err := repo.History(ctx, core.HistoryQuery{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "c"}, names(first.Items))
	assert.Equal(t, 3, first.Total, "the queued one is not history")
	assert.Zero(t, first.Newer)

	last := first.Items[1]
	second, err := repo.History(ctx, core.HistoryQuery{Limit: 2, BeforeAt: last.ReadyAt.UnixMilli(), BeforeID: last.ID})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, names(second.Items))
	assert.Equal(t, 2, second.Newer)
}

func TestHistoryFiltersByStateKindAndCategory(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDownloadRepo(t)
	finished(t, repo, "nzb-ready", core.StateReady, core.SourceNZB, time.Minute)
	finished(t, repo, "nzb-failed", core.StateFailed, core.SourceNZB, 2*time.Minute)
	finished(t, repo, "tor-ready", core.StateReady, core.SourceTorrent, 3*time.Minute)

	failed, err := repo.History(ctx, core.HistoryQuery{State: core.StateFailed})
	require.NoError(t, err)
	torrents, err := repo.History(ctx, core.HistoryQuery{SourceKind: core.SourceTorrent})
	require.NoError(t, err)

	assert.Equal(t, []string{"nzb-failed"}, names(failed.Items))
	assert.Equal(t, []string{"tor-ready"}, names(torrents.Items))
}
