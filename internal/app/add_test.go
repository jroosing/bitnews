package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

const tinyNZB = `<nzb><file subject="&quot;a.rar&quot;"><segments><segment bytes="10" number="1">a@b</segment></segments></file></nzb>`

type settingsAt struct{ incomplete string }

func (s settingsAt) Current() core.Settings {
	st := core.DefaultSettings()
	st.Paths.Usenet.Incomplete = s.incomplete
	return st
}

type failingSources struct{ err error }

func (f failingSources) Create(context.Context, core.ID, string, core.Secret) error { return f.err }
func (f failingSources) Get(context.Context, core.ID) (string, core.Secret, error) {
	return "", "", f.err
}

type failingDownloads struct {
	core.DownloadRepo
	err error
}

func (f failingDownloads) Create(context.Context, core.Download) error { return f.err }

func entries(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err == nil && p != root {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestAddRemovesTheDownloadAndFilesWhenTheSourceCannotBeStored(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	root := filepath.Join(t.TempDir(), "incomplete")
	ds := app.NewDownloadService(s.downloads, s.cats, s.bus)
	svc := app.NewAddService(ds, failingSources{errors.New("disk on fire")}, settingsAt{root})
	st, err := svc.Stage(ctx, strings.NewReader(tinyNZB))
	require.NoError(t, err)

	_, err = svc.Add(ctx, st, app.AddRequest{})

	require.Error(t, err)
	list, _, lerr := s.downloads.List(ctx, core.DownloadQuery{})
	require.NoError(t, lerr)
	assert.Empty(t, list, "no half-added Download")
	assert.Empty(t, entries(t, root))
	assert.Empty(t, s.events())
}

func TestAddRemovesTheFilesWhenTheDownloadCannotBeStored(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	root := filepath.Join(t.TempDir(), "incomplete")
	repo := failingDownloads{DownloadRepo: s.downloads, err: errors.New("db gone")}
	ds := app.NewDownloadService(repo, s.cats, s.bus)
	svc := app.NewAddService(ds, failingSources{}, settingsAt{root})
	st, err := svc.Stage(ctx, strings.NewReader(tinyNZB))
	require.NoError(t, err)

	_, err = svc.Add(ctx, st, app.AddRequest{})

	require.Error(t, err)
	assert.Empty(t, entries(t, root))
}

func TestDiscardRemovesAStagedNZBAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	root := filepath.Join(t.TempDir(), "incomplete")
	svc := app.NewAddService(app.NewDownloadService(s.downloads, s.cats, s.bus), failingSources{}, settingsAt{root})
	st, err := svc.Stage(ctx, strings.NewReader(tinyNZB))
	require.NoError(t, err)
	require.NotEmpty(t, entries(t, root))

	st.Discard()
	st.Discard()

	assert.Empty(t, entries(t, root))
}

func TestStageCreatesTheIncompleteRootAndItsParents(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	root := filepath.Join(t.TempDir(), "a", "b", "incomplete")
	svc := app.NewAddService(app.NewDownloadService(s.downloads, s.cats, s.bus), failingSources{}, settingsAt{root})

	st, err := svc.Stage(ctx, strings.NewReader(tinyNZB))
	require.NoError(t, err)
	defer st.Discard()

	info, err := os.Stat(root)
	require.NoError(t, err, "the parent is created too")
	assert.True(t, info.IsDir())
}

// hookedSources runs before on every Create, then stores for real.
type hookedSources struct {
	core.DownloadSourceRepo
	before func(id core.ID)
}

func (h hookedSources) Create(ctx context.Context, id core.ID, path string, pw core.Secret) error {
	h.before(id)
	return h.DownloadSourceRepo.Create(ctx, id, path, pw)
}

// failingTransition fails the move to Queued.
type failingTransition struct {
	core.DownloadRepo
}

func (failingTransition) UpdateState(context.Context, core.ID, core.StateChange) (core.Download, error) {
	return core.Download{}, errors.New("db hiccup")
}

func TestAddNeverShowsAQueuedDownloadBeforeItsSourceIsStored(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	root := filepath.Join(t.TempDir(), "incomplete")
	sources := hookedSources{DownloadSourceRepo: store.NewDownloadSourceRepo(s.db, store.GenerateKey())}
	var seenState core.State
	sources.before = func(id core.ID) {
		queued, _, err := s.downloads.List(ctx, core.DownloadQuery{State: core.StateQueued})
		require.NoError(t, err)
		for _, q := range queued {
			assert.NotEqual(t, id, q.ID, "visible as Queued before its source exists")
		}
		got, err := s.downloads.Get(ctx, id)
		require.NoError(t, err, "the Download row exists before the source (foreign key)")
		seenState = got.State
	}
	svc := app.NewAddService(app.NewDownloadService(s.downloads, s.cats, s.bus), sources, settingsAt{root}, app.WithClock(s.clock.Now))
	st, err := svc.Stage(ctx, strings.NewReader(tinyNZB))
	require.NoError(t, err)

	d, err := svc.Add(ctx, st, app.AddRequest{Paused: true, Priority: core.DownloadPriorityHigh})

	require.NoError(t, err)
	assert.Equal(t, core.StateAdded, seenState)
	stored, err := s.downloads.Get(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, core.StateQueued, stored.State)
	assert.True(t, stored.Paused, "a paused add stays paused in Queued")
	assert.Equal(t, core.DownloadPriorityHigh, stored.Priority)
	assert.Equal(t, stored, d, "the returned Download is the stored one")
	evs := s.events()
	require.Len(t, evs, 1)
	assert.Equal(t, core.DownloadAdded{Download: stored}, evs[0], "announced only once Queued")
}

func TestAddTheQueuedTransitionFailingRemovesEverything(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	root := filepath.Join(t.TempDir(), "incomplete")
	sources := store.NewDownloadSourceRepo(s.db, store.GenerateKey())
	ds := app.NewDownloadService(failingTransition{s.downloads}, s.cats, s.bus)
	svc := app.NewAddService(ds, sources, settingsAt{root})
	st, err := svc.Stage(ctx, strings.NewReader(tinyNZB))
	require.NoError(t, err)

	_, err = svc.Add(ctx, st, app.AddRequest{Password: "pw"})

	require.Error(t, err)
	list, _, lerr := s.downloads.List(ctx, core.DownloadQuery{})
	require.NoError(t, lerr)
	assert.Empty(t, list, "the Download row is gone")
	var n int
	require.NoError(t, s.db.Reader().QueryRow(`SELECT count(*) FROM download_sources`).Scan(&n))
	assert.Zero(t, n, "and so is its source")
	assert.Empty(t, entries(t, root))
	assert.Empty(t, s.events())
}

func TestAddQueuePositionsCountDownloadsStillBeingAdded(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	root := filepath.Join(t.TempDir(), "incomplete")
	sources := store.NewDownloadSourceRepo(s.db, store.GenerateKey())
	svc := app.NewAddService(app.NewDownloadService(s.downloads, s.cats, s.bus), sources, settingsAt{root})

	var positions []int
	for range 3 {
		st, err := svc.Stage(ctx, strings.NewReader(tinyNZB))
		require.NoError(t, err)
		d, err := svc.Add(ctx, st, app.AddRequest{})
		require.NoError(t, err)
		positions = append(positions, d.QueuePosition)
	}

	assert.Equal(t, []int{0, 1, 2}, positions)
}

func TestStageIntoAnUnwritableFolderIsRefusedNamingTheFolderAndTheUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	ctx := context.Background()
	s := newStack(t)
	root := filepath.Join(t.TempDir(), "incomplete")
	require.NoError(t, os.Mkdir(root, 0o555))
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	svc := app.NewAddService(app.NewDownloadService(s.downloads, s.cats, s.bus), failingSources{}, settingsAt{root})

	_, err := svc.Stage(ctx, strings.NewReader(tinyNZB))

	var ae *app.Error
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, "folder_not_writable", ae.Code)
	assert.ErrorIs(t, err, app.ErrUnavailable)
	assert.Contains(t, ae.Message, root)
	assert.Contains(t, ae.Message, fmt.Sprintf("uid %d", os.Geteuid()))
}
