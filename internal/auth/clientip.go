package auth

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// ParsePrefixes parses CIDR strings, as stored in the settings, and skips the
// ones that do not parse (Settings.Validate already rejects them on save).
func ParsePrefixes(cidrs []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

// ClientIP returns the address of the client that made r.
//
// It is the peer address (RemoteAddr) unless that peer is inside trusted: only
// then is X-Forwarded-For believed. The header is read from the right, the
// way each trusted proxy appended to it, skipping entries that are themselves
// trusted hops; the first untrusted entry is the client. Entries to the left
// of it are whatever the client claimed and are never used, so a spoofed
// header cannot pick the result.
//
// Anything odd falls back to the peer address: an unparseable entry, or a
// chain made only of trusted hops. IPv4-mapped IPv6 addresses are unmapped
// and zones are dropped. The zero Addr comes back only when RemoteAddr itself
// cannot be parsed.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := peerAddr(r.RemoteAddr)
	if !peer.IsValid() || !contains(trusted, peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for _, hop := range slices.Backward(hops) {
		a, err := netip.ParseAddr(strings.TrimSpace(hop))
		if err != nil {
			return peer
		}
		a = normalize(a)
		if !contains(trusted, a) {
			return a
		}
	}
	return peer
}

func peerAddr(remote string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return normalize(ap.Addr())
	}
	if a, err := netip.ParseAddr(remote); err == nil {
		return normalize(a)
	}
	return netip.Addr{}
}

func normalize(a netip.Addr) netip.Addr { return a.Unmap().WithZone("") }

func contains(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
