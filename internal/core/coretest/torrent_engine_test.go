package coretest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/core/coretest"
)

var _ core.TorrentEngine = (*coretest.FakeTorrentEngine)(nil)

const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x"

func TestFakeAddRecordsTheSpecAndUsesTheMagnetHash(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	spec := core.TorrentSpec{Magnet: magnet, StorageDir: "/data/x", Paused: true}

	h, err := f.Add(context.Background(), spec)

	require.NoError(t, err)
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", h.String())
	assert.Equal(t, []coretest.TorrentCall{{Method: "Add", Hash: h, Spec: spec}}, f.Calls())
	st, err := f.Stats(h)
	require.NoError(t, err)
	assert.Equal(t, core.TorrentPaused, st.State)
}

func TestFakeAddMetainfoHashIsStableAndDuplicateConflicts(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	spec := core.TorrentSpec{Metainfo: []byte("meta"), StorageDir: "/d"}

	h1, err := f.Add(context.Background(), spec)
	require.NoError(t, err)
	h2, err := f.Add(context.Background(), spec)

	assert.ErrorIs(t, err, core.ErrConflict)
	assert.Equal(t, h1, h2)
}

func TestFakeRejectsInvalidSpec(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	_, err := f.Add(context.Background(), core.TorrentSpec{})
	assert.ErrorIs(t, err, core.ErrInvalidTorrentSpec)
}

func TestFakeUnknownHashIsNotFound(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	var h core.InfoHash
	assert.ErrorIs(t, f.Pause(h), core.ErrNotFound)
	assert.ErrorIs(t, f.Resume(h), core.ErrNotFound)
	assert.ErrorIs(t, f.Remove(context.Background(), h, true), core.ErrNotFound)
	assert.ErrorIs(t, f.Relocate(context.Background(), h, "/x"), core.ErrNotFound)
	assert.ErrorIs(t, f.SetShareLimits(h, core.UnlimitedShareLimit()), core.ErrNotFound)
	_, err := f.Stats(h)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestFakePauseResumeRemoveRelocateChangeState(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	ctx := context.Background()
	h, err := f.Add(ctx, core.TorrentSpec{Magnet: magnet, StorageDir: "/a"})
	require.NoError(t, err)
	st, _ := f.Stats(h)
	assert.Equal(t, core.TorrentMetadataPending, st.State)

	f.SetStats(h, core.TorrentStats{State: core.TorrentDownloading, Size: 10})
	require.NoError(t, f.Pause(h))
	st, _ = f.Stats(h)
	assert.Equal(t, core.TorrentPaused, st.State)
	assert.EqualValues(t, 10, st.Size)

	require.NoError(t, f.Resume(h))
	st, _ = f.Stats(h)
	assert.Equal(t, core.TorrentDownloading, st.State)

	require.NoError(t, f.Relocate(ctx, h, "/b"))
	assert.Equal(t, "/b", f.StorageDir(h))

	require.NoError(t, f.Remove(ctx, h, true))
	_, err = f.Stats(h)
	assert.ErrorIs(t, err, core.ErrNotFound)
	last := f.Calls()[len(f.Calls())-1]
	assert.Equal(t, coretest.TorrentCall{Method: "Remove", Hash: h, DeleteData: true}, last)
}

func TestFakeRecordsLimits(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	h, _ := f.Add(context.Background(), core.TorrentSpec{Magnet: magnet, StorageDir: "/a"})

	require.NoError(t, f.SetShareLimits(h, core.ShareLimit{Ratio: 2, SeedingTime: -1}))
	require.NoError(t, f.SetRateLimits(100, 50))

	calls := f.Calls()
	assert.Equal(t, core.ShareLimit{Ratio: 2, SeedingTime: -1}, calls[1].Limit)
	assert.Equal(t, coretest.TorrentCall{Method: "SetRateLimits", Down: 100, Up: 50}, calls[2])
}

func TestFakeEmitDeliversEventsInOrder(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	var h core.InfoHash
	h[0] = 1

	f.Emit(core.TorrentEvent{Kind: core.TorrentMetadataReceived, Hash: h})
	f.Emit(core.TorrentEvent{Kind: core.TorrentFetched, Hash: h})

	assert.Equal(t, core.TorrentMetadataReceived, (<-f.Events()).Kind)
	assert.Equal(t, core.TorrentFetched, (<-f.Events()).Kind)
}

func TestFakeFailNextMakesAMethodFailOnce(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	boom := errors.New("boom")
	f.FailNext("Add", boom)

	_, err := f.Add(context.Background(), core.TorrentSpec{Magnet: magnet, StorageDir: "/a"})
	assert.ErrorIs(t, err, boom)

	_, err = f.Add(context.Background(), core.TorrentSpec{Magnet: magnet, StorageDir: "/a"})
	assert.NoError(t, err)
}

func TestFakeMetainfoReturnsWhatTheTorrentWasAddedWith(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	h, err := f.Add(context.Background(), core.TorrentSpec{Metainfo: []byte("meta"), StorageDir: "/d"})
	require.NoError(t, err)

	got, err := f.Metainfo(h)

	require.NoError(t, err)
	assert.Equal(t, []byte("meta"), got)
}

func TestFakeMetainfoOfAMagnetNeedsTheMetadataFirst(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	h, err := f.Add(context.Background(), core.TorrentSpec{Magnet: magnet, StorageDir: "/d"})
	require.NoError(t, err)

	_, err = f.Metainfo(h)
	require.Error(t, err)

	f.SetMetainfo(h, []byte("from the swarm"))
	got, err := f.Metainfo(h)
	require.NoError(t, err)
	assert.Equal(t, []byte("from the swarm"), got)
}

func TestFakeMetainfoOfAnUnknownTorrentIsNotFound(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()

	_, err := f.Metainfo(core.InfoHash{})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestFakeRelocateWaitsWhileBlocked(t *testing.T) {
	f := coretest.NewFakeTorrentEngine()
	h, err := f.Add(context.Background(), core.TorrentSpec{Metainfo: []byte("m"), StorageDir: "/old"})
	require.NoError(t, err)
	f.SetBlock("Relocate")
	done := make(chan error, 1)

	go func() { done <- f.Relocate(context.Background(), h, "/new") }()
	require.Eventually(t, func() bool { return len(f.Calls()) == 2 }, time.Second, time.Millisecond)

	assert.Equal(t, "/old", f.StorageDir(h), "still moving")
	f.Unblock("Relocate")
	require.NoError(t, <-done)
	assert.Equal(t, "/new", f.StorageDir(h))
}
