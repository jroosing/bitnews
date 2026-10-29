package blackbox_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quickShutdown is how fast a daemon with nothing in flight must be gone
// after the signal. The Go daemon takes a few milliseconds; the grace period
// for requests in flight is 10 seconds and must not be spent here.
const quickShutdown = 3 * time.Second

func TestShutdownOnSIGTERMIsCleanAndFast(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))
	d.get("/api/v1/health", nil)

	r, took := d.terminate()

	assert.Equal(t, 0, r.code, r.stderr)
	assert.Less(t, took, quickShutdown)
	assert.Contains(t, r.stderr, `msg="shutdown complete"`)
	assert.Empty(t, r.stdout)
	_, err := net.DialTimeout("tcp", d.addr, time.Second)
	assert.Error(t, err, "the port is closed once the daemon has exited")
}

func TestShutdownOnSIGINTIsCleanToo(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	require.NoError(t, d.cmd.Process.Signal(syscall.SIGINT))
	r := d.wait(exitTimeout)

	assert.Equal(t, 0, r.code, r.stderr)
	assert.Contains(t, r.stderr, `msg="shutdown complete"`)
}

func TestShutdownClosesAnIdleKeepAliveConnection(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))
	conn, err := net.Dial("tcp", d.addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)

	// One request on a kept-alive connection, then the connection sits idle.
	_, err = io.WriteString(conn, "GET /api/v1/health HTTP/1.1\r\nHost: x\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, healthBody, string(body))

	r, took := d.terminate()

	assert.Equal(t, 0, r.code, r.stderr)
	assert.Less(t, took, quickShutdown, "an idle connection does not hold the shutdown")
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = br.ReadByte()
	assert.ErrorIs(t, err, io.EOF, "the daemon closed the idle connection")
}

func TestShutdownClosesAConnectionThatNeverSentARequest(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))
	conn, err := net.Dial("tcp", d.addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Let the daemon accept the connection first, or it may not know of it
	// when the signal comes.
	time.Sleep(300 * time.Millisecond)

	// The Go daemon counts such a connection as idle once it is 5 seconds
	// old, so it may take that long; the grace period is 10 seconds.
	require.NoError(t, d.cmd.Process.Signal(syscall.SIGTERM))
	r := d.wait(9 * time.Second)

	assert.Equal(t, 0, r.code, r.stderr)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = conn.Read(make([]byte, 1))
	assert.True(t, errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET), "the connection is closed, got %v", err)
}

func TestShutdownEndsAnOpenEventStream(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	conn, err := net.Dial("tcp", d.addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "GET /api/v1/events HTTP/1.1\r\nHost: x\r\nAccept: text/event-stream\r\nAuthorization: Bearer "+cfg.AppKey+"\r\n\r\n")
	require.NoError(t, err)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	// The first event is the hello with the ID of this process.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	var seen strings.Builder
	for !strings.Contains(seen.String(), "\n\n") {
		line, err := br.ReadString('\n')
		require.NoError(t, err)
		seen.WriteString(line)
	}
	assert.Contains(t, seen.String(), "event: hello\n")
	assert.Contains(t, seen.String(), `"instance_id"`)

	r, took := d.terminate()

	assert.Equal(t, 0, r.code, r.stderr)
	assert.Less(t, took, quickShutdown, "an event stream does not hold the shutdown for the grace period")
}
