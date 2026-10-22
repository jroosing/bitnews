package torrent

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	anacrolix "github.com/anacrolix/torrent"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// ClientConfigFor exposes the anacrolix client configuration an Engine would
// start with, so tests can assert the security-relevant settings.
func ClientConfigFor(cfg Config) (*anacrolix.ClientConfig, error) {
	e := &Engine{}
	return e.clientConfig(cfg)
}

// ListenAddr is the loopback address other test engines can dial.
func (e *Engine) ListenAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(e.cl.LocalPort()))
}

// AddPeer hands torrent h a peer address to connect to, and keeps offering it
// every 300 ms until the engine closes or the torrent is gone, as a tracker
// would. (A single offer can be lost when a dial times out under load.)
func (e *Engine) AddPeer(h core.InfoHash, addr string) error {
	if err := e.addPeerOnce(h, addr); err != nil {
		return err
	}
	go func() {
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-tick.C:
				if e.addPeerOnce(h, addr) != nil {
					return
				}
			}
		}
	}()
	return nil
}

func (e *Engine) addPeerOnce(h core.InfoHash, addr string) error {
	ent, err := e.lookup(h)
	if err != nil {
		return err
	}
	ent.mu.Lock()
	t := ent.t
	ent.mu.Unlock()
	t.AddPeers([]anacrolix.PeerInfo{{Addr: anacrolix.StringAddr(addr), Trusted: true}})
	return nil
}

// BytesHashed is how many bytes the current incarnation of torrent h has
// hashed; zero proves piece completion spared it a rehash.
func (e *Engine) BytesHashed(h core.InfoHash) int64 {
	ent, err := e.lookup(h)
	if err != nil {
		return -1
	}
	ent.mu.Lock()
	t := ent.t
	ent.mu.Unlock()
	st := t.Stats()
	return st.BytesHashed.Int64()
}

// CheckShareLimits runs one Share Limit check, as the ticker does.
func (e *Engine) CheckShareLimits() { e.checkShareLimits() }

// AnacrolixTorrent returns the anacrolix torrent behind h.
func (e *Engine) AnacrolixTorrent(h core.InfoHash) *anacrolix.Torrent {
	ent, err := e.lookup(h)
	if err != nil {
		return nil
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()
	return ent.t
}

// TorrentPaths exposes the path validation of a .torrent file.
func TorrentPaths(dir string, torrentFile []byte) ([]string, error) {
	_, info, err := parseMetainfo(torrentFile)
	if err != nil {
		return nil, err
	}
	return torrentPaths(dir, info)
}

// Handshake is what one extended handshake negotiated about PEX.
type Handshake struct {
	Hash            core.InfoHash
	RemoteOffersPEX bool // the peer's ut_pex, as far as the engine lets it count
	WeOfferPEX      bool // our side offers ut_pex on this connection
}

// ObserveHandshakes records every extended handshake the engine sees. Call it
// before adding torrents; read the result with the returned function.
func (e *Engine) ObserveHandshakes() (get func() []Handshake) {
	var mu sync.Mutex
	var seen []Handshake
	f := func(h core.InfoHash, remote, local bool) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, Handshake{h, remote, local})
	}
	e.handshakes.Store(&f)
	return func() []Handshake {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// DebugStatus is anacrolix's own status report, for diagnosing stalls.
func (e *Engine) DebugStatus() string {
	var b strings.Builder
	e.cl.WriteStatus(&b)
	return b.String()
}

// LocalPort is the port the engine listens on.
func (e *Engine) LocalPort() int { return e.cl.LocalPort() }

// ParseSpec runs the parsing an Add does on a spec, without starting anything.
func (e *Engine) ParseSpec(spec core.TorrentSpec) error {
	_, _, err := e.parseSpec(spec)
	return err
}
