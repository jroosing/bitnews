package nntp

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/netbind"
)

var _ Dialer = (*netbind.Dialer)(nil)

// The interface and address binding of M5-07 plugs in through Config.Dialer:
// the TLS handshake and the NNTP session run over the bound connection.
func TestDialOverNetbindDialerConnectsFromTheBoundAddress(t *testing.T) {
	srv := newFakeServer(t, func(s *session) { greetOK(s); s.readCmd() })
	cfg := srv.config()
	serverAddr := srv.ln.Addr().String()
	bound := netbind.Binding{Address: "127.0.0.1"}.Dialer(nil)
	var local net.Addr
	cfg.Dialer = dialerFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		c, err := bound.DialContext(ctx, network, serverAddr)
		if err == nil {
			local = c.LocalAddr()
		}
		return c, err
	})

	c := dialCfg(t, cfg)

	require.NoError(t, c.Close())
	require.Equal(t, "127.0.0.1", local.(*net.TCPAddr).IP.String())
}

func TestDialOverNetbindDialerFailsClosedWhenTheBindingCannotBeApplied(t *testing.T) {
	srv := newFakeServer(t, neverGreets)
	cfg := srv.config()
	serverAddr := srv.ln.Addr().String()
	bound := netbind.Binding{Address: "192.0.2.77"}.Dialer(nil) // not assigned to this host
	cfg.Dialer = dialerFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return bound.DialContext(ctx, network, serverAddr)
	})

	c, err := Dial(context.Background(), cfg)

	require.Nil(t, c)
	require.ErrorIs(t, err, ErrConnection)
}
