package blackbox_test

import (
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The connection test of a Server: POST /api/v1/servers/test (an unsaved
// spec) and POST /api/v1/servers/{id}/test (a saved Server). The answer is
// 200 whatever happened; the body says what, with messages that never carry
// credentials. internal/app/prober.go is the specification.

// probeStart starts a daemon that trusts the test CA, with the default
// Server of the config dir (which nobody listens for).
func probeStart(t *testing.T, ca *testCA) *daemon {
	t.Helper()
	cfg := prepareConfigDir(t)
	return startDaemon(t, cfg, withEnv("SSL_CERT_FILE", ca.File))
}

// probeBody is the JSON spec of a Server at host:port with the fake's
// credentials.
func probeBody(host string, port int) string {
	return fmt.Sprintf(`{"name":"probe","host":%q,"port":%d,"username":%q,"password":%q}`, host, port, serverUsername, serverPassword)
}

// probeResult is the answer of a test route.
type probeResult struct {
	OK      bool
	Class   string
	Detail  string
	Message string
}

// probe tests an unsaved spec and checks the shape of the answer.
func probe(t *testing.T, d *daemon, body string) probeResult {
	t.Helper()
	r := d.api(http.MethodPost, "/api/v1/servers/test", body)
	requireOK(t, r)
	return decodeProbe(t, r)
}

func decodeProbe(t *testing.T, r response) probeResult {
	t.Helper()
	m := obj(t, r)
	require.Len(t, m, 6, r.body)
	for _, k := range []string{"ok", "class", "detail", "message", "connect_ms", "latency_ms"} {
		require.Contains(t, m, k, r.body)
	}
	assert.GreaterOrEqual(t, num(m, "connect_ms"), int64(0))
	assert.GreaterOrEqual(t, num(m, "latency_ms"), int64(0))
	if m["ok"] != true {
		assert.Zero(t, num(m, "connect_ms"), "no times unless the connection worked")
		assert.Zero(t, num(m, "latency_ms"))
	}
	assert.NotContains(t, r.body, serverPassword, "the password is never in an answer")
	return probeResult{OK: m["ok"] == true, Class: m["class"].(string), Detail: m["detail"].(string), Message: m["message"].(string)}
}

func TestUsenetServerTestOfAWorkingServerIsOK(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword)
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{OK: true, Class: "ok", Detail: "", Message: "connected and authenticated"}, res)
	cmds := srv.Commands()
	require.GreaterOrEqual(t, len(cmds), 3, "%v", cmds)
	assert.Equal(t, "AUTHINFO USER "+serverUsername, cmds[0])
	assert.Equal(t, "AUTHINFO PASS "+serverPassword, cmds[1])
	assert.Equal(t, "DATE", cmds[2], "the round trip that gives the latency")
	assert.Equal(t, 1, srv.Dials())
}

func TestUsenetServerTestOfASavedServerUsesItsStoredPassword(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword)
	d := probeStart(t, ca)
	created := d.api(http.MethodPost, "/api/v1/servers", probeBody(srv.Host(), srv.Port()))
	require.Equal(t, http.StatusCreated, created.status, created.body)
	id := obj(t, created)["id"].(string)

	r := d.api(http.MethodPost, "/api/v1/servers/"+id+"/test", "")
	requireOK(t, r)

	assert.Equal(t, probeResult{OK: true, Class: "ok", Message: "connected and authenticated"}, decodeProbe(t, r))
	assert.Contains(t, srv.Commands(), "AUTHINFO PASS "+serverPassword)

	// A wrong password in the stored Server is reported the same way.
	require.Equal(t, http.StatusOK, d.api(http.MethodPatch, "/api/v1/servers/"+id, `{"password":"changed"}`).status)
	r = d.api(http.MethodPost, "/api/v1/servers/"+id+"/test", "")
	requireOK(t, r)
	assert.Equal(t, probeResult{Class: "auth", Detail: "rejected", Message: "the server rejected the user name or password (481)"}, decodeProbe(t, r))
}

func TestUsenetServerTestWithAWrongPasswordIsAuthRejected(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, "another-password")
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "auth", Detail: "rejected", Message: "the server rejected the user name or password (481)"}, res)
	assert.Equal(t, 1, srv.Auths())
}

func TestUsenetServerTestWorksWithAHostNameCertificate(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword)
	d := probeStart(t, ca)

	res := probe(t, d, probeBody("localhost", srv.Port()))

	assert.Equal(t, probeResult{OK: true, Class: "ok", Message: "connected and authenticated"}, res)
}

