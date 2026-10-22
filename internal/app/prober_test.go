package app_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
)

type fakeProbeConn struct {
	date   func() (time.Time, error)
	closed bool
}

func (c *fakeProbeConn) Date(context.Context) (time.Time, error) { return c.date() }
func (c *fakeProbeConn) Close() error                            { c.closed = true; return nil }

func probeServer() core.Server {
	return core.Server{Host: "news.example.com", Port: 563, Username: "joe", Password: "hunter2-secret"}
}

func TestProbeOfAWorkingServerReportsOKWithTimings(t *testing.T) {
	var now time.Time
	step := 0
	clock := func() time.Time { step++; now = now.Add(time.Duration(step) * 10 * time.Millisecond); return now }
	conn := &fakeProbeConn{date: func() (time.Time, error) { return time.Now(), nil }}
	var gotCfg nntp.Config
	p := app.NewTestProber(func(_ context.Context, cfg nntp.Config) (app.ProbeConn, error) { gotCfg = cfg; return conn, nil }, clock)

	res := p.Probe(context.Background(), probeServer())

	assert.True(t, res.OK)
	assert.Equal(t, app.ProbeOK, res.Class)
	assert.Positive(t, res.ConnectTime)
	assert.Positive(t, res.Latency)
	assert.True(t, conn.closed, "the connection is closed")
	assert.Equal(t, "news.example.com", gotCfg.Host)
	assert.Equal(t, 563, gotCfg.Port)
	assert.Equal(t, "joe", gotCfg.User)
	assert.Equal(t, "hunter2-secret", gotCfg.Pass)
	assert.Less(t, gotCfg.ConnectTimeout, time.Minute, "a test uses a short timeout")
	assert.NotZero(t, gotCfg.ConnectTimeout)
}

func TestProbeClassifiesEveryFailureStep(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "news.example.com", IsNotFound: true}
	dnsTimeout := &net.DNSError{Err: "i/o timeout", Name: "news.example.com", IsTimeout: true}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	cert := func(inner error) error {
		return &tls.CertificateVerificationError{Err: inner}
	}
	for name, tc := range map[string]struct {
		err    error
		class  app.ProbeClass
		detail string
	}{
		"dns not found":  {&nntp.Error{Kind: nntp.ErrConnection, Op: "dial", Err: &net.OpError{Op: "dial", Err: dnsErr}}, app.ProbeDNS, "not_found"},
		"dns timeout":    {&nntp.Error{Kind: nntp.ErrConnection, Op: "dial", Err: dnsTimeout}, app.ProbeDNS, "timeout"},
		"tcp refused":    {&nntp.Error{Kind: nntp.ErrConnection, Op: "dial", Err: refused}, app.ProbeTCP, "other"},
		"tcp timeout":    {&nntp.Error{Kind: nntp.ErrConnection, Op: "dial", Err: context.DeadlineExceeded}, app.ProbeTCP, "timeout"},
		"tls untrusted":  {&nntp.Error{Kind: nntp.ErrConnection, Op: "tls", Err: cert(x509.UnknownAuthorityError{})}, app.ProbeTLS, "certificate_untrusted"},
		"tls hostname":   {&nntp.Error{Kind: nntp.ErrConnection, Op: "tls", Err: cert(x509.HostnameError{Host: "x"})}, app.ProbeTLS, "certificate_hostname"},
		"tls expired":    {&nntp.Error{Kind: nntp.ErrConnection, Op: "tls", Err: cert(x509.CertificateInvalidError{Reason: x509.Expired})}, app.ProbeTLS, "certificate_expired"},
		"tls other cert": {&nntp.Error{Kind: nntp.ErrConnection, Op: "tls", Err: cert(x509.CertificateInvalidError{Reason: x509.TooManyIntermediates})}, app.ProbeTLS, "certificate_invalid"},
		"tls plaintext":  {&nntp.Error{Kind: nntp.ErrConnection, Op: "tls", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}, app.ProbeTLS, "not_tls"},
		"tls handshake":  {&nntp.Error{Kind: nntp.ErrConnection, Op: "tls", Err: errors.New("remote error: tls: protocol version not supported")}, app.ProbeTLS, "handshake"},
		"tls timeout":    {&nntp.Error{Kind: nntp.ErrConnection, Op: "tls", Err: context.DeadlineExceeded}, app.ProbeTLS, "timeout"},
		"wrong password": {&nntp.Error{Kind: nntp.ErrAuth, Op: "auth", Code: 481, Text: "Authentication failed"}, app.ProbeAuth, "rejected"},
		"greeting 502":   {&nntp.Error{Kind: nntp.ErrConnection, Op: "greeting", Code: 502, Text: "too many connections"}, app.ProbeProtocol, "greeting"},
		"auth 502":       {&nntp.Error{Kind: nntp.ErrConnection, Op: "auth", Code: 502, Text: "limit"}, app.ProbeProtocol, "auth_response"},
		"garbage":        {&nntp.Error{Kind: nntp.ErrProtocol, Op: "greeting", Text: "line too long"}, app.ProbeProtocol, "greeting"},
		"date":           {&nntp.Error{Kind: nntp.ErrProtocol, Op: "date", Code: 500, Text: "what?"}, app.ProbeProtocol, "date"},
		"other":          {errors.New("boom"), app.ProbeProtocol, "other"},
	} {
		t.Run(name, func(t *testing.T) {
			p := app.NewTestProber(func(context.Context, nntp.Config) (app.ProbeConn, error) { return nil, tc.err }, time.Now)

			res := p.Probe(context.Background(), probeServer())

			assert.False(t, res.OK)
			assert.Equal(t, tc.class, res.Class)
			assert.Equal(t, tc.detail, res.Detail)
			assert.NotEmpty(t, res.Message)
		})
	}
}

