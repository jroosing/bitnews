package blackbox_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/torrent/torrenttest"
)

// A magnet with an x.pe hint reaches ready against a local seeder. Runs on
// the Go daemon and, with BITNEWS_E2E_BINARY, on the Zig daemon.
func TestTorrentMagnetReachesReady(t *testing.T) {
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	s := core.DefaultSettings()
	s.Paths.Usenet = core.Roots{Incomplete: roots.usenetIncomplete, Complete: roots.complete}
	s.Paths.Torrent = core.Roots{Incomplete: roots.torrentIncomplete, Complete: roots.torrentComplete}
	s.Queues.MinFree = 1 << 20
	s.Queues.Torrent = 2
	s.Torrent.DisableDHT = true
	saveSettings(t, cfg, s)

	content := torrenttest.MakeRandom(t, "Show.S01", map[string]int{"ep1.mkv": 20_000})
	seeder := torrenttest.Seed(t, content)

	d := startDaemon(t, cfg)
	defer d.stop()

	r := d.api(http.MethodPost, "/api/v1/downloads", fmt.Sprintf(`{"magnet":%q}`, content.Magnet(torrenttest.Addr(seeder))))
	require.Equal(t, http.StatusCreated, r.status, r.body)
	id, _ := obj(t, r)["id"].(string)
	require.NotEmpty(t, id)

	deadline := time.Now().Add(longWait)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = d.download(id)
		switch last["state"] {
		case "ready":
			require.EqualValues(t, content.Size(), num(last, "total_bytes"))
			out, _ := last["output_path"].(string)
			require.NotEmpty(t, out)
			content.AssertFilesEqual(t, out)
			return
		case "failed":
			t.Fatalf("download failed: %s", rbody(last))
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("timed out in %v\n--- download ---\n%s\n--- stderr ---\n%s", last["state"], rbody(last), d.stderr.String())
}

func rbody(m map[string]any) string {
	return fmt.Sprint(m)
}
