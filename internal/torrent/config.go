package torrent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"time"

	"github.com/anacrolix/dht/v2"
	anacrolix "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"

	"gitlab.com/kyura4k/gitnews/internal/netbind"
)

// ErrInvalidConfig is returned by New for settings that cannot work.
var ErrInvalidConfig = errors.New("torrent: invalid config")

// MSE is the message stream encryption (header obfuscation) policy.
type MSE string

// The MSE policies. The zero value means MSEPrefer.
const (
	MSEPrefer  MSE = "prefer"  // offer obfuscation, accept plaintext peers
	MSERequire MSE = "require" // refuse plaintext peers
	MSEDisable MSE = "disable" // plaintext only
)

// Config configures an Engine.
//
// UPnP and NAT-PMP port mapping are permanently off and have no setting.
type Config struct {
	// ListenPort is the TCP and uTP port. Zero picks a random port.
	ListenPort int
	// BindAddr, when set, is the one local IP address the engine listens on
	// and dials from. Empty listens on all interfaces. Host names are refused.
	BindAddr string
	// Binding binds every socket of the engine (peer TCP and uTP, DHT,
	// trackers) to an interface and/or a local address, as a kill switch.
	// It takes effect when New starts the engine; the daemon restarts the
	// engine to change it. New fails with ErrBindUnavailable when the
	// binding cannot carry traffic, and never starts unbound. Binding.Address
	// and BindAddr are the same setting: set at most one, or the same value.
	Binding netbind.Binding

	// DisableUTP turns off uTP and leaves TCP only. Peers dial both at once
	// and, in anacrolix/torrent v1.61.0, a transfer occasionally stalls when
	// both connections race; the local swarm tests therefore use TCP only.
	DisableUTP bool

	// DHT, PEX and LSD are used for public torrents only. Private torrents
	// (BEP 27) never use them, whatever these say. anacrolix/torrent v1.61.0
	// has no LSD, so LSD has no effect yet.
	DHT, PEX, LSD bool
	// DHTBootstrap lists "host:port" DHT nodes to start from. Empty uses
	// anacrolix's public bootstrap nodes.
	DHTBootstrap []string

	// MSE is the header obfuscation policy. The zero value is MSEPrefer.
	MSE MSE

	// PieceStore holds the piece completion rows. Required. The engine
	// owns the completion built on it and closes it in Close.
	PieceStore PieceStore
	// FlushInterval is how often piece marks are written to PieceStore.
	// Zero means DefaultFlushInterval.
	FlushInterval time.Duration

	// ShareLimitInterval is how often Share Limits are checked. Zero means
	// 30 seconds.
	ShareLimitInterval time.Duration
	// Now is the clock for Share Limits and seeding time. Nil means time.Now.
	Now func() time.Time

	// EventBuffer is the size of the Events channel. Zero means 256. When it
	// is full, events are dropped (and logged) rather than block the engine.
	EventBuffer int

	// Logger receives engine and anacrolix log lines. Nil discards them.
	// anacrolix logs tracker URLs, which may contain passkeys, so pass a
	// logger whose handler redacts them if it is not discarded.
	Logger *slog.Logger
}

const (
	defaultShareLimitInterval = 30 * time.Second
	defaultEventBuffer        = 256
	// maxRateBurst caps the burst of the rate limiters at one MiB, which
	// anacrolix also uses for an unlimited limiter.
	maxRateBurst = 1 << 20
	// minRateBurst has to cover a whole 16 KiB chunk plus protocol overhead.
	minRateBurst = 64 << 10
)

func (c Config) validate() error {
	switch c.MSE {
	case "", MSEPrefer, MSERequire, MSEDisable:
	default:
		return fmt.Errorf("%w: unknown MSE policy %q", ErrInvalidConfig, c.MSE)
	}
	if c.ListenPort < 0 || c.ListenPort > math.MaxUint16 {
		return fmt.Errorf("%w: listen port %d", ErrInvalidConfig, c.ListenPort)
	}
	if c.BindAddr != "" && net.ParseIP(c.BindAddr) == nil {
		return fmt.Errorf("%w: bind address %q is not an IP address", ErrInvalidConfig, c.BindAddr)
	}
	if err := c.Binding.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if c.BindAddr != "" && c.Binding.Address != "" && c.BindAddr != c.Binding.Address {
		return fmt.Errorf("%w: bind address %q conflicts with binding address %q", ErrInvalidConfig, c.BindAddr, c.Binding.Address)
	}
	if c.PieceStore == nil {
		return fmt.Errorf("%w: piece store required", ErrInvalidConfig)
	}
	for _, n := range c.DHTBootstrap {
		if _, _, err := net.SplitHostPort(n); err != nil {
			return fmt.Errorf("%w: DHT bootstrap node %q: %v", ErrInvalidConfig, n, err)
		}
	}
	return nil
}

