package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"syscall"

	anacrolix "github.com/anacrolix/torrent"
	"github.com/anacrolix/utp"

	"gitlab.com/kyura4k/gitnews/internal/netbind"
)

// ErrBindUnavailable is wrapped by the error New returns when the configured
// network binding cannot carry traffic right now (the interface is missing or
// down, or the address is not assigned). The engine never starts unbound.
var ErrBindUnavailable = errors.New("torrent: network binding unavailable")

// Network binding covers these sockets of the engine, all opened with the
// binding's Control hook and bound address:
//
//   - the TCP peer listeners and every outgoing TCP peer dial,
//   - the uTP socket (listener and outgoing uTP dials), unless DisableUTP,
//   - the DHT sockets (shared with uTP when uTP is on), if DHT is on,
//   - HTTP tracker announces and metainfo fetches (HTTPDialContext,
//     TrackerDialContext) and UDP tracker sockets (TrackerListenPacket).
//
// anacrolix/torrent's own listeners cannot take a Control hook, so with a
// binding the client is started with all of its own peer and DHT sockets
// off (DisableTCP, DisableUTP, NoDHT) and the engine attaches the bound ones
// itself. Nothing is left to the unbound defaults: port mapping, web seeds
// and WebTorrent are off for every engine.
//
// Not covered: DNS lookups (tracker, DHT bootstrap and peer host names) use
// the system resolver and are not bound. See docs/network-binding.md.

// binding is the effective binding: Config.Binding, with the legacy
// BindAddr as its address when Binding has none.
func (c Config) binding() netbind.Binding {
	b := c.Binding
	if b.Address == "" {
		b.Address = c.BindAddr
	}
	return b
}

// checkBinding fails with ErrBindUnavailable when the binding is not usable
// right now.
func checkBinding(cfg Config) error {
	b := cfg.binding()
	if !b.Enabled() {
		return nil
	}
	if st := b.CheckNow(); !st.Available {
		return fmt.Errorf("%w: %s", ErrBindUnavailable, st.Reason)
	}
	return nil
}

// families lists the IP families ("4", "6") the binding opens sockets for:
// one when the address pins it, both otherwise.
func families(b netbind.Binding) []string {
	if b.Address != "" {
		if a, err := netip.ParseAddr(b.Address); err == nil {
			if a.Unmap().Is4() {
				return []string{"4"}
			}
			return []string{"6"}
		}
	}
	return []string{"4", "6"}
}

// tcpPeerDialer dials peers over TCP through a bound dialer. It is an
// anacrolix Dialer.
type tcpPeerDialer struct {
	network string
	d       *netbind.Dialer
}

func (t tcpPeerDialer) DialerNetwork() string { return t.network }
func (t tcpPeerDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return t.d.DialContext(ctx, t.network, addr)
}

// utpPeerDialer dials peers over a bound uTP socket.
type utpPeerDialer struct {
	network string
	s       *utp.Socket
}

func (u utpPeerDialer) DialerNetwork() string { return u.network }
func (u utpPeerDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return u.s.DialContext(ctx, u.network, addr)
}

// family is the set of bound sockets of one IP family.
type family struct {
	network string // "4" or "6"
	tcp     net.Listener
	utp     *utp.Socket
	dhtConn net.PacketConn // the uTP socket, a plain UDP socket, or nil
	ownDHT  bool           // dhtConn is not the uTP socket
}

func (f *family) close() {
	if f.tcp != nil {
		_ = f.tcp.Close()
	}
	if f.utp != nil {
		_ = f.utp.Close()
	}
	if f.ownDHT && f.dhtConn != nil {
		_ = f.dhtConn.Close()
	}
}

