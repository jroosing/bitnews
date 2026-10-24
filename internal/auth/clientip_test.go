package auth_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/auth"
)

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

func reqFrom(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientIPUsesRemoteAddrWithoutTrustedProxies(t *testing.T) {
	r := reqFrom("203.0.113.9:4000", "198.51.100.7")

	assert.Equal(t, netip.MustParseAddr("203.0.113.9"), auth.ClientIP(r, nil))
}

func TestClientIPIgnoresForwardedForFromAnUntrustedPeer(t *testing.T) {
	r := reqFrom("203.0.113.9:4000", "192.168.1.5")

	got := auth.ClientIP(r, prefixes(t, "10.0.0.0/8"))

	assert.Equal(t, netip.MustParseAddr("203.0.113.9"), got, "a spoofed header must not pick the client")
}

func TestClientIPHonoursForwardedForFromATrustedPeer(t *testing.T) {
	r := reqFrom("10.0.0.2:4000", "198.51.100.7")

	got := auth.ClientIP(r, prefixes(t, "10.0.0.0/8"))

	assert.Equal(t, netip.MustParseAddr("198.51.100.7"), got)
}

func TestClientIPWalksForwardedForFromTheRightSkippingTrustedHops(t *testing.T) {
	// client-supplied "192.168.1.5" on the left is ignored: the rightmost
	// untrusted entry is the one our own proxy chain saw.
	r := reqFrom("10.0.0.2:4000", "192.168.1.5, 198.51.100.7, 10.0.0.3")

	got := auth.ClientIP(r, prefixes(t, "10.0.0.0/8"))

	assert.Equal(t, netip.MustParseAddr("198.51.100.7"), got)
}

func TestClientIPJoinsRepeatedForwardedForHeaders(t *testing.T) {
	r := reqFrom("10.0.0.2:4000", "192.168.1.5", "198.51.100.7, 10.0.0.3")

	got := auth.ClientIP(r, prefixes(t, "10.0.0.0/8"))

	assert.Equal(t, netip.MustParseAddr("198.51.100.7"), got)
}

func TestClientIPFallsBackToRemoteAddrOnAnInvalidForwardedEntry(t *testing.T) {
	for _, xff := range []string{"not-an-ip", "198.51.100.7, garbage", "", ",", "198.51.100.7:80"} {
		r := reqFrom("10.0.0.2:4000", xff)

		got := auth.ClientIP(r, prefixes(t, "10.0.0.0/8"))

		assert.Equal(t, netip.MustParseAddr("10.0.0.2"), got, "XFF %q", xff)
	}
}

func TestClientIPFallsBackToRemoteAddrWhenEveryHopIsTrusted(t *testing.T) {
	r := reqFrom("10.0.0.2:4000", "10.0.0.9, 10.0.0.3")

	got := auth.ClientIP(r, prefixes(t, "10.0.0.0/8"))

	assert.Equal(t, netip.MustParseAddr("10.0.0.2"), got)
}

func TestClientIPNormalisesIPv4MappedAndZonedAddresses(t *testing.T) {
	r := reqFrom("[::ffff:203.0.113.9]:4000")
	assert.Equal(t, netip.MustParseAddr("203.0.113.9"), auth.ClientIP(r, nil))

	r = reqFrom("10.0.0.2:4000", "fe80::1%eth0")
	assert.Equal(t, netip.MustParseAddr("fe80::1"), auth.ClientIP(r, prefixes(t, "10.0.0.0/8")))
}

func TestClientIPReturnsZeroAddrWhenRemoteAddrIsUnparseable(t *testing.T) {
	r := reqFrom("pipe")

	assert.False(t, auth.ClientIP(r, nil).IsValid())
}

func TestClientIPAcceptsRemoteAddrWithoutPort(t *testing.T) {
	r := reqFrom("203.0.113.9")

	assert.Equal(t, netip.MustParseAddr("203.0.113.9"), auth.ClientIP(r, nil))
}

func TestParsePrefixesSkipsInvalidEntries(t *testing.T) {
	got := auth.ParsePrefixes([]string{"10.0.0.0/8", "nope", "2001:db8::/32", ""})

	assert.Equal(t, prefixes(t, "10.0.0.0/8", "2001:db8::/32"), got)
}
