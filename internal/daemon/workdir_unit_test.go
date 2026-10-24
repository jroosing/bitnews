package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

func TestWorkDirIsTheStoredNZBsDirAndStaysSoWhenTheSettingsChange(t *testing.T) {
	db, key := openDB(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	d := core.Download{ID: core.NewID(), Name: "x", SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at}
	require.NoError(t, store.NewDownloadRepo(db).Create(ctx, d))
	sources := store.NewDownloadSourceRepo(db, key)
	require.NoError(t, sources.Create(ctx, d.ID, "/old/incomplete/"+string(d.ID)+"/x.nzb", ""))
	s := core.DefaultSettings()
	s.Paths.Usenet.Incomplete = "/old/incomplete"
	w := newWorkDirs(sources, func() core.Settings { return s }, slog.New(slog.DiscardHandler))

	assert.Equal(t, "/old/incomplete/"+string(d.ID), w.dirFor(d))
	s.Paths.Usenet.Incomplete = "/new/incomplete"
	assert.Equal(t, "/old/incomplete/"+string(d.ID), w.dirFor(d), "cached, and the stored path wins anyway")
	w.forget(d.ID)
	assert.Equal(t, "/old/incomplete/"+string(d.ID), w.dirFor(d), "a fresh lookup finds the stored path")
}

func TestWorkDirFallsBackToTheSettingsWithoutASourceAndDoesNotCacheIt(t *testing.T) {
	db, key := openDB(t)
	s := core.DefaultSettings()
	s.Paths.Usenet.Incomplete = "/one"
	w := newWorkDirs(store.NewDownloadSourceRepo(db, key), func() core.Settings { return s }, slog.New(slog.DiscardHandler))
	d := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB}

	assert.Equal(t, filepath.Join("/one", string(d.ID)), w.dirFor(d))
	s.Paths.Usenet.Incomplete = "/two"
	assert.Equal(t, filepath.Join("/two", string(d.ID)), w.dirFor(d))
}
