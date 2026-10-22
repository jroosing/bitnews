package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/privdrop"
)

// listenOn starts an HTTP server answering /api/v1/health with status and
// returns the port it listens on.
func listenOn(t *testing.T, status int, delay time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(delay)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	return port
}

func runHealthcheck(t *testing.T, listen string) (int, string) {
	t.Helper()
	env := map[string]string{}
	if listen != "" {
		env["BITNEWS_LISTEN"] = listen
	}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"healthcheck"}, getenv, privdrop.OS(), &out, &errOut, nil)
	return code, out.String() + errOut.String()
}

func TestHealthcheckExitsZeroWhenTheHealthRouteAnswers200(t *testing.T) {
	port := listenOn(t, http.StatusOK, 0)

	code, _ := runHealthcheck(t, "0.0.0.0:"+port)

	assert.Equal(t, 0, code)
}

func TestHealthcheckUsesLoopbackEvenWhenListeningOnAllInterfaces(t *testing.T) {
	port := listenOn(t, http.StatusOK, 0)

	code, _ := runHealthcheck(t, "[::]:"+port)

	assert.Equal(t, 0, code)
}

func TestHealthcheckExitsOneOnANon200Status(t *testing.T) {
	port := listenOn(t, http.StatusServiceUnavailable, 0)

	code, out := runHealthcheck(t, "0.0.0.0:"+port)

	assert.Equal(t, 1, code)
	assert.Contains(t, out, "503")
}

func TestHealthcheckExitsOneWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	require.NoError(t, ln.Close())

	code, _ := runHealthcheck(t, "0.0.0.0:"+port)

	assert.Equal(t, 1, code)
}

func TestHealthcheckGivesUpAfterThreeSeconds(t *testing.T) {
	port := listenOn(t, http.StatusOK, 4*time.Second)
	start := time.Now()

	code, _ := runHealthcheck(t, "0.0.0.0:"+port)

	assert.Equal(t, 1, code)
	assert.Less(t, time.Since(start), 3900*time.Millisecond)
}

func TestHealthcheckRejectsAnInvalidListenAddress(t *testing.T) {
	code, out := runHealthcheck(t, "no-port")

	assert.Equal(t, 2, code)
	assert.Contains(t, out, "BITNEWS_LISTEN")
}
