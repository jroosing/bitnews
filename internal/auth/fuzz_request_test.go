package auth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// FuzzClientIP feeds hostile RemoteAddr and X-Forwarded-For values to the
// client IP resolver. It must not panic, and an untrusted peer's header must
// never change the answer.
func FuzzClientIP(f *testing.F) {
	f.Add("10.0.0.2:4000", "198.51.100.7, 10.0.0.3")
	f.Add("[::1]:80", "fe80::1%eth0")
	f.Add("203.0.113.9", ",,,")
	f.Add("", "")
	f.Add("[::ffff:10.0.0.1]:1", "::ffff:1.2.3.4")
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	f.Fuzz(func(t *testing.T, remote, xff string) {
		r := &http.Request{RemoteAddr: remote, Header: http.Header{"X-Forwarded-For": {xff}}}
		got := ClientIP(r, trusted)
		if got.IsValid() && (got.Is4In6() || got.Zone() != "") {
			t.Fatalf("not normalised: %v", got)
		}
		peer := peerAddr(remote)
		if peer.IsValid() && !contains(trusted, peer) && got != peer {
			t.Fatalf("untrusted peer %v: header changed the result to %v", peer, got)
		}
	})
}

// FuzzCSRFOrigin feeds hostile Origin, Host and allowed_hosts values to the
// cross-origin check. It must not panic, a request without Origin and
// Sec-Fetch-Site is always fine, and a cross-site one never is unless its
// origin is trusted.
func FuzzCSRFOrigin(f *testing.F) {
	f.Add("https://bitnews.lan", "bitnews.lan", "bitnews.lan")
	f.Add("null", "example.test", "")
	f.Add("http://[::1]:9494", "[::1]:9494", "[::1]")
	f.Add("https://user@x", "x", "x")
	f.Add("%zz://", "", "\x00")
	f.Fuzz(func(t *testing.T, origin, host, allowed string) {
		list := []string{allowed}
		r := httptest.NewRequest("POST", "http://example.test/", nil)
		r.Host = host
		if err := CheckOrigin(r, list); err != nil {
			t.Fatalf("a request without Origin or Sec-Fetch-Site must pass: %v", err)
		}
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://evil.invalid")
		if CheckOrigin(r, list) == nil && !hostInList("evil.invalid", list) {
			t.Fatal("a cross-site request must be refused")
		}
		r.Header.Set("Origin", origin)
		_ = CheckOrigin(r, list)
		_ = hostInList(host, []string{allowed})
	})
}
