package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func countOf(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
