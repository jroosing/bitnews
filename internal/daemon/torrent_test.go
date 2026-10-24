package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/qbit"
	"github.com/jroosing/bitnews/internal/core"
	trunner "github.com/jroosing/bitnews/internal/torrent/runner"
)

var (
	_ trunner.Remover = (*shareLimitRemover)(nil)
	_ fetchHolder     = (*trunner.Runner)(nil)
)

type stubFinisher struct {
	calls int
	out   string
}

func (s *stubFinisher) Finish(context.Context, core.Download, string, string) (string, error) {
	s.calls++
	return s.out, nil
}

type stubReporter struct{ calls int }

func (s *stubReporter) FetchReport(context.Context, core.Download) (core.FetchReport, error) {
	s.calls++
	return core.FetchReport{MissingArticles: 3}, nil
}

func TestTorrentFetchReporterAnswersForTorrentsAndLeavesNZBsToTheUsenetRunner(t *testing.T) {
	usenet := &stubReporter{}
	r := torrentFetchReporter{usenet: usenet}

	got, err := r.FetchReport(context.Background(), core.Download{SourceKind: core.SourceTorrent})
	require.NoError(t, err)
	assert.Equal(t, core.FetchReport{}, got)
	assert.Zero(t, usenet.calls)

	got, err = r.FetchReport(context.Background(), core.Download{SourceKind: core.SourceNZB})
	require.NoError(t, err)
	assert.Equal(t, 3, got.MissingArticles)
}

func TestTorrentFinisherLeavesAManagedTorrentWhereItIs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.mkv"), []byte("a"), 0o600))
	inner := &stubFinisher{out: "/elsewhere"}
	f := torrentFinisher{inner: inner, managed: func(core.ID) bool { return true }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	out, err := f.Finish(context.Background(), core.Download{ID: "x", SourceKind: core.SourceTorrent}, dir, filepath.Join(dir, "_unpack"))

	require.NoError(t, err)
	assert.Equal(t, dir, out)
	assert.Zero(t, inner.calls, "the real Finisher would move the files from under the engine")
	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 1, "no marker, nothing added")
}

func TestTorrentFinisherFailsWhenTheDirectoryIsGone(t *testing.T) {
	f := torrentFinisher{inner: &stubFinisher{}, managed: func(core.ID) bool { return true }}

	_, err := f.Finish(context.Background(), core.Download{SourceKind: core.SourceTorrent}, filepath.Join(t.TempDir(), "gone"), "")

	assert.Error(t, err)
}

func TestTorrentFinisherPassesEverythingElseOn(t *testing.T) {
	for name, tc := range map[string]struct {
		d       core.Download
		managed func(core.ID) bool
	}{
		"an NZB":                     {core.Download{SourceKind: core.SourceNZB}, func(core.ID) bool { return true }},
		"a torrent the engine lacks": {core.Download{SourceKind: core.SourceTorrent}, func(core.ID) bool { return false }},
		"torrents are off":           {core.Download{SourceKind: core.SourceTorrent}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			inner := &stubFinisher{out: "/out"}
			f := torrentFinisher{inner: inner, managed: tc.managed}

			out, err := f.Finish(context.Background(), tc.d, t.TempDir(), "")

			require.NoError(t, err)
			assert.Equal(t, "/out", out)
			assert.Equal(t, 1, inner.calls)
		})
	}
}

func TestBindingKillswitchResumesOnlyWhatItPaused(t *testing.T) {
	var mu sync.Mutex
	var paused, resumed int
	k := &bindingKillswitch{
		name: "usenet", log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pause:  func(string) bool { mu.Lock(); defer mu.Unlock(); paused++; return false }, // the user had paused it
		resume: func() { mu.Lock(); defer mu.Unlock(); resumed++ },
	}

	k.onPause("interface gone")
	k.onResume()

	assert.Equal(t, 1, paused)
	assert.Zero(t, resumed, "the user's own pause stays")

	k.pause = func(string) bool { mu.Lock(); defer mu.Unlock(); paused++; return true }
	k.onPause("interface gone")
	k.onResume()
	k.onResume()
	assert.Equal(t, 1, resumed, "resumed once")
}

func TestBindingWatcherWarnsOncePerChange(t *testing.T) {
	var buf syncBuf
	s := core.DefaultSettings()
	w := &bindingWatcher{start: s.Network, current: func() core.Settings { return s }, log: slog.New(slog.NewTextHandler(&buf, nil))}

	w.check()
	assert.Empty(t, buf.String(), "nothing changed")

	s.Network.Torrent.Interface = "wg0"
	w.check()
	w.check()
	assert.Equal(t, 1, countOf(buf.String(), "restart bitnews"))

	s.Network.Torrent.Interface = ""
	w.check()
	assert.Equal(t, 1, countOf(buf.String(), "restart bitnews"), "back to what we started with")
}

func countOf(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

func TestCoreShareLimitMapsMinusOneToUnlimited(t *testing.T) {
	assert.Equal(t, core.UnlimitedShareLimit(), coreShareLimit(-1, -1))
	assert.Equal(t, core.ShareLimit{Ratio: 1.25, SeedingTime: 90 * time.Second}, coreShareLimit(1.25, 90))
	assert.Equal(t, core.ShareLimit{Ratio: 0, SeedingTime: -1}, coreShareLimit(0, -1), "a ratio of zero is a limit that is reached at once")
}

func TestQBitGlobalShareReportsTheSeedingDefaults(t *testing.T) {
	s := core.DefaultSettings()
	got := qbitGlobalShare(func() core.Settings { return s })()
	assert.Equal(t, qbit.GlobalShare{Action: "pause"}, got, "no default limit, pause")

	s.Seeding = core.SeedingSettings{RatioLimit: 1.5, SeedingTimeMinutes: 120, Action: core.SeedingActionRemoveData}
	got = qbitGlobalShare(func() core.Settings { return s })()
	assert.Equal(t, qbit.GlobalShare{RatioEnabled: true, Ratio: 1.5, SeedingTimeEnabled: true, SeedingMinutes: 120, Action: "remove_files"}, got)
	s.Seeding.Action = core.SeedingActionRemove
	assert.Equal(t, "remove", qbitGlobalShare(func() core.Settings { return s })().Action)
}
