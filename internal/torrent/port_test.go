package torrent_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/torrent"
)

// freePort returns a TCP and UDP port on 127.0.0.1 that nothing listens on
// right now.
func freePort(t *testing.T) int {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		require.NoError(t, err)
		p := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if pc, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(p))); err == nil {
			_ = pc.Close()
			return p
		}
	}
	require.FailNow(t, "no free port")
	return 0
}

func dialPort(port int) error {
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err == nil {
		_ = c.Close()
	}
	return err
}

func TestSetListenPortMovesTheListenerWithoutARestart(t *testing.T) {
	t.Parallel()
	e := newEngine(t)
	old := e.Port()
	p := freePort(t)

	require.NoError(t, e.SetListenPort(p))

	assert.Equal(t, p, e.Port())
	assert.Equal(t, p, e.ListenPortStatus().Port)
	require.NoError(t, dialPort(p), "the new port accepts peers")
	assert.Error(t, dialPort(old), "the old port is closed")
}

func TestSetListenPortWorksWithoutANetworkBinding(t *testing.T) {
	t.Parallel()
	e := newEngine(t, func(c *torrent.Config) { c.BindAddr = "" })
	p := freePort(t)

	require.NoError(t, e.SetListenPort(p))

	assert.Equal(t, p, e.Port())
	require.NoError(t, dialPort(p))
}

func TestSetListenPortToATakenPortKeepsTheOldOne(t *testing.T) {
	t.Parallel()
	e := newEngine(t)
	old := e.Port()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port

	err = e.SetListenPort(taken)

	require.Error(t, err)
	assert.Equal(t, old, e.Port())
	require.NoError(t, dialPort(old), "the old port still accepts peers")
}

func TestSetListenPortRefusesPortsOutOfRange(t *testing.T) {
	t.Parallel()
	e := newEngine(t)
	for _, p := range []int{0, -1, 65536} {
		assert.ErrorIs(t, e.SetListenPort(p), torrent.ErrInvalidConfig, "port %d", p)
	}
}

func TestSetListenPortToTheSamePortIsANoOp(t *testing.T) {
	t.Parallel()
	e := newEngine(t)
	require.NoError(t, dialPort(e.Port()))
	require.Eventually(t, func() bool { return !e.ListenPortStatus().LastIncoming.IsZero() }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, e.SetListenPort(e.Port()))

	assert.False(t, e.ListenPortStatus().LastIncoming.IsZero(), "the sockets were not reopened")
}

func TestAnIncomingConnectionShowsThePortIsOpen(t *testing.T) {
	t.Parallel()
	e := newEngine(t)
	st := e.ListenPortStatus()
	assert.True(t, st.LastIncoming.IsZero(), "nobody connected in yet")
	assert.False(t, st.Since.IsZero())

	require.NoError(t, dialPort(e.Port()))

	require.Eventually(t, func() bool { return e.ListenPortStatus().Open() }, 5*time.Second, 10*time.Millisecond)
}

func TestANewPortStartsUnconfirmed(t *testing.T) {
	t.Parallel()
	e := newEngine(t)
	require.NoError(t, dialPort(e.Port()))
	require.Eventually(t, func() bool { return e.ListenPortStatus().Open() }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, e.SetListenPort(freePort(t)))

	assert.False(t, e.ListenPortStatus().Open(), "an incoming connection on the old port says nothing about the new one")
}

func TestSwarmReachesASeederOnItsNewPort(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "moved.mkv", map[string]int{"": 60_000})
	seeder := seed(t, c)
	require.NoError(t, seeder.SetListenPort(freePort(t)))
	leech := newEngine(t)
	dir := t.TempDir()

	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)

	c.assertFilesEqual(t, dir)
	assert.True(t, seeder.ListenPortStatus().Open(), "the leecher connected in")
}

func TestSetListenPortAfterCloseFails(t *testing.T) {
	t.Parallel()
	e := newEngine(t)
	require.NoError(t, e.Close())

	assert.ErrorIs(t, e.SetListenPort(freePort(t)), torrent.ErrClosed)
}

func TestSetListenPortMovesUTPAndTheDHTToo(t *testing.T) {
	t.Parallel()
	e := newEngine(t, func(c *torrent.Config) {
		c.DisableUTP = false
		c.DHT = true
		c.DHTBootstrap = []string{"127.0.0.1:9"}
	})
	p := freePort(t)

	require.NoError(t, e.SetListenPort(p))

	for _, a := range append(e.ListenAddrs(), e.DHTAddrs()...) {
		_, port, err := net.SplitHostPort(a.String())
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(p), port, "%s %s", a.Network(), a)
	}
}
