package app_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/core/coretest"
	"github.com/jroosing/bitnews/internal/store"
)

// liveSettings is a SettingsReader whose CleanNames option can change
// between two adds.
type liveSettings struct {
	torrentSettings
	cleanNames atomic.Bool
}

func (l *liveSettings) Current() core.Settings {
	st := l.torrentSettings.Current()
	st.PostProcess.CleanNames = l.cleanNames.Load()
	return st
}

func newCleanRig(t *testing.T) (*app.AddService, *liveSettings, *stack) {
	t.Helper()
	s := newStack(t)
	base := t.TempDir()
	live := &liveSettings{torrentSettings: torrentSettings{filepath.Join(base, "usenet"), filepath.Join(base, "torrents")}}
	key := store.GenerateKey()
	ds := app.NewDownloadService(s.downloads, s.cats, s.bus)
	svc := app.NewAddService(ds, store.NewDownloadSourceRepo(s.db, key), live,
		app.WithClock(s.clock.Now), app.WithTorrentSources(store.NewTorrentSourceRepo(s.db, key)))
	return svc, live, s
}

func addNZB(t *testing.T, svc *app.AddService, req app.AddRequest) core.Download {
	t.Helper()
	st, err := svc.Stage(context.Background(), strings.NewReader(tinyNZB))
	require.NoError(t, err)
	d, err := svc.Add(context.Background(), st, req)
	require.NoError(t, err)
	return d
}

const dirtyName = "Some.Show.S01E01.1080p-GRP-Obfuscated"
const cleanedName = "Some.Show.S01E01.1080p-GRP"

func TestAddCleansTheNZBNameWhenCleanNamesIsOn(t *testing.T) {
	svc, live, s := newCleanRig(t)
	live.cleanNames.Store(true)

	d := addNZB(t, svc, app.AddRequest{Name: dirtyName})

	assert.Equal(t, cleanedName, d.Name)
	stored, err := s.downloads.Get(context.Background(), d.ID)
	require.NoError(t, err)
	assert.Equal(t, cleanedName, stored.Name)
	assert.FileExists(t, filepath.Join(live.usenet, string(d.ID), cleanedName+".nzb"), "the stored NZB file follows the cleaned name")
}

func TestAddKeepsTheNZBNameWhenCleanNamesIsOff(t *testing.T) {
	svc, _, _ := newCleanRig(t)

	d := addNZB(t, svc, app.AddRequest{Name: dirtyName})

	assert.Equal(t, dirtyName, d.Name)
}

func TestAddReadsCleanNamesAtEveryAdd(t *testing.T) {
	svc, live, _ := newCleanRig(t)

	assert.Equal(t, dirtyName, addNZB(t, svc, app.AddRequest{Name: dirtyName}).Name)
	live.cleanNames.Store(true)
	assert.Equal(t, cleanedName, addNZB(t, svc, app.AddRequest{Name: dirtyName}).Name)
	live.cleanNames.Store(false)
	assert.Equal(t, dirtyName, addNZB(t, svc, app.AddRequest{Name: dirtyName}).Name)
}

func TestAddCleansTheNameTakenFromTheFileNameToo(t *testing.T) {
	svc, live, _ := newCleanRig(t)
	live.cleanNames.Store(true)

	d := addNZB(t, svc, app.AddRequest{FileName: dirtyName + ".nzb"})

	assert.Equal(t, cleanedName, d.Name)
}

func TestAddTorrentNeverCleansTheNameEvenWhenCleanNamesIsOn(t *testing.T) {
	ctx := context.Background()
	svc, live, _ := newCleanRig(t)
	live.cleanNames.Store(true)
	data, _ := coretest.BuildTorrent(dirtyName, showFiles)

	st, err := svc.StageTorrent(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	d, err := svc.AddTorrent(ctx, st, app.AddRequest{})

	require.NoError(t, err)
	assert.Equal(t, dirtyName, d.Name)
}

func TestAddMagnetNeverCleansTheNameEvenWhenCleanNamesIsOn(t *testing.T) {
	ctx := context.Background()
	svc, live, _ := newCleanRig(t)
	live.cleanNames.Store(true)

	st, err := svc.StageMagnet(ctx, "magnet:?xt=urn:btih:"+hexHash+"&dn="+dirtyName)
	require.NoError(t, err)
	d, err := svc.AddTorrent(ctx, st, app.AddRequest{})

	require.NoError(t, err)
	assert.Equal(t, dirtyName, d.Name)
}
