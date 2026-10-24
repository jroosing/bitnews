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
	"sync"
	"syscall"
	"time"

	anacrolix "github.com/anacrolix/torrent"
	"github.com/anacrolix/utp"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/netbind"
)

// ErrBindUnavailable is wrapped by the error New returns when the configured
// network binding cannot carry traffic right now (the interface is missing or
// down, or the address is not assigned). The engine never starts unbound.
var ErrBindUnavailable = errors.New("torrent: network binding unavailable")

// The engine opens its peer and DHT sockets itself, bound or not (see
// sockets), so that the listen port can change while it runs. A network
// binding covers these sockets of the engine, all opened with the binding's
// Control hook and bound address:
//
//   - the TCP peer listeners and every outgoing TCP peer dial,
//   - the uTP socket (listener and outgoing uTP dials), unless DisableUTP,
//   - the DHT sockets (shared with uTP when uTP is on), if DHT is on,
//   - HTTP tracker announces and metainfo fetches (HTTPDialContext,
//     TrackerDialContext) and UDP tracker sockets (TrackerListenPacket).
//
// anacrolix/torrent's own listeners cannot take a Control hook and cannot
// move to another port, so the client is always started with all of its own
// peer and DHT sockets off (DisableTCP, DisableUTP, NoDHT) and the engine
// attaches its own. Nothing is left to the unbound defaults: port mapping, web seeds
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

// utpPeerDialer dials peers over the current uTP socket of family i.
type utpPeerDialer struct {
	network string
	s       *sockets
	i       int
}

func (u utpPeerDialer) DialerNetwork() string { return u.network }
func (u utpPeerDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	f, _, err := u.s.current(u.i)
	if err != nil {
		return nil, err
	}
	return f.utp.DialContext(ctx, u.network, addr)
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

// sockets is the engine's set of listen sockets, one family per IP family.
// The client only ever sees the stable wrappers below (swapListener,
// swapPacketConn, utpPeerDialer), never a socket itself, so rebind can open
// sockets on another port and swap them in under a running client. That is
// how the listen port changes live, for example when a VPN hands out a new
// forwarded port.
type sockets struct {
	b   netbind.Binding
	cfg Config
	now func() time.Time

	mu     sync.Mutex
	fams   []*family
	gen    uint64 // bumped on every swap
	closed bool
	since  time.Time // when fams started listening
	lastIn time.Time // the last accepted connection on fams, zero if none
}

// errSocketsClosed is what the wrappers return once the engine closed its
// sockets.
var errSocketsClosed = net.ErrClosed

// current returns family i of the current set and its generation.
func (s *sockets) current(i int) (*family, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.fams[i], s.gen, errSocketsClosed
	}
	return s.fams[i], s.gen, nil
}

// swapped reports whether the sockets of generation gen were replaced (or
// closed) since: an error from one of them then only means it was closed
// under its reader.
func (s *sockets) swapped(gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen != gen && !s.closed
}

// accepted records an incoming connection on the sockets of generation gen.
func (s *sockets) accepted(gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen == gen {
		s.lastIn = s.now()
	}
}

func (s *sockets) port() int {
	return s.fams[0].tcp.Addr().(*net.TCPAddr).Port
}

func (s *sockets) status() core.ListenPortStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return core.ListenPortStatus{Port: s.port(), Since: s.since, LastIncoming: s.lastIn}
}

// rebind opens every family on port and swaps the new sockets in. If any
// family cannot be opened the old sockets stay in use. Connections made over
// the old TCP listener live on; uTP connections over the old socket end.
func (s *sockets) rebind(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if port == s.port() {
		return nil
	}
	next := make([]*family, 0, len(s.fams))
	for _, f := range s.fams {
		nf, err := openFamily(s.b, s.cfg, f.network, port)
		if err != nil {
			for _, n := range next {
				n.close()
			}
			return fmt.Errorf("torrent: listen on port %d: %w", port, err)
		}
		next = append(next, nf)
	}
	old := s.fams
	s.fams, s.gen = next, s.gen+1
	s.since, s.lastIn = s.now(), time.Time{}
	for _, f := range old {
		f.close()
	}
	return nil
}