func openFamily(b netbind.Binding, cfg Config, v string, port int) (*family, error) {
	lc := b.ListenConfig()
	ctx := context.Background()
	f := &family{network: v}
	ln, err := lc.Listen(ctx, "tcp"+v, b.ListenAddr(port))
	if err != nil {
		return nil, fmt.Errorf("listen tcp%s: %w", v, err)
	}
	f.tcp = ln
	if port == 0 {
		port = ln.Addr().(*net.TCPAddr).Port
	}
	if !cfg.DisableUTP || cfg.DHT {
		pc, err := lc.ListenPacket(ctx, "udp"+v, b.ListenAddr(port))
		if err != nil {
			f.close()
			return nil, fmt.Errorf("listen udp%s: %w", v, err)
		}
		if cfg.DisableUTP {
			f.dhtConn, f.ownDHT = pc, true
		} else {
			s, err := utp.NewSocketFromPacketConn(pc)
			if err != nil {
				_ = pc.Close()
				f.close()
				return nil, fmt.Errorf("start utp%s: %w", v, err)
			}
			f.utp = s
			if cfg.DHT {
				f.dhtConn = s
			}
		}
	}
	return f, nil
}

// openBound opens the bound sockets of every family. A random port (zero)
// is shared by all of them and retried when the UDP port is taken.
func openBound(b netbind.Binding, cfg Config, log *slog.Logger) ([]*family, error) {
	var lastErr error
	for range 8 {
		var fams []*family
		port := cfg.ListenPort
		var err error
		for i, v := range families(b) {
			var f *family
			f, err = openFamily(b, cfg, v, port)
			if err != nil {
				// The second family is optional (a host may have no IPv6),
				// but a failed binding is never skipped.
				if i > 0 && !errors.Is(err, netbind.ErrBind) {
					log.Info("torrent: ip family unavailable, skipped", "family", "IPv"+v, "error", err.Error())
					err = nil
					continue
				}
				break
			}
			fams = append(fams, f)
			if port == 0 {
				port = f.tcp.Addr().(*net.TCPAddr).Port
			}
		}
		if err == nil {
			return fams, nil
		}
		for _, f := range fams {
			f.close()
		}
		lastErr = err
		if !(cfg.ListenPort == 0 && errors.Is(err, syscall.EADDRINUSE)) {
			break
		}
	}
	return nil, lastErr
}

// attachBound opens the bound sockets and hands them to the client. The
// returned closers are the engine's to close after the client.
func attachBound(cl *anacrolix.Client, cfg Config, log *slog.Logger) ([]io.Closer, error) {
	b := cfg.binding()
	fams, err := openBound(b, cfg, log)
	if err != nil {
		return nil, fmt.Errorf("torrent: open bound sockets: %w", err)
	}
	var closers []io.Closer
	closeAll := func() {
		for _, f := range fams {
			f.close()
		}
		for _, closer := range slices.Backward(closers) {
			_ = closer.Close()
		}
	}
	dialer := b.Dialer(&net.Dialer{FallbackDelay: -1, KeepAlive: -1})
	for _, f := range fams {
		cl.AddListener(f.tcp)
		cl.AddDialer(tcpPeerDialer{network: "tcp" + f.network, d: dialer})
		if f.utp != nil {
			cl.AddListener(f.utp)
			cl.AddDialer(utpPeerDialer{network: "udp" + f.network, s: f.utp})
		}
	}
	for _, f := range fams {
		if f.dhtConn == nil {
			continue
		}
		ds, err := cl.NewAnacrolixDhtServer(f.dhtConn)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("torrent: start bound DHT: %w", err)
		}
		cl.AddDhtServer(anacrolix.AnacrolixDhtServerWrapper{Server: ds})
		closers = append(closers, closerFunc(func() error { ds.Close(); return nil }))
	}
	for _, f := range fams {
		closers = append(closers, closerFunc(func() error { f.close(); return nil }))
	}
	log.Info("torrent: network binding active",
		"interface", b.Interface, "address", b.Address, "port", cl.LocalPort(),
		"utp", !cfg.DisableUTP, "dht", cfg.DHT)
	return closers, nil
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }
