package view_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// fakeLive answers Live from a map and records nothing else.
type fakeLive map[core.ID]view.Live

func (f fakeLive) Live(_ context.Context, d core.Download) view.Live { return f[d.ID] }

func newModule(t *testing.T, live view.LiveSource) *view.Module {
	t.Helper()
	m, err := view.New(view.Config{Live: live, Settings: core.DefaultSettings})
	require.NoError(t, err)
	return m
}

func dl(state core.State, opts ...func(*core.Download)) core.Download {
	d := core.Download{
		ID: "d1", Name: "Some.Release", State: state, SourceKind: core.SourceNZB,
		Priority: core.DownloadPriorityNormal, TotalBytes: 1000,
	}
	for _, o := range opts {
		o(&d)
	}
	return d
}

func TestPhaseFollowsTheStateAndTheStage(t *testing.T) {
	cases := []struct {
		name  string
		d     core.Download
		stage core.Stage
		want  view.Phase
	}{
		{"added", dl(core.StateAdded), "", view.PhaseWaiting},
		{"queued", dl(core.StateQueued), "", view.PhaseWaiting},
		{"metadata", dl(core.StateFetchingMetadata), "", view.PhaseMetadata},
		{"fetching", dl(core.StateFetching), "", view.PhaseDownloading},
		{"paused while queued", dl(core.StateQueued, func(d *core.Download) { d.Paused = true }), "", view.PhasePaused},
		{"paused while fetching", dl(core.StateFetching, func(d *core.Download) { d.Paused = true }), "", view.PhasePaused},
		{"fetched is the first step", dl(core.StateFetched), "", view.PhaseVerifying},
		{"post-processing without a stage", dl(core.StatePostProcessing), "", view.PhaseVerifying},
		{"verifying", dl(core.StatePostProcessing), core.StageVerifying, view.PhaseVerifying},
		{"repairing", dl(core.StatePostProcessing), core.StageRepairing, view.PhaseRepairing},
		{"unpacking", dl(core.StatePostProcessing), core.StageUnpacking, view.PhaseUnpacking},
		{"finishing", dl(core.StatePostProcessing), core.StageFinishing, view.PhaseFinishing},
		{"ready", dl(core.StateReady), "", view.PhaseReady},
		{"failed", dl(core.StateFailed), "", view.PhaseFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newModule(t, fakeLive{"d1": {Stage: c.stage, HaveStage: c.stage != ""}})

			v := m.Of(context.Background(), c.d)

			assert.Equal(t, c.want, v.Phase)
			assert.Equal(t, c.d.State.InQueue(), v.InQueue)
		})
	}
}

func TestProgressIsTheFractionDoneAndOnceFetchedComplete(t *testing.T) {
	m := newModule(t, fakeLive{})

	half := m.Of(context.Background(), dl(core.StateFetching, func(d *core.Download) { d.DoneBytes = 250 }))
	fetched := m.Of(context.Background(), dl(core.StateFetched, func(d *core.Download) { d.DoneBytes = 990 }))
	unknown := m.Of(context.Background(), dl(core.StateFetching, func(d *core.Download) { d.TotalBytes = 0; d.DoneBytes = 5 }))
	over := m.Of(context.Background(), dl(core.StateFetching, func(d *core.Download) { d.DoneBytes = 5000 }))

	assert.InDelta(t, 0.25, half.Progress, 1e-9)
	assert.Equal(t, 1.0, fetched.Progress)
	assert.Equal(t, int64(1000), fetched.Done, "a Fetched Download has all its bytes")
	assert.Equal(t, 0.0, unknown.Progress)
	assert.Equal(t, 1.0, over.Progress, "never above 1")
	assert.Equal(t, int64(250), half.Done)
	assert.Equal(t, int64(1000), half.Total)
}

func TestEtaIsTheRemainingBytesOverTheDownloadsOwnRate(t *testing.T) {
	m := newModule(t, fakeLive{"d1": {DownRate: 100}})

	v := m.Of(context.Background(), dl(core.StateFetching, func(d *core.Download) { d.DoneBytes = 400 }))

	assert.True(t, v.ETAKnown)
	assert.Equal(t, 6*time.Second, v.ETA)
	assert.Equal(t, int64(100), v.DownRate)
}

func TestEtaIsUnknownWithoutARateOrOutsideDownloading(t *testing.T) {
	still := newModule(t, fakeLive{})
	moving := newModule(t, fakeLive{"d1": {DownRate: 100}})

	assert.False(t, still.Of(context.Background(), dl(core.StateFetching)).ETAKnown)
	assert.False(t, moving.Of(context.Background(), dl(core.StateQueued)).ETAKnown)
	assert.False(t, moving.Of(context.Background(), dl(core.StateReady)).ETAKnown)
}

