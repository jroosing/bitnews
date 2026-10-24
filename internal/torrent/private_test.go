package torrent_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/torrent"
)

func TestPrivateTorrentIsFlaggedInStats(t *testing.T) {
	t.Parallel()
	pub := makeContent(t, "Public", map[string]int{"f": 3000})
	priv := makeContent(t, "Private", map[string]int{"f": 3000}, contentOpts{private: true})
	e := newEngine(t)
	ctx := context.Background()
	hp, err := e.Add(ctx, core.TorrentSpec{Metainfo: pub.torrent, StorageDir: pub.seedDir})
	require.NoError(t, err)
	hq, err := e.Add(ctx, core.TorrentSpec{Metainfo: priv.torrent, StorageDir: priv.seedDir})
	require.NoError(t, err)

	sp, _ := e.Stats(hp)
	sq, _ := e.Stats(hq)

	assert.False(t, sp.Private)
	assert.True(t, sq.Private)
}

func TestPrivateTorrentDownloadsLikeAnyOther(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "PrivateShow", map[string]int{"a": 40_000}, contentOpts{private: true})
	seeder := seed(t, c)
	leech := newEngine(t)
	dir := t.TempDir()
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))

	waitEvent(t, leech, core.TorrentFetched, h)

	c.assertFilesEqual(t, dir)
}

// dhtObserver is a DHT node on loopback that records which info hashes it is
// asked about.
type dhtObserver struct {
	srv  *dht.Server
	addr string

	mu      sync.Mutex
	queried map[core.InfoHash]int
}

func newDHTObserver(t *testing.T) *dhtObserver {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	o := &dhtObserver{queried: map[core.InfoHash]int{}, addr: pc.LocalAddr().String()}
	cfg := dht.NewDefaultServerConfig()
	cfg.Conn = pc
	cfg.StartingNodes = func() ([]dht.Addr, error) { return nil, nil }
	cfg.OnQuery = func(q *krpc.Msg, _ net.Addr) bool {
		if q.A != nil && (q.Q == "get_peers" || q.Q == "announce_peer") {
			o.mu.Lock()
			o.queried[core.InfoHash(q.A.InfoHash)]++
			o.mu.Unlock()
		}
		return true
	}
	o.srv, err = dht.NewServer(cfg)
	require.NoError(t, err)
	t.Cleanup(o.srv.Close)
	return o
}

func (o *dhtObserver) asked(h core.InfoHash) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.queried[h]
}

func TestPrivateTorrentsNeverTouchTheDHT(t *testing.T) {
	t.Parallel()
	obs := newDHTObserver(t)
	pub := makeContent(t, "PublicDHT", map[string]int{"f": 3000})
	priv := makeContent(t, "PrivateDHT", map[string]int{"f": 3000}, contentOpts{private: true})
	e := newEngine(t, func(c *torrent.Config) { c.DHT, c.DHTBootstrap = true, []string{obs.addr} })
	ctx := context.Background()
	_, err := e.Add(ctx, core.TorrentSpec{Metainfo: priv.torrent, StorageDir: priv.seedDir})
	require.NoError(t, err)
	_, err = e.Add(ctx, core.TorrentSpec{Metainfo: pub.torrent, StorageDir: pub.seedDir})
	require.NoError(t, err)

	// The public torrent proves that the engine does announce on this DHT...
	require.Eventually(t, func() bool { return obs.asked(pub.hash) > 0 }, 20*time.Second, 50*time.Millisecond,
		"the control: a public torrent is announced to the DHT")
	// ...so silence about the private one means something.
	time.Sleep(2 * time.Second)
	assert.Zero(t, obs.asked(priv.hash), "a private torrent must never be looked up or announced on the DHT")
}

func TestPrivateTorrentsNeverNegotiatePEX(t *testing.T) {
	t.Parallel()
	for _, private := range []bool{false, true} {
		name := map[bool]string{false: "public torrent negotiates ut_pex", true: "private torrent does not"}[private]
		t.Run(name, func(t *testing.T) {
			c := makeContent(t, "PEX", map[string]int{"f": 120_000}, contentOpts{private: private})
			withPEX := func(cfg *torrent.Config) { cfg.PEX = true }
			seen := map[string]func() []torrent.Handshake{}
			seeder := seed(t, c, withPEX, func(cfg *torrent.Config) {}) // observer set below, before any peer connects
			seen["seeder"] = seeder.ObserveHandshakes()
			leech := newEngine(t, withPEX)
			seen["leecher"] = leech.ObserveHandshakes()
			// Slow the download down so the connection lives long enough to look at.
			require.NoError(t, leech.SetRateLimits(30_000, 0))
			h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: t.TempDir()})
			require.NoError(t, err)
			require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))

			engines := map[string]func() []torrent.Handshake{"seeder": seen["seeder"], "leecher": seen["leecher"]}
			for label, get := range engines {
				require.Eventually(t, func() bool { return len(get()) > 0 }, 10*time.Second, 20*time.Millisecond, label)
				for _, x := range get() {
					assert.Equal(t, !private, x.RemoteOffersPEX, "%s: ut_pex offered by the remote", label)
					assert.Equal(t, !private, x.WeOfferPEX, "%s: ut_pex offered by us", label)
				}
			}
		})
	}
}
