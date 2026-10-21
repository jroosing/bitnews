package netbind_test

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/netbind"
)

// skipIfNotPermitted skips when the kernel or sandbox refuses SO_BINDTODEVICE
// for this user (Linux before 5.7 needs CAP_NET_RAW).
func skipIfNotPermitted(t *testing.T, err error) {
	t.Helper()
	if err != nil && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) {
		t.Skipf("SO_BINDTODEVICE not permitted: %v", err)
	}
}

func TestDialerBoundToLoopbackInterfaceConnects(t *testing.T) {
	ln := tcpEcho(t)
	b := netbind.Binding{Interface: "lo", Address: "127.0.0.1"}

	c, err := b.Dialer(nil).DialContext(ctx5(t), "tcp", ln.Addr().String())

	skipIfNotPermitted(t, err)
	require.NoError(t, err)
	_ = c.Close()
}

func TestListenerBoundToLoopbackInterfaceAccepts(t *testing.T) {
	b := netbind.Binding{Interface: "lo"}
	ln, err := b.ListenConfig().Listen(ctx5(t), "tcp4", b.ListenAddr(0))
	skipIfNotPermitted(t, err)
	require.NoError(t, err)
	defer ln.Close()

	c, err := net.Dial("tcp4", ln.Addr().String())

	require.NoError(t, err)
	_ = c.Close()
}

func TestDialerBoundToMissingInterfaceFailsAndDoesNotFallBack(t *testing.T) {
	ln := tcpEcho(t)
	b := netbind.Binding{Interface: "nobind0"}

	c, err := b.Dialer(nil).DialContext(ctx5(t), "tcp", ln.Addr().String())

	if c != nil {
		_ = c.Close()
	}
	require.Error(t, err, "a reachable server must not be dialed unbound")
	assert.ErrorIs(t, err, netbind.ErrBind)
}

func TestListenerBoundToMissingInterfaceFails(t *testing.T) {
	b := netbind.Binding{Interface: "nobind0"}

	ln, err := b.ListenConfig().Listen(ctx5(t), "tcp4", b.ListenAddr(0))

	if ln != nil {
		_ = ln.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, netbind.ErrBind)
}