func TestATorrentViewAddsTheEnginesCountersAndTheRatio(t *testing.T) {
	m := newModule(t, fakeLive{"d1": {Torrent: &view.TorrentLive{
		Hash: "abc", Registered: true, Size: 2000, DoneBytes: 1500, Peers: 4, Seeds: 2,
		Uploaded: 3000, SeedingTime: time.Hour,
	}}})

	v := m.Of(context.Background(), dl(core.StateFetching, func(d *core.Download) { d.SourceKind = core.SourceTorrent; d.DoneBytes = 10 }))

	require.NotNil(t, v.Torrent)
	assert.Equal(t, int64(2000), v.Total, "the engine knows the size")
	assert.Equal(t, int64(1500), v.Done, "and how much is done")
	assert.InDelta(t, 0.75, v.Progress, 1e-9)
	assert.InDelta(t, 1.5, v.Torrent.Ratio, 1e-9)
	assert.Equal(t, "abc", v.Torrent.Hash)
	assert.Equal(t, 4, v.Torrent.Peers)
	assert.Equal(t, time.Hour, v.Torrent.SeedingTime)
}

func TestARegularDownloadHasNoTorrentFacts(t *testing.T) {
	m := newModule(t, fakeLive{})

	assert.Nil(t, m.Of(context.Background(), dl(core.StateFetching)).Torrent)
}

func TestTheShareLimitIsTheGlobalDefaultUnlessTheDownloadHasItsOwn(t *testing.T) {
	ratio := 2.0
	seeding := 30 * time.Minute
	settings := core.DefaultSettings()
	settings.Seeding.RatioLimit = 1.5
	settings.Seeding.SeedingTimeMinutes = 60
	live := fakeLive{
		"global":  {Torrent: &view.TorrentLive{}},
		"ratio":   {Torrent: &view.TorrentLive{OwnRatio: &ratio}},
		"both":    {Torrent: &view.TorrentLive{OwnRatio: &ratio, OwnSeedingTime: &seeding}},
		"nolimit": {Torrent: &view.TorrentLive{OwnRatio: new(-1.0), OwnSeedingTime: ptr(time.Duration(-1))}},
	}
	m, err := view.New(view.Config{Live: live, Settings: func() core.Settings { return settings }})
	require.NoError(t, err)
	of := func(id core.ID) view.ShareLimit {
		return m.Of(context.Background(), dl(core.StateReady, func(d *core.Download) { d.ID = id })).Torrent.ShareLimit
	}

	assert.Equal(t, view.ShareLimit{Global: true}, of("global"))
	assert.Equal(t, view.ShareLimit{Ratio: 2, Seeding: time.Hour}, of("ratio"), "the global default fills the other half")
	assert.Equal(t, view.ShareLimit{Ratio: 2, Seeding: 30 * time.Minute}, of("both"))
	assert.Equal(t, view.ShareLimit{Ratio: -1, Seeding: -1}, of("nolimit"))
}

//go:fix inline
func ptr[T any](v T) *T { return new(v) }

func TestAFailedDownloadCarriesItsReasonAndACleanedDetail(t *testing.T) {
	m := newModule(t, fakeLive{})

	v := m.Of(context.Background(), dl(core.StateFailed, func(d *core.Download) {
		d.FailureReason = core.FailureMissingArticles
		d.FailureDetail = "fetch https://user:pw@news.example.com/x?key=1 failed\n\tbadly"
	}))

	require.NotNil(t, v.Failure)
	assert.Equal(t, core.FailureMissingArticles, v.Failure.Reason)
	assert.NotContains(t, v.Failure.Detail, "pw")
	assert.NotContains(t, v.Failure.Detail, "\n")
	assert.Contains(t, v.Failure.Detail, "badly")
	assert.Nil(t, m.Of(context.Background(), dl(core.StateReady)).Failure)
}

func TestCleanDetailCapsTheLengthOnARuneBoundary(t *testing.T) {
	long := ""
	for range 200 {
		long += "é"
	}

	got := view.CleanDetail(long)

	assert.LessOrEqual(t, len(got), 303)
	assert.True(t, len(got) > 3 && got[len(got)-3:] == "...")
	assert.True(t, isValidUTF8(got))
}

func isValidUTF8(s string) bool { return []rune(s) != nil && string([]rune(s)) == s }

func TestCleanDetailDropsInvisibleFormatCharacters(t *testing.T) {
	got := view.CleanDetail("name\u202egpj.exe\u200b ok")

	assert.NotContains(t, got, "\u202e")
	assert.NotContains(t, got, "\u200b")
	assert.Contains(t, got, "ok")
}

func TestAFetchedTorrentIsCompleteAgainstTheEnginesSize(t *testing.T) {
	m := newModule(t, fakeLive{"d1": {Torrent: &view.TorrentLive{Size: 2000}}})

	v := m.Of(context.Background(), dl(core.StateReady, func(d *core.Download) { d.SourceKind = core.SourceTorrent }))

	assert.Equal(t, int64(2000), v.Total)
	assert.Equal(t, v.Total, v.Done, "done matches the total shown")
	assert.Equal(t, 1.0, v.Progress)
}
