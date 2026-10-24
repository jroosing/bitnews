package netbind_test

import (
	"context"
	"net"
	"regexp"
	"testing"
	"time"

	"github.com/jroosing/bitnews/internal/netbind"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// FuzzBinding checks that nothing hostile gets past Validate and that a
// Binding built from arbitrary text never panics or dials.
func FuzzBinding(f *testing.F) {
	for _, s := range []string{"", "wg0", "../etc", "wg0\x00", "a b", "10.0.0.1", "::1", "fe80::1%eth0", "0.0.0.0", "ff02::1", "1.2.3.4/8", "é"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, iface, addr string) {
		b := netbind.Binding{Interface: iface, Address: addr}
		err := b.Validate()
		if err == nil {
			if iface != "" && !safeName.MatchString(iface) {
				t.Fatalf("Validate accepted interface %q", iface)
			}
			if addr != "" {
				ip := net.ParseIP(addr)
				if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
					t.Fatalf("Validate accepted address %q", addr)
				}
			}
		}
		_ = b.ListenAddr(1)
		_ = b.Check(nil)
		if err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if c, derr := b.Dialer(nil).DialContext(ctx, "tcp", "127.0.0.1:1"); derr == nil {
				_ = c.Close()
				t.Fatalf("an invalid binding %+v dialed", b)
			}
		}
	})
}
