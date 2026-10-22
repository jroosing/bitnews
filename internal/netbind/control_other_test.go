//go:build !linux

package netbind_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/netbind"
)

func TestInterfaceBindingIsUnsupportedOffLinuxAndDialFails(t *testing.T) {
	ln := tcpEcho(t)
	b := netbind.Binding{Interface: "lo0", Address: "127.0.0.1"}

	c, err := b.Dialer(nil).DialContext(ctx5(t), "tcp", ln.Addr().String())

	if c != nil {
		_ = c.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, netbind.ErrUnsupported)
	assert.ErrorIs(t, err, netbind.ErrBind)
}

func TestInterfaceBindingIsUnsupportedOffLinuxAndListenFails(t *testing.T) {
	b := netbind.Binding{Interface: "lo0"}

	ln, err := b.ListenConfig().Listen(ctx5(t), "tcp", b.ListenAddr(0))

	if ln != nil {
		_ = ln.Close()
	}
	var opErr *net.OpError
	require.ErrorAs(t, err, &opErr)
	assert.ErrorIs(t, err, netbind.ErrUnsupported)
}
