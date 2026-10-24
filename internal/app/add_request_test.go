package app_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/core/coretest"
)

func TestReceiveTellsAnNZBFromATorrentByItsContent(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	data, _ := coretest.BuildTorrent("Some.Show.S01", showFiles)

	tor, err := r.svc.Receive(ctx, bytes.NewReader(data), "whatever.bin", "")
	require.NoError(t, err)
	defer tor.Discard()
	nzb, err := r.svc.Receive(ctx, strings.NewReader(tinyNZB), "", "")
	require.NoError(t, err)
	defer nzb.Discard()

	assert.Equal(t, core.SourceTorrent, tor.Kind())
	assert.Equal(t, core.SourceNZB, nzb.Kind())
}

func TestReceiveRefusesAFileWhoseExtensionLies(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	data, _ := coretest.BuildTorrent("x", showFiles)

	for _, tc := range []struct {
		name, body, code string
	}{
		{"a.torrent", tinyNZB, "not_a_torrent"},
		{"a.nzb", string(data), "not_an_nzb"},
		{"a.nzb", "", "empty_file"},
	} {
		_, err := r.svc.Receive(ctx, strings.NewReader(tc.body), tc.name, "")
		require.ErrorIs(t, err, app.ErrInvalidInput, tc.code)
		assert.Equal(t, tc.code, errCode(t, err))
	}
	assert.Empty(t, entries(t, r.usenet), "nothing is staged")
	assert.Empty(t, entries(t, r.torrents))
}

func TestAddTakesAReceivedFileAndUsesItsFileName(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	up, err := r.svc.Receive(ctx, strings.NewReader(`<nzb><file subject="x"><segments><segment bytes="10" number="1">a@b</segment></segments></file></nzb>`), "My.Release.nzb", core.SourceNZB)
	require.NoError(t, err)

	d, err := r.svc.Add(ctx, app.AddRequest{Upload: up})

	require.NoError(t, err)
	assert.Equal(t, "My.Release", d.Name)
	assert.Equal(t, core.SourceNZB, d.SourceKind)
}

func TestAddNeedsExactlyOneSource(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	_, err := r.svc.Add(ctx, app.AddRequest{})
	assert.Equal(t, "missing_source", errCode(t, err))

	up, err := r.svc.Receive(ctx, strings.NewReader(tinyNZB), "", core.SourceNZB)
	require.NoError(t, err)
	_, err = r.svc.Add(ctx, app.AddRequest{Upload: up, URL: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)})
	assert.Equal(t, "ambiguous_source", errCode(t, err))
	assert.Empty(t, entries(t, r.usenet), "the upload is removed")
}

func TestAddRemovesTheUploadWhenTheRequestIsRefused(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	up, err := r.svc.Receive(ctx, strings.NewReader(tinyNZB), "", "")
	require.NoError(t, err)

	_, err = r.svc.Add(ctx, app.AddRequest{Upload: up, Priority: "urgent"})

	assert.Equal(t, "invalid_priority", errCode(t, err))
	assert.Empty(t, entries(t, r.usenet))
}

func TestAddRefusesTheWrongKindWhenOnlyOneIsAccepted(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	up, err := r.svc.Receive(ctx, strings.NewReader(tinyNZB), "", "")
	require.NoError(t, err)

	_, err = r.svc.Add(ctx, app.AddRequest{Upload: up, Only: core.SourceTorrent})
	assert.Equal(t, "not_a_torrent", errCode(t, err))
	assert.Empty(t, entries(t, r.usenet))

	_, err = r.svc.Add(ctx, app.AddRequest{URL: magnetURI(""), Only: core.SourceNZB})
	assert.Equal(t, "not_an_nzb", errCode(t, err))
}

func TestAddCreatesAnUnknownCategoryByNameAndReusesAKnownOne(t *testing.T) {
	ctx := context.Background()
	r := newTorrentRig(t)
	add := func(cat string) core.Download {
		up, err := r.svc.Receive(ctx, strings.NewReader(tinyNZB), "", "")
		require.NoError(t, err)
		d, err := r.svc.Add(ctx, app.AddRequest{Upload: up, CategoryName: cat})
		require.NoError(t, err)
		return d
	}

	first := add("TV")
	second := add("tv")

	cats, err := r.cats.List(ctx)
	require.NoError(t, err)
	require.Len(t, cats, 1)
	assert.Equal(t, "tv", cats[0].Name)
	assert.Equal(t, cats[0].ID, first.CategoryID)
	assert.Equal(t, cats[0].ID, second.CategoryID)
}

// recordedLimits is an app.ShareLimitSetter.
type recordedLimits struct {
	mu  sync.Mutex
	got map[core.ID]core.ShareLimit
}

func (r *recordedLimits) SetShareLimit(_ context.Context, id core.ID, l core.ShareLimit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[core.ID]core.ShareLimit{}
	}
	r.got[id] = l
	return nil
}

func TestAddGivesANewTorrentItsShareLimit(t *testing.T) {
	ctx := context.Background()
	limits := &recordedLimits{}
	r := newTorrentRig(t, app.WithShareLimits(limits))
	data, _ := coretest.BuildTorrent("Some.Show.S01", showFiles)
	up, err := r.svc.Receive(ctx, bytes.NewReader(data), "", core.SourceTorrent)
	require.NoError(t, err)
	want := core.ShareLimit{Ratio: 2, SeedingTime: time.Hour}

	d, err := r.svc.Add(ctx, app.AddRequest{Upload: up, ShareLimit: &want})

	require.NoError(t, err)
	assert.Equal(t, want, limits.got[d.ID])
}

func TestCategoryEnsureFindsOrCreates(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := app.NewCategoryService(s.cats, s.downloads, s.bus)

	created, err := svc.Ensure(ctx, "Movies")
	require.NoError(t, err)
	again, err := svc.Ensure(ctx, "MOVIES")
	require.NoError(t, err)

	assert.Equal(t, "movies", created.Name)
	assert.Equal(t, created.ID, again.ID)
}
