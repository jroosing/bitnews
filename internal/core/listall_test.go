package core_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

func TestListAllReadsEveryPage(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := store.NewDownloadRepo(db)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		require.NoError(t, repo.Create(ctx, core.Download{
			ID: core.NewID(), Name: fmt.Sprint(i), SourceKind: core.SourceNZB, State: core.StateQueued,
			Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at,
		}))
	}

	all, err := core.ListAll(ctx, repo, core.DownloadQuery{State: core.StateQueued, Limit: 2})
	require.NoError(t, err)
	assert.Len(t, all, 5, "three pages of two")

	none, err := core.ListAll(ctx, repo, core.DownloadQuery{State: core.StateReady})
	require.NoError(t, err)
	assert.Empty(t, none)
}
