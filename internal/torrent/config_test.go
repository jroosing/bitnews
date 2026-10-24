package torrent_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/jroosing/bitnews/internal/torrent"
)

func baseConfig(t *testing.T) torrent.Config {
	t.Helper()
	return torrent.Config{PieceStore: newMemStore()}
}

func TestClientConfigNeverUsesPortForwarding(t *testing.T) {
	for _, dht := range []bool{false, true} {
		cfg := baseConfig(t)
		cfg.DHT, cfg.PEX, cfg.ListenPort = dht, dht, 51413

		cc, err := torrent.ClientConfigFor(cfg)

		require.NoError(t, err)
		assert.True(t, cc.NoDefaultPortForwarding, "UPnP and NAT-PMP are permanently off")
	}
}

func TestClientConfigMapsDiscoverySettings(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DHT, cfg.PEX = true, false

	cc, err := torrent.ClientConfigFor(cfg)

	require.NoError(t, err)
	// The DHT runs on the engine's own sockets, so anacrolix's is always off.
	assert.True(t, cc.NoDHT)
	assert.True(t, cc.DisablePEX)
	assert.False(t, cc.PeriodicallyAnnounceTorrentsToDht, "the engine announces itself, skipping private torrents")

	cfg.DHT, cfg.PEX = false, true
	cc, err = torrent.ClientConfigFor(cfg)
	require.NoError(t, err)
	assert.False(t, cc.DisablePEX)
}

func TestClientConfigMapsTheMSEPolicy(t *testing.T) {
	tests := map[torrent.MSE]struct{ preferred, required bool }{
		"":                 {true, false}, // the default is prefer
		torrent.MSEPrefer:  {true, false},
		torrent.MSERequire: {true, true},
		torrent.MSEDisable: {false, false},
	}
	for mse, want := range tests {
		t.Run(string(mse), func(t *testing.T) {
			cfg := baseConfig(t)
			cfg.MSE = mse

			cc, err := torrent.ClientConfigFor(cfg)

			require.NoError(t, err)
			assert.Equal(t, want.preferred, cc.HeaderObfuscationPolicy.Preferred)
			assert.Equal(t, want.required, cc.HeaderObfuscationPolicy.RequirePreferred)
		})
	}
}

func TestClientConfigListenAndBind(t *testing.T) {
	cfg := baseConfig(t)
	cfg.ListenPort = 6881

	cc, err := torrent.ClientConfigFor(cfg)
	require.NoError(t, err)
	assert.Equal(t, 6881, cc.ListenPort)
	assert.Equal(t, "", cc.ListenHost("tcp4"), "no bind address listens on all interfaces")
	assert.False(t, cc.DisableIPv4)
	assert.False(t, cc.DisableIPv6)

	cfg.BindAddr = "192.0.2.7"
	cc, err = torrent.ClientConfigFor(cfg)
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.7", cc.ListenHost("tcp4"))
	assert.Equal(t, "192.0.2.7", cc.ListenHost("udp4"))
	assert.True(t, cc.DisableIPv6, "an IPv4 bind address must not open IPv6 sockets")
	require.NotNil(t, cc.HTTPDialContext)
	require.NotNil(t, cc.TrackerDialContext)

	cfg.BindAddr = "2001:db8::1"
	cc, err = torrent.ClientConfigFor(cfg)
	require.NoError(t, err)
	assert.True(t, cc.DisableIPv4)
	assert.False(t, cc.DisableIPv6)
}

func TestClientConfigHardensTheClient(t *testing.T) {
	cc, err := torrent.ClientConfigFor(baseConfig(t))

	require.NoError(t, err)
	assert.True(t, cc.DisableWebseeds, "a hostile torrent must not make us fetch arbitrary URLs")
	assert.True(t, cc.DisableWebtorrent)
	assert.True(t, cc.Seed)
	assert.NotNil(t, cc.DefaultStorage, "nothing may be stored outside a torrent's own directory")
	assert.NotNil(t, cc.Slogger)
}

func TestClientConfigRateLimitersStartUnlimited(t *testing.T) {
	cc, err := torrent.ClientConfigFor(baseConfig(t))

	require.NoError(t, err)
	require.NotNil(t, cc.DownloadRateLimiter)
	require.NotNil(t, cc.UploadRateLimiter)
	assert.Equal(t, rate.Inf, cc.DownloadRateLimiter.Limit())
	assert.Equal(t, rate.Inf, cc.UploadRateLimiter.Limit())
}

func TestClientConfigRejectsInvalidSettings(t *testing.T) {
	tests := map[string]func(*torrent.Config){
		"unknown MSE":    func(c *torrent.Config) { c.MSE = "maybe" },
		"host name bind": func(c *torrent.Config) { c.BindAddr = "localhost" },
		"bind with port": func(c *torrent.Config) { c.BindAddr = "127.0.0.1:80" },
		"negative port":  func(c *torrent.Config) { c.ListenPort = -1 },
		"port too large": func(c *torrent.Config) { c.ListenPort = 65536 },
		"no piece store": func(c *torrent.Config) { c.PieceStore = nil },
		"bad bootstrap":  func(c *torrent.Config) { c.DHTBootstrap = []string{"no-port"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig(t)
			mutate(&cfg)

			_, err := torrent.ClientConfigFor(cfg)

			assert.ErrorIs(t, err, torrent.ErrInvalidConfig)
		})
	}
}

var _ = net.IPv4zero
