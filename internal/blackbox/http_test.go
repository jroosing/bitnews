package blackbox_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/stretchr/testify/require"
)

// response is an HTTP answer read completely.
type response struct {
	status int
	header http.Header
	body   string
}

// request sends one request and reads the whole answer. A nil header and an
// empty body are fine.
func (d *daemon) request(method, path string, header map[string]string, body string) response {
	d.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+d.addr+path, rd)
	require.NoError(d.t, err)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := d.client.Do(req)
	require.NoError(d.t, err, "%s %s", method, path)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(d.t, err)
	return response{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

func (d *daemon) get(path string, header map[string]string) response {
	d.t.Helper()
	return d.request(http.MethodGet, path, header, "")
}

// withKey returns the headers of a request with the app key as a bearer token.
func bearer(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

// raw sends the bytes as they are on a new connection, adds nothing and
// returns everything the daemon writes until it closes the connection or the
// deadline passes. The request should carry "Connection: close".
func (d *daemon) raw(request string) string {
	d.t.Helper()
	conn, err := net.DialTimeout("tcp", d.addr, 5*time.Second)
	require.NoError(d.t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(d.t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	_, err = io.WriteString(conn, request)
	require.NoError(d.t, err)
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, bufio.NewReader(conn))
	return buf.String()
}

// Request targets and bodies that several tests use.
const (
	errUnauthorized = `{"error":{"code":"unauthorized","message":"a valid API key is required"}}` + "\n"
	errNotFound     = `{"error":{"code":"not_found","message":"no such route"}}` + "\n"
	errMethod       = `{"error":{"code":"method_not_allowed","message":"this method is not allowed for this path"}}` + "\n"
	errSetup        = `{"error":{"code":"setup_required","message":"first-run setup is required: create the admin account with the setup token from the server log"}}` + "\n"
	errTooLarge     = `{"error":{"code":"request_too_large","message":"request body is too large"}}` + "\n"
	healthBody      = `{"status":"ok"}` + "\n"
)

// splitLines splits s into its non-empty lines.
func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
