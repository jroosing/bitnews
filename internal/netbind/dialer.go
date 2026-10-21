package netbind

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
)

// Control returns a net.Dialer or net.ListenConfig Control hook that binds
// each socket to the interface before it connects or listens, or nil when no
// interface is set. On Linux it sets SO_BINDTODEVICE:
//
//   - Since Linux 5.7 an unprivileged process may bind its own sockets to an
//     interface; on older kernels this needs CAP_NET_RAW, and without it
//     the hook returns an error wrapping ErrBind.
//   - A missing interface, a failed setsockopt or an interface name that
//     fails validation makes the dial or listen fail. The socket is never
//     used unbound.
//
// On other systems a binding with an interface returns a hook that always
// fails with ErrUnsupported.
//
// The address part of the binding is applied by Dialer (LocalAddr) and
// ListenAddr, not by this hook.
func (b Binding) Control() func(network, address string, c syscall.RawConn) error {
	if b.Interface == "" {
		return nil
	}
	name := b.Interface
	return func(_, _ string, c syscall.RawConn) error { return bindToDevice(c, name) }
}

// ListenConfig returns a net.ListenConfig whose sockets carry the interface
// binding. Listen on ListenAddr(port) to get the address binding too.
func (b Binding) ListenConfig() *net.ListenConfig {
	return &net.ListenConfig{Control: b.Control()}
}

// Dialer dials with the binding: the socket is bound to the interface
// (Control) and connects from the bound local address. It satisfies the
// nntp.Dialer interface.
//
// The host name of the remote is resolved by the system resolver before the
// socket exists, so DNS lookups are not bound.
type Dialer struct {
	base net.Dialer
	ip   netip.Addr
	err  error
}

// Dialer returns a Dialer that dials like base (nil means a zero
// net.Dialer) with the binding applied. base is not modified; its Control
// hook still runs, after the interface binding.
func (b Binding) Dialer(base *net.Dialer) *Dialer {
	d := &Dialer{}
	if base != nil {
		d.base = *base
	}
	d.err = b.Validate()
	if d.err != nil {
		d.err = fmt.Errorf("%w: %w", ErrBind, d.err)
	}
	d.ip, _ = b.ip()
	if ours, theirs := b.Control(), d.base.Control; ours != nil {
		d.base.Control = func(network, address string, c syscall.RawConn) error {
			if err := ours(network, address, c); err != nil {
				return err
			}
			if theirs != nil {
				return theirs(network, address, c)
			}
			return nil
		}
	}
	return d
}

// DialContext implements nntp.Dialer. It fails when the binding is invalid
// or cannot be applied; it never dials unbound.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.err != nil {
		return nil, d.err
	}
	nd := d.base
	if d.ip.IsValid() {
		la, err := d.localAddr(network)
		if err != nil {
			return nil, err
		}
		nd.LocalAddr = la
	}
	return nd.DialContext(ctx, network, addr)
}

// localAddr is the local address for a dial on network. Networks without IP
// addresses (unix) have none.
func (d *Dialer) localAddr(network string) (net.Addr, error) {
	switch {
	case strings.HasSuffix(network, "4") && !d.ip.Is4():
		return nil, fmt.Errorf("%w: address %s is not IPv4 but the network is %s", ErrBind, d.ip, network)
	case strings.HasSuffix(network, "6") && !d.ip.Is6():
		return nil, fmt.Errorf("%w: address %s is not IPv6 but the network is %s", ErrBind, d.ip, network)
	}
	ip := net.IP(d.ip.AsSlice())
	switch {
	case strings.HasPrefix(network, "tcp"):
		return &net.TCPAddr{IP: ip}, nil
	case strings.HasPrefix(network, "udp"):
		return &net.UDPAddr{IP: ip}, nil
	case strings.HasPrefix(network, "ip"):
		return &net.IPAddr{IP: ip}, nil
	}
	return nil, nil
}