// clientConfig builds the anacrolix client configuration for cfg and wires
// the engine's hooks and rate limiters into it.
func (e *Engine) clientConfig(cfg Config) (*anacrolix.ClientConfig, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cc := anacrolix.NewDefaultClientConfig()
	cc.ListenPort = cfg.ListenPort

	cc.DisableUTP = cfg.DisableUTP
	cc.NoDefaultPortForwarding = true // permanent: no UPnP, no NAT-PMP
	cc.NoDHT = !cfg.DHT
	cc.DisablePEX = !cfg.PEX
	// The engine runs the DHT announces itself so that it can skip private
	// torrents; anacrolix v1.61.0 announces every torrent.
	cc.PeriodicallyAnnounceTorrentsToDht = false
	cc.ConfigureAnacrolixDhtServer = e.configureDHTServer
	if len(cfg.DHTBootstrap) > 0 {
		cc.DhtStartingNodes = bootstrapNodes(cfg.DHTBootstrap)
	}

	switch cfg.MSE {
	case MSERequire:
		cc.HeaderObfuscationPolicy = anacrolix.HeaderObfuscationPolicy{Preferred: true, RequirePreferred: true}
	case MSEDisable:
		cc.HeaderObfuscationPolicy = anacrolix.HeaderObfuscationPolicy{}
	default:
		cc.HeaderObfuscationPolicy = anacrolix.HeaderObfuscationPolicy{Preferred: true}
	}

	if b := cfg.binding(); b.Enabled() {
		if b.Address != "" {
			ip := net.ParseIP(b.Address)
			cc.ListenHost = func(string) string { return b.Address }
			cc.DisableIPv4 = ip.To4() == nil
			cc.DisableIPv6 = ip.To4() != nil
		}
		// anacrolix's own sockets cannot be bound to an interface: turn them
		// all off and let attachBound add bound ones (see bind.go).
		cc.DisableTCP, cc.DisableUTP, cc.NoDHT = true, true, true
		d := b.Dialer(&net.Dialer{})
		cc.HTTPDialContext = d.DialContext
		cc.TrackerDialContext = d.DialContext
		lc := b.ListenConfig()
		cc.TrackerListenPacket = func(network, _ string) (net.PacketConn, error) {
			return lc.ListenPacket(context.Background(), network, b.ListenAddr(0))
		}
	}

	// Torrents carry their own storage; the default must never write.
	cc.DefaultStorage = refusingStorage{}
	// A hostile torrent must not make us fetch arbitrary URLs.
	cc.DisableWebseeds = true
	cc.DisableWebtorrent = true
	cc.Seed = true // keep uploading to peers that have nothing to give back

	e.down = rate.NewLimiter(rate.Inf, maxRateBurst)
	e.up = rate.NewLimiter(rate.Inf, maxRateBurst)
	cc.DownloadRateLimiter = e.down
	cc.UploadRateLimiter = e.up

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	cc.Slogger = logger

	cc.Callbacks.PeerConnAdded = append(cc.Callbacks.PeerConnAdded, e.onPeerConnAdded)
	cc.Callbacks.ReadExtendedHandshake = e.onExtendedHandshake
	return cc, nil
}

// bootstrapNodes resolves the configured "host:port" nodes each time the DHT
// asks for starting nodes.
func bootstrapNodes(nodes []string) func(network string) dht.StartingNodesGetter {
	return func(network string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) {
			var out []dht.Addr
			var errs []error
			for _, n := range nodes {
				ua, err := net.ResolveUDPAddr(network, n)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				out = append(out, dht.NewAddr(ua))
			}
			if len(out) == 0 {
				return nil, errors.Join(errs...)
			}
			return out, nil
		}
	}
}

// setRateLimit applies a limit in bytes per second to l; zero or less means
// unlimited.
func setRateLimit(l *rate.Limiter, bytesPerSecond int64) {
	if bytesPerSecond <= 0 {
		l.SetLimit(rate.Inf)
		l.SetBurst(maxRateBurst)
		return
	}
	l.SetBurst(int(min(max(bytesPerSecond, minRateBurst), maxRateBurst)))
	l.SetLimit(rate.Limit(bytesPerSecond))
}

// refusingStorage is the client's default storage. Every torrent brings its
// own, so reaching this one is a bug.
type refusingStorage struct{}

func (refusingStorage) OpenTorrent(context.Context, *metainfo.Info, metainfo.Hash) (storage.TorrentImpl, error) {
	return storage.TorrentImpl{}, errors.New("torrent: no default storage")
}