func TestUsenetServerTestOfACertificateNoCAVouchesForIsUntrusted(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	srv := startNNTP(t, ca, withCert(ca.issue([]string{"127.0.0.1"}, selfSignedCert()))).Credentials(serverUsername, serverPassword)
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "tls", Detail: "certificate_untrusted", Message: "the server's certificate is not trusted"}, res)
	assert.Zero(t, srv.Auths(), "no credentials are sent over a connection that is not trusted")
	assert.Empty(t, srv.Commands())
}

func TestUsenetServerTestOfACertificateForAnotherHostIsAHostnameMismatch(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca, withCert(ca.issue([]string{"news.other.example"}))).Credentials(serverUsername, serverPassword)
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "tls", Detail: "certificate_hostname", Message: "the server's certificate is not valid for this host name"}, res)
	assert.Empty(t, srv.Commands(), "no credentials are sent")
}

func TestUsenetServerTestOfAnExpiredCertificate(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca, withCert(ca.issue([]string{"127.0.0.1"}, expired()))).Credentials(serverUsername, serverPassword)
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "tls", Detail: "certificate_expired", Message: "the server's certificate has expired or is not valid yet"}, res)
	assert.Empty(t, srv.Commands())
}

func TestUsenetServerTestOfAPortThatSpeaksNoTLS(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	srv := startNNTP(t, ca, withPlaintext())
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "tls", Detail: "not_tls", Message: "the server does not speak TLS on this port: bitnews only connects over TLS"}, res)
	assert.NotContains(t, srv.Commands(), "AUTHINFO PASS "+serverPassword, "the password is not sent in the clear")
}

func TestUsenetServerTestOfAServerThatHangsUpDuringTheHandshake(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
	d := probeStart(t, ca)

	res := probe(t, d, probeBody("127.0.0.1", ln.Addr().(*net.TCPAddr).Port))

	assert.Equal(t, probeResult{Class: "tls", Detail: "handshake", Message: "the TLS handshake failed"}, res)
}

func TestUsenetServerTestOfAPortNobodyListensOnIsRefused(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	d := probeStart(t, ca)

	res := probe(t, d, probeBody("127.0.0.1", freePort(t)))

	assert.Equal(t, probeResult{Class: "tcp", Detail: "refused", Message: "the connection was refused: check the host and port"}, res)
}

func TestUsenetServerTestOfAnUnknownHostIsADNSProblem(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	d := probeStart(t, ca)

	res := probe(t, d, probeBody("no-such-host.invalid", 563))

	assert.False(t, res.OK)
	assert.Equal(t, "dns", res.Class, "%+v", res)
	// The detail depends on what the resolver answers: not_found for an
	// NXDOMAIN, timeout or other when there is no network.
	want := map[string]string{
		"not_found": "the host name does not exist",
		"timeout":   "the host name lookup timed out",
		"other":     "the host name could not be resolved",
	}
	assert.Equal(t, want[res.Detail], res.Message, "%+v", res)
}

func TestUsenetServerTestOfAServerThatNeverGreetsTimesOut(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Silent()
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "protocol", Detail: "greeting", Message: "the server did not answer in time"}, res)
}

func TestUsenetServerTestOfAServerThatClosesAfterTheHandshake(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).CloseAtOnce()
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "protocol", Detail: "greeting", Message: "unexpected response from the server"}, res)
}

func TestUsenetServerTestQuotesARefusalAndScrubsTheCredentialsFromIt(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Greeting("502 go away " + serverUsername + " " + serverPassword)
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "protocol", Detail: "greeting", Message: "unexpected response from the server (502 go away *** ***)"}, res)
}

func TestUsenetServerTestOfAServerThatFailsTheAuthenticationStrangely(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword).AuthReply("400 service temporarily unavailable")
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "protocol", Detail: "auth_response", Message: "unexpected response from the server (400 service temporarily unavailable)"}, res)
}

func TestUsenetServerTestOfAServerThatCannotTellTheTime(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword).DateReply("500 what is a date")
	d := probeStart(t, ca)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "protocol", Detail: "date", Message: "unexpected response from the server (500 what is a date)"}, res)
}

func TestUsenetServerTestAnswersATooManyConnectionsGreeting(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword).MaxConnections(1)
	d := probeStart(t, ca)
	_ = dialTLSAndHold(t, ca, srv)

	res := probe(t, d, probeBody(srv.Host(), srv.Port()))

	assert.Equal(t, probeResult{Class: "protocol", Detail: "greeting", Message: "unexpected response from the server (502 Too many connections)"}, res)
}
