package torrent_test

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/netbind"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

// Interface binding on the loopback interface (SO_BINDTODEVICE). It skips
// when the kernel or sandbox does not allow it for this user.
func TestEnginesBoundToTheLoopbackInterfaceSwapData(t *testing.T) {
	bound := func(cfg *torrent.Config) {
		cfg.BindAddr = ""
		cfg.Binding = netbind.Binding{Interface: "lo"}
		cfg.DisableUTP = false
		cfg.PieceStore = newMemStore()
	}
	probe, err := torrent.New(torrent.Config{PieceStore: newMemStore(), Binding: netbind.Binding{Interface: "lo"}})
	if err != nil && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) {
		t.Skipf("SO_BINDTODEVICE not permitted: %v", err)
	}
	require.NoError(t, err)
	require.NoError(t, probe.Close())

	c := makeContent(t, "BoundLo", map[string]int{"a": 80_000})
	seeder := seed(t, c, bound)
	leech := newEngine(t, bound)
	dir := t.TempDir()
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))

	waitEvent(t, leech, core.TorrentFetched, h)

	c.assertFilesEqual(t, dir)
}
