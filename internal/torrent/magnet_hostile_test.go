package torrent_test

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strconv"
	"testing"

	anacrolix "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

// rawSeeder runs a plain anacrolix client that serves the metadata of a
// torrent whose names our engine would refuse. Its storage maps every file to
// one safe name, which is enough to hand out the info dictionary.
func rawSeeder(t *testing.T, torrentFile []byte) (addr string) {
	t.Helper()
	cfg := anacrolix.NewDefaultClientConfig()
	cfg.ListenHost = anacrolix.LoopbackListenHost
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.NoDefaultPortForwarding = true
	cfg.Seed = true
	cfg.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   t.TempDir(),
		FilePathMaker:   func(storage.FilePathMakerOpts) string { return "one-file" },
		PieceCompletion: storage.NewMapPieceCompletion(),
	})
	cl, err := anacrolix.NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })
	mi, err := metainfo.Load(bytes.NewReader(torrentFile))
	require.NoError(t, err)
	tor, err := cl.AddTorrent(mi)
	require.NoError(t, err)
	<-tor.GotInfo()
	tor.DownloadAll() // a torrent that wants nothing accepts no connections
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(cl.LocalPort()))
}

func TestMagnetWhoseMetadataHasHostileNamesFailsTheTorrentAndWritesNothing(t *testing.T) {
	t.Parallel()
	data := hostileTorrent(t, "root", []hostileFile{{path: []string{"..", "..", "evil"}, data: []byte("payload")}})
	mi, err := metainfo.Load(bytes.NewReader(data))
	require.NoError(t, err)
	h := core.InfoHash(mi.HashInfoBytes())
	addr := rawSeeder(t, data)
	parent := t.TempDir()
	e := newEngine(t)
	h2, err := e.Add(context.Background(), core.TorrentSpec{Magnet: magnetFor(h), StorageDir: filepath.Join(parent, "storage")})
	require.NoError(t, err)
	require.Equal(t, h, h2)

	require.NoError(t, e.AddPeer(h, addr))
	ev := waitEvent(t, e, core.TorrentErrored, h)

	assert.ErrorIs(t, ev.Err, torrent.ErrInvalidMetainfo)
	st, _ := e.Stats(h)
	assert.Equal(t, core.TorrentError, st.State)
	for _, p := range listAll(t, parent) {
		assert.Equal(t, "storage", p, "nothing but the empty storage dir")
	}
}
