package web

import (
	"net/netip"
	"testing"
)

func FuzzParseNetworks(f *testing.F) {
	for _, s := range []string{"192.168.1.0/24", "10.0.0.1\n::1", "0.0.0.0/0, fe80::1%eth0", "x/y", "\x00/8", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		nets, bad := parseNetworks(raw)
		for _, n := range nets {
			p, err := netip.ParsePrefix(n)
			if err != nil || p != p.Masked() {
				t.Fatalf("parseNetworks returned %q, not a masked prefix", n)
			}
		}
		_ = bad
	})
}

func FuzzParseHosts(f *testing.F) {
	for _, s := range []string{"bitnews.lan", "192.168.1.10:9494", "[::1]:80", "http://x/", "a..b", "x:99999", "\x00", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		hosts, _ := parseHosts(raw)
		for _, h := range hosts {
			if !validHostEntry(h) || h != lower(h) {
				t.Fatalf("parseHosts returned %q", h)
			}
		}
	})
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