func TestProbeDateFailureIsAProtocolErrorAndStillCloses(t *testing.T) {
	conn := &fakeProbeConn{date: func() (time.Time, error) {
		return time.Time{}, &nntp.Error{Kind: nntp.ErrProtocol, Op: "date", Code: 500}
	}}
	p := app.NewTestProber(func(context.Context, nntp.Config) (app.ProbeConn, error) { return conn, nil }, time.Now)

	res := p.Probe(context.Background(), probeServer())

	assert.Equal(t, app.ProbeProtocol, res.Class)
	assert.Equal(t, "date", res.Detail)
	assert.True(t, conn.closed)
}

func TestProbeResultNeverContainsTheCredentials(t *testing.T) {
	// A server that echoes the credentials in its error text.
	leaky := &nntp.Error{Kind: nntp.ErrAuth, Op: "auth", Code: 481, Text: "bad login joe hunter2-secret"}
	p := app.NewTestProber(func(context.Context, nntp.Config) (app.ProbeConn, error) { return nil, leaky }, time.Now)

	res := p.Probe(context.Background(), probeServer())

	assert.NotContains(t, res.Message, "hunter2-secret")
	assert.NotContains(t, res.Message, "joe")
	assert.NotContains(t, res.Detail, "hunter2-secret")
}

func TestRealProbeToAnUnresolvableHostIsDNS(t *testing.T) {
	p := app.NewNNTPProber()
	s := probeServer()
	s.Host = "does-not-exist.invalid"

	res := p.Probe(context.Background(), s)

	assert.False(t, res.OK)
	assert.Equal(t, app.ProbeDNS, res.Class, "%+v", res)
}

func TestRealProbeToAClosedPortIsTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	s := probeServer()
	s.Host, s.Port = "127.0.0.1", port

	res := app.NewNNTPProber().Probe(context.Background(), s)

	assert.Equal(t, app.ProbeTCP, res.Class, "%+v", res)
	assert.Equal(t, "refused", res.Detail)
}

func TestRealProbeToAServerWithAnUntrustedCertificateIsTLSCertificate(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.StartTLS()
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	s := probeServer()
	s.Host = host
	s.Port = atoi(t, portStr)

	res := app.NewNNTPProber().Probe(context.Background(), s)

	assert.Equal(t, app.ProbeTLS, res.Class, "%+v", res)
	assert.True(t, strings.HasPrefix(res.Detail, "certificate_"), res.Detail)
	assert.False(t, res.OK)
}

func TestRealProbeToAPlainTextServerIsTLSNotTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("200 hello, I speak plain text\r\n"))
			time.Sleep(50 * time.Millisecond)
			_ = c.Close()
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	s := probeServer()
	s.Host, s.Port = host, atoi(t, portStr)

	res := app.NewNNTPProber().Probe(context.Background(), s)

	assert.Equal(t, app.ProbeTLS, res.Class, "%+v", res)
	assert.Equal(t, "not_tls", res.Detail)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
