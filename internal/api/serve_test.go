package api_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/api"
)

func TestHTTPServerIsHardened(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	srv := e.server.HTTPServer("127.0.0.1:9494")

	assert.Equal(t, "127.0.0.1:9494", srv.Addr)
	assert.Equal(t, 10*time.Second, srv.ReadHeaderTimeout)
	assert.Equal(t, 60*time.Second, srv.ReadTimeout)
	assert.Equal(t, 120*time.Second, srv.IdleTimeout)
	assert.Equal(t, 64<<10, srv.MaxHeaderBytes)
	assert.Positive(t, srv.WriteTimeout, "only the event stream lifts the write timeout, per request")
	assert.NotNil(t, srv.Handler)
}

// start runs the env's server on a free loopback port until the returned
// stop function is called, and returns its base URL.
// oneShot never reuses connections. With keep-alive the default client may
// leave an unused speculative connection behind, and http.Server.Shutdown only
// treats a connection that never sent a request as idle after 5 seconds, which
// is exactly the tests' grace period.
var oneShot = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func start(t *testing.T, e *env) (base string, stop func() error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- api.Serve(ctx, e.server.HTTPServer(ln.Addr().String()), ln, 5*time.Second) }()
	var once bool
	stop = func() error {
		if !once {
			once = true
			cancel()
		}
		return <-done
	}
	t.Cleanup(func() { cancel() })
	return "http://" + ln.Addr().String(), stop
}

// listenWith runs the env's server like start, after letting tweak change
// the http.Server, and returns the listener and server.
func listenWith(t *testing.T, e *env, tweak func(*http.Server)) (net.Listener, *http.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := e.server.HTTPServer(ln.Addr().String())
	tweak(srv)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- api.Serve(ctx, srv, ln, time.Second) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ln, srv
}

func TestRealServerServesTheAPIAndCapsRequestBodies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	base, stop := start(t, e)

	health, err := oneShot.Get(base + "/api/v1/health")
	require.NoError(t, err)
	_ = health.Body.Close()
	req, err := http.NewRequest("POST", base+"/api/v1/categories", strings.NewReader(`{"name":"`+strings.Repeat("a", 32<<10)+`"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	big, err := oneShot.Do(req)
	require.NoError(t, err)
	_ = big.Body.Close()

	assert.Equal(t, http.StatusOK, health.StatusCode)
	assert.Equal(t, http.StatusRequestEntityTooLarge, big.StatusCode)
	assert.NoError(t, stop())
}

func TestGracefulShutdownLetsARequestInFlightFinish(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	e.server.HandleForTest("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	base, stop := start(t, e)
	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := oneShot.Get(base + "/slow")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- result{body: string(b), err: err}
	}()
	<-started

	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	select {
	case err := <-stopped:
		t.Fatalf("Serve returned %v while a request was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	res := <-got
	require.NoError(t, res.err)
	assert.Equal(t, "finished", res.body)
	assert.NoError(t, <-stopped)
}
