package torrent_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/netbind"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

func TestClientConfigWithBindingTurnsOffTheUnboundBuiltinSockets(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DHT = true
	cfg.Binding = netbind.Binding{Address: "127.0.0.1"}

	cc, err := torrent.ClientConfigFor(cfg)

	require.NoError(t, err)
	// anacrolix's own listeners and dialers cannot take a Control hook, so
	// the engine opens bound ones itself after the client starts.
	assert.True(t, cc.DisableTCP)
	assert.True(t, cc.DisableUTP)
	assert.True(t, cc.NoDHT)
	require.NotNil(t, cc.HTTPDialContext)
	require.NotNil(t, cc.TrackerDialContext)
	require.NotNil(t, cc.TrackerListenPacket)
}

func TestClientConfigWithoutBindingKeepsTheBuiltinSockets(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DHT = true

	cc, err := torrent.ClientConfigFor(cfg)

	require.NoError(t, err)
	assert.False(t, cc.DisableTCP)
	assert.False(t, cc.DisableUTP)
	assert.False(t, cc.NoDHT)
}

func TestTrackerAndHTTPSocketsUseTheBinding(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Binding = netbind.Binding{Address: "127.0.0.1"}
	cc, err := torrent.ClientConfigFor(cfg)
	require.NoError(t, err)

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	for name, dial := range map[string]func(context.Context, string, string) (net.Conn, error){
		"tracker http": cc.TrackerDialContext, "http": cc.HTTPDialContext,
	} {
		c, err := dial(context.Background(), "tcp", ln.Addr().String())
		require.NoError(t, err, name)
		assert.Equal(t, "127.0.0.1", c.LocalAddr().(*net.TCPAddr).IP.String(), name)
		_ = c.Close()
	}

	pc, err := cc.TrackerListenPacket("udp4", "")
	require.NoError(t, err)
	defer pc.Close()
	assert.Equal(t, "127.0.0.1", pc.LocalAddr().(*net.UDPAddr).IP.String(), "tracker udp")
}

func TestBindingConflictingWithBindAddrIsInvalid(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BindAddr = "127.0.0.1"
	cfg.Binding = netbind.Binding{Address: "127.0.0.2"}

	_, err := torrent.ClientConfigFor(cfg)

	assert.ErrorIs(t, err, torrent.ErrInvalidConfig)
}

func TestInvalidBindingIsRefused(t *testing.T) {
	for _, b := range []netbind.Binding{{Interface: "bad name"}, {Address: "not-an-ip"}, {Address: "0.0.0.0"}} {
		cfg := baseConfig(t)
		cfg.Binding = b

		_, err := torrent.ClientConfigFor(cfg)

		assert.ErrorIs(t, err, torrent.ErrInvalidConfig, "%+v", b)
	}
}

func TestNewFailsWhenTheBoundInterfaceIsMissing(t *testing.T) {
	cfg := torrent.Config{PieceStore: newMemStore(), Binding: netbind.Binding{Interface: "nobind0"}}

	e, err := torrent.New(cfg)

	if e != nil {
		_ = e.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, torrent.ErrBindUnavailable)
	assert.Contains(t, err.Error(), `"nobind0"`)
}

func TestNewFailsWhenTheBoundAddressIsNotOnThisHost(t *testing.T) {
	cfg := torrent.Config{PieceStore: newMemStore(), Binding: netbind.Binding{Address: "192.0.2.77"}}

	e, err := torrent.New(cfg)

	if e != nil {
		_ = e.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, torrent.ErrBindUnavailable)
	assert.Contains(t, err.Error(), "192.0.2.77")
}

func TestBoundEngineListensTCPAndUTPAndDHTOnlyOnTheBoundAddress(t *testing.T) {
	e := newEngine(t, func(c *torrent.Config) {
		c.BindAddr = ""
		c.Binding = netbind.Binding{Address: "127.0.0.1"}
		c.DisableUTP = false
		c.DHT = true
		c.DHTBootstrap = []string{"127.0.0.1:9"}
	})

	var networks []string
	for _, a := range e.ListenAddrs() {
		assert.Equal(t, "127.0.0.1", hostOf(t, a))
		networks = append(networks, a.Network())
	}
	assert.ElementsMatch(t, []string{"tcp", "udp"}, networks, "an IPv4 address opens one TCP and one uTP socket and no IPv6 ones")
	dhts := e.DHTAddrs()
	require.Len(t, dhts, 1)
	assert.Equal(t, "127.0.0.1", hostOf(t, dhts[0]))
	assert.NotZero(t, e.LocalPort())
}

func hostOf(t *testing.T, a net.Addr) string {
	t.Helper()
	h, _, err := net.SplitHostPort(a.String())
	require.NoError(t, err)
	return h
}

func TestBoundEnginesSwapDataOverTCPAndUTP(t *testing.T) {
	for _, tc := range []struct {
		name       string
		disableUTP bool
	}{{"tcp", true}, {"utp enabled", false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := makeContent(t, "Bound", map[string]int{"a": 60_000, "b": 5_000})
			bound := func(cfg *torrent.Config) {
				cfg.BindAddr = ""
				cfg.Binding = netbind.Binding{Address: "127.0.0.1"}
				cfg.DisableUTP = tc.disableUTP
			}
			seeder := seed(t, c, bound)
			leech := newEngine(t, bound)
			dir := t.TempDir()
			h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
			require.NoError(t, err)
			require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))

			waitEvent(t, leech, core.TorrentFetched, h)

			c.assertFilesEqual(t, dir)
		})
	}
}

func TestBoundEngineCloseReleasesItsPort(t *testing.T) {
	e := newEngine(t, func(c *torrent.Config) { c.DisableUTP = false })
	port := e.LocalPort()
	require.NoError(t, e.Close())

	require.Eventually(t, func() bool {
		l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err != nil {
			return false
		}
		_ = l.Close()
		pc, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err != nil {
			return false
		}
		_ = pc.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "TCP and UDP sockets must be closed with the engine")
}