func (s *sockets) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for _, f := range s.fams {
		f.close()
	}
	return nil
}

// swapListener is the anacrolix Listener of family i: the TCP listener, or
// the uTP socket when utp is set.
type swapListener struct {
	s   *sockets
	i   int
	utp bool
}

func (l swapListener) get(f *family) net.Listener {
	if l.utp {
		return f.utp
	}
	return f.tcp
}

func (l swapListener) Accept() (net.Conn, error) {
	for {
		f, gen, err := l.s.current(l.i)
		if err != nil {
			return nil, err
		}
		c, err := l.get(f).Accept()
		if err == nil {
			l.s.accepted(gen)
			return c, nil
		}
		if !l.s.swapped(gen) {
			return nil, err
		}
	}
}

func (l swapListener) Addr() net.Addr {
	f, _, _ := l.s.current(l.i)
	return l.get(f).Addr()
}

// swapPacketConn is the DHT socket of family i: the uTP socket, or a plain
// UDP socket without uTP.
type swapPacketConn struct {
	s *sockets
	i int
}

func (p swapPacketConn) conn() net.PacketConn {
	f, _, _ := p.s.current(p.i)
	return f.dhtConn
}

func (p swapPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		f, gen, err := p.s.current(p.i)
		if err != nil {
			return 0, nil, err
		}
		n, addr, err := f.dhtConn.ReadFrom(b)
		if err == nil || !p.s.swapped(gen) {
			return n, addr, err
		}
	}
}

func (p swapPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	return p.conn().WriteTo(b, addr)
}

// Close does nothing: the sockets belong to the engine, which closes them
// after the DHT servers.
func (p swapPacketConn) Close() error                       { return nil }
func (p swapPacketConn) LocalAddr() net.Addr                { return p.conn().LocalAddr() }
func (p swapPacketConn) SetDeadline(t time.Time) error      { return p.conn().SetDeadline(t) }
func (p swapPacketConn) SetReadDeadline(t time.Time) error  { return p.conn().SetReadDeadline(t) }
func (p swapPacketConn) SetWriteDeadline(t time.Time) error { return p.conn().SetWriteDeadline(t) }

// attachSockets opens the engine's sockets and hands the client wrappers of
// them. The returned closers are the engine's to close after the client: the
// DHT servers first, then the sockets.
func attachSockets(cl *anacrolix.Client, cfg Config, log *slog.Logger) (*sockets, []io.Closer, error) {
	b := cfg.binding()
	fams, err := openBound(b, cfg, log)
	if err != nil {
		return nil, nil, fmt.Errorf("torrent: open listen sockets: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	s := &sockets{b: b, cfg: cfg, now: now, fams: fams, since: now()}
	var closers []io.Closer
	closeAll := func() {
		for _, closer := range slices.Backward(closers) {
			_ = closer.Close()
		}
		_ = s.Close()
	}
	dialer := b.Dialer(&net.Dialer{FallbackDelay: -1, KeepAlive: -1})
	for i, f := range fams {
		cl.AddListener(swapListener{s: s, i: i})
		cl.AddDialer(tcpPeerDialer{network: "tcp" + f.network, d: dialer})
		if f.utp != nil {
			cl.AddListener(swapListener{s: s, i: i, utp: true})
			cl.AddDialer(utpPeerDialer{network: "udp" + f.network, s: s, i: i})
		}
	}
	for i, f := range fams {
		if f.dhtConn == nil {
			continue
		}
		ds, err := cl.NewAnacrolixDhtServer(swapPacketConn{s: s, i: i})
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("torrent: start DHT: %w", err)
		}
		cl.AddDhtServer(anacrolix.AnacrolixDhtServerWrapper{Server: ds})
		closers = append(closers, closerFunc(func() error { ds.Close(); return nil }))
	}
	closers = append(closers, s)
	log.Info("torrent: listening",
		"interface", b.Interface, "address", b.Address, "port", s.port(),
		"utp", !cfg.DisableUTP, "dht", cfg.DHT)
	return s, closers, nil
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }
