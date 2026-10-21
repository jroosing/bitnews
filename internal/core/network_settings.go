package core

import (
	"fmt"
	"net/netip"
)

// MaxInterfaceNameLen is the longest network interface name Linux accepts
// (IFNAMSIZ minus the trailing NUL).
const MaxInterfaceNameLen = 15

// NetworkSettings is the optional outbound network binding of each protocol.
// Binding is a kill switch: when a protocol is bound, its sockets can only
// use the named interface and/or the local address, so if the interface (a
// VPN, for instance) goes away the protocol has no route and stops, instead
// of leaving through another interface. Both bindings are off by default.
//
// The bindings affect outbound NNTP connections and BitTorrent sockets
// (peers, uTP, DHT, tracker announces) only. The API and UI listener is
// never bound, whatever these say. DNS lookups use the system resolver and
// are not bound.
//
// Settings stored before this key existed load with both bindings off.
type NetworkSettings struct {
	Usenet  ProtocolBinding `json:"usenet"`
	Torrent ProtocolBinding `json:"torrent"`
}

// ProtocolBinding binds one protocol's sockets. Both fields may be empty
// (no binding); setting either turns the binding on.
type ProtocolBinding struct {
	// Interface is the network interface name, such as "wg0" or "tun0".
	// On Linux it is applied with SO_BINDTODEVICE; elsewhere startup with
	// an interface set fails rather than silently not binding.
	Interface string `json:"interface"`
	// Address is the local IP address, without port or zone, that sockets
	// bind to. When Interface is also set the address should belong to it.
	Address string `json:"address"`
}

// Enabled reports whether the protocol is bound at all.
func (b ProtocolBinding) Enabled() bool { return b.Interface != "" || b.Address != "" }

// validate returns one error per problem, naming the setting by prefix such
// as "network.usenet".
func (b ProtocolBinding) validate(prefix string) []error {
	var problems []error
	if b.Interface != "" {
		if err := ValidInterfaceName(b.Interface); err != nil {
			problems = append(problems, fmt.Errorf("%s.interface %q: %w", prefix, b.Interface, err))
		}
	}
	if b.Address != "" {
		if err := validBindAddress(b.Address); err != nil {
			problems = append(problems, fmt.Errorf("%s.address %q: %w", prefix, b.Address, err))
		}
	}
	return problems
}

func (n NetworkSettings) validate() []error {
	return append(n.Usenet.validate("network.usenet"), n.Torrent.validate("network.torrent")...)
}

// ValidInterfaceName reports why name cannot be a network interface name:
// at most 15 bytes of [A-Za-z0-9_.:-]. It exists so a hostile name never
// reaches a socket option.
func ValidInterfaceName(name string) error {
	if name == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(name) > MaxInterfaceNameLen {
		return fmt.Errorf("longer than %d characters", MaxInterfaceNameLen)
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == ':', c == '-':
		default:
			return fmt.Errorf("character %q is not allowed (use letters, digits and _ . : -)", c)
		}
	}
	return nil
}

func validBindAddress(s string) error {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return fmt.Errorf("not an IP address")
	}
	switch {
	case a.Zone() != "":
		return fmt.Errorf("must not have a zone")
	case a.IsUnspecified():
		return fmt.Errorf("must not be the unspecified address")
	case a.IsMulticast():
		return fmt.Errorf("must not be a multicast address")
	}
	return nil
}
