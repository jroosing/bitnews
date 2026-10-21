// Package netbind binds a protocol's outbound sockets to a network
// interface and/or a local address, as an optional kill switch for VPN
// setups. See docs/network-binding.md.
//
// A Binding produces net.Dialer and net.ListenConfig values whose sockets
// are bound before they connect or listen. If a requested binding cannot be
// applied the dial or listen fails; it never falls back to an unbound
// socket. A Monitor watches the interface and a Killswitch turns its
// transitions into pause and resume calls.
//
// The package knows nothing about the API listener, which is never bound.
package netbind
