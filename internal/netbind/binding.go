package netbind

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// ErrUnsupported is returned when a binding needs an OS feature this build
// lacks: binding to an interface works on Linux only. Address binding works
// everywhere. macOS has IP_BOUND_IF, which is not implemented.
var ErrUnsupported = errors.New("netbind: interface binding is not supported on this OS")

// ErrBind is wrapped by every error that means "the binding could not be
// applied to a socket". Callers must treat it as fatal for that socket and
// never retry unbound.
var ErrBind = errors.New("netbind: cannot apply binding")

// Binding is one protocol's binding. The zero value binds nothing.
type Binding struct {
	// Interface is an interface name such as "wg0" (SO_BINDTODEVICE on Linux).
	Interface string
	// Address is a local IP address, without port or zone.
	Address string
}

// Enabled reports whether any binding is requested.
func (b Binding) Enabled() bool { return b.Interface != "" || b.Address != "" }

// Validate checks the syntax of the interface name and the address. The
// name is checked here and again when it is applied, because it ends up in
// a socket option.
func (b Binding) Validate() error {
	if b.Interface != "" {
		if err := core.ValidInterfaceName(b.Interface); err != nil {
			return fmt.Errorf("netbind: interface %q: %w", b.Interface, err)
		}
	}
	if b.Address != "" {
		if _, err := b.ip(); err != nil {
			return err
		}
	}
	return nil
}

// ip parses Address. A zero Addr with a nil error means no address is set.
func (b Binding) ip() (netip.Addr, error) {
	if b.Address == "" {
		return netip.Addr{}, nil
	}
	a, err := netip.ParseAddr(b.Address)
	if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("netbind: address %q is not a usable local IP address", b.Address)
	}
	return a.Unmap(), nil
}

// ListenAddr is the "host:port" a listener of this binding listens on: the
// bound address, or every address (the interface restricts it) when only an
// interface is bound.
func (b Binding) ListenAddr(port int) string {
	return net.JoinHostPort(b.Address, strconv.Itoa(port))
}
