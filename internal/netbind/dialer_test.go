package netbind_test

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/netbind"
)

func tcpEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln
}

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestDialerBindsTCPLocalAddress(t *testing.T) {
	ln := tcpEcho(t)
	d := netbind.Binding{Address: "127.0.0.1"}.Dialer(nil)

	c, err := d.DialContext(ctx5(t), "tcp", ln.Addr().String())

	require.NoError(t, err)
	defer c.Close()
	assert.Equal(t, "127.0.0.1", c.LocalAddr().(*net.TCPAddr).IP.String())
}

func TestDialerBindsUDPLocalAddress(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()
	d := netbind.Binding{Address: "127.0.0.1"}.Dialer(nil)

	c, err := d.DialContext(ctx5(t), "udp4", pc.LocalAddr().String())

	require.NoError(t, err)
	defer c.Close()
	assert.Equal(t, "127.0.0.1", c.LocalAddr().(*net.UDPAddr).IP.String())
}

func TestDialerFromAddressNotOnThisHostFailsInsteadOfFallingBack(t *testing.T) {
	ln := tcpEcho(t)
	d := netbind.Binding{Address: "192.0.2.77"}.Dialer(nil) // TEST-NET-1, never assigned

	c, err := d.DialContext(ctx5(t), "tcp", ln.Addr().String())

	if c != nil {
		_ = c.Close()
	}
	require.Error(t, err)
}

func TestDialerAddressFamilyMismatchFails(t *testing.T) {
	ln := tcpEcho(t)
	d := netbind.Binding{Address: "::1"}.Dialer(nil)

	c, err := d.DialContext(ctx5(t), "tcp4", ln.Addr().String())

	if c != nil {
		_ = c.Close()
	}
	require.Error(t, err)
}

func TestDialerKeepsBaseDialerSettings(t *testing.T) {
	ln := tcpEcho(t)
	base := &net.Dialer{Timeout: time.Second}
	called := 0
	base.Control = func(string, string, syscall.RawConn) error { called++; return nil }
	d := netbind.Binding{Address: "127.0.0.1"}.Dialer(base)

	c, err := d.DialContext(ctx5(t), "tcp", ln.Addr().String())

	require.NoError(t, err)
	_ = c.Close()
	assert.Equal(t, 1, called, "the base Control hook still runs")
	assert.Nil(t, base.LocalAddr, "the base dialer is not modified")
}

func TestListenConfigListensOnTheBoundAddress(t *testing.T) {
	b := netbind.Binding{Address: "127.0.0.1"}

	ln, err := b.ListenConfig().Listen(ctx5(t), "tcp", b.ListenAddr(0))

	require.NoError(t, err)
	defer ln.Close()
	assert.Equal(t, "127.0.0.1", ln.Addr().(*net.TCPAddr).IP.String())
}

func TestDialerWithHostileInterfaceNameFailsBeforeAnySocket(t *testing.T) {
	ln := tcpEcho(t)
	for _, name := range []string{"wg0\x00evil", "../x", "a b", "waytoolonginterfacename"} {
		d := netbind.Binding{Interface: name}.Dialer(nil)

		c, err := d.DialContext(ctx5(t), "tcp", ln.Addr().String())

		if c != nil {
			_ = c.Close()
		}
		require.Error(t, err, "%q", name)
		assert.ErrorIs(t, err, netbind.ErrBind, "%q", name)
	}
}
