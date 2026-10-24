//go:build e2e

package daemon_test

import (
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

type portStatus struct {
	Port int  `json:"port"`
	Open bool `json:"open"`
}

func TestTorrentEndToEndAVPNHookMovesTheListenPortLive(t *testing.T) {
	e := newTorrentE2E(t)
	e.start()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	var set portStatus
	e.call("POST", "/api/v1/torrent/port", map[string]int{"port": port}, http.StatusOK, &set)

	assert.Equal(t, port, set.Port)
	assert.False(t, set.Open, "nobody connected in on the new port yet")
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	require.NoError(t, err, "the engine listens on the new port")
	_ = c.Close()
	require.Eventually(t, func() bool {
		var st portStatus
		e.call("GET", "/api/v1/torrent/port", nil, http.StatusOK, &st)
		return st.Open
	}, 5*time.Second, 50*time.Millisecond)
	var s core.Settings
	e.call("GET", "/api/v1/settings", nil, http.StatusOK, &s)
	assert.Equal(t, port, s.Torrent.ListenPort, "the port survives a restart")
}
