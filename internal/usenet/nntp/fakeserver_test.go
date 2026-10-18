package nntp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// testHost is the name every test certificate is valid for and every test
// Config dials. The Dialer ignores it and connects to the fake server.
const testHost = "news.test"

// testPKI is a throwaway certificate authority created at test time.
type testPKI struct {
	t    *testing.T
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nntp test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testPKI{t: t, cert: cert, key: key, pool: pool}
}

// issue returns a server certificate for names signed by the CA.
func (p *testPKI) issue(names ...string) tls.Certificate {
	p.t.Helper()
	return makeCert(p.t, p.cert, p.key, names)
}

// selfSigned returns a certificate that no CA vouches for.
func selfSigned(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	return makeCert(t, nil, nil, names)
}

func makeCert(t *testing.T, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, names []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeServer is a scriptable in-process NNTP server speaking TLS on a
// loopback port. Each accepted connection runs the handler.
type fakeServer struct {
	t       *testing.T
	pki     *testPKI
	ln      net.Listener
	handler func(*session)

	mu       sync.Mutex
	commands []string
	wg       sync.WaitGroup
	sessions sync.WaitGroup
	conns    []net.Conn
}

type serverOption func(*serverConfig)

type serverConfig struct {
	cert      *tls.Certificate
	names     []string
	plaintext bool
	tlsConf   func(*tls.Config)
}

// withCert serves cert instead of one issued by the test CA for testHost.
func withCert(c tls.Certificate) serverOption {
	return func(s *serverConfig) { s.cert = &c }
}

// withNames serves a certificate from the test CA valid for names only.
func withNames(names ...string) serverOption {
	return func(s *serverConfig) { s.names = names }
}

// withPlaintext makes the server skip TLS and speak NNTP in the clear.
func withPlaintext() serverOption {
	return func(s *serverConfig) { s.plaintext = true }
}

// withTLSConfig lets a test tweak the server's tls.Config (versions, ...).
func withTLSConfig(f func(*tls.Config)) serverOption {
	return func(s *serverConfig) { s.tlsConf = f }
}

func newFakeServer(t *testing.T, handler func(*session), opts ...serverOption) *fakeServer {
	t.Helper()
	var sc serverConfig
	for _, o := range opts {
		o(&sc)
	}
	pki := newTestPKI(t)
	names := []string{testHost}
	if sc.names != nil {
		names = sc.names
	}
	cert := pki.issue(names...)
	if sc.cert != nil {
		cert = *sc.cert
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}}
	if sc.tlsConf != nil {
		sc.tlsConf(tc)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, pki: pki, ln: ln, handler: handler}
	s.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			s.wg.Add(1)
			s.sessions.Add(1)
			go func() {
				defer s.wg.Done()
				defer s.sessions.Done()
				defer c.Close()
				var conn net.Conn = c
				if !sc.plaintext {
					tconn := tls.Server(c, tc)
					_ = tconn.SetDeadline(time.Now().Add(30 * time.Second))
					if err := tconn.Handshake(); err != nil {
						return
					}
					_ = tconn.SetDeadline(time.Time{})
					conn = tconn
				}
				s.handler(&session{srv: s, conn: conn, br: bufio.NewReader(conn)})
			}()
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

// config returns a Config that reaches this server and trusts its CA.
func (s *fakeServer) config() Config {
	addr := s.ln.Addr().String()
	return Config{
		Host: testHost,
		Port: s.ln.Addr().(*net.TCPAddr).Port,
		Dialer: dialerFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}),
		ConnectTimeout: 5 * time.Second,
		ReadTimeout:    5 * time.Second,
		rootCAs:        s.pki.pool,
	}
}

// receivedNoWait returns the commands read so far without waiting for the
// sessions to end.
func (s *fakeServer) receivedNoWait() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// received waits for every session handler to return, then returns every
// command line the server read.
func (s *fakeServer) received() []string {
	s.t.Helper()
	done := make(chan struct{})
	go func() { s.sessions.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.t.Fatal("fake server handler did not finish")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// session is one accepted client connection.
type session struct {
	srv  *fakeServer
	conn net.Conn
	br   *bufio.Reader
}

// send writes raw bytes; use "\r\n" yourself for full control.
func (s *session) send(raw string) {
	_, _ = s.conn.Write([]byte(raw))
}

// line sends a single CRLF-terminated response line.
func (s *session) line(l string) { s.send(l + "\r\n") }

// readCmd reads one command line (without CRLF) and records it. It returns
// "" and false on EOF.
func (s *session) readCmd() (string, bool) {
	l, err := s.br.ReadString('\n')
	if err != nil {
		return "", false
	}
	l = strings.TrimRight(l, "\r\n")
	s.srv.mu.Lock()
	s.srv.commands = append(s.srv.commands, l)
	s.srv.mu.Unlock()
	return l, true
}

// expect reads one command and fails the test if it differs from want.
func (s *session) expect(want string) {
	s.srv.t.Helper()
	got, ok := s.readCmd()
	if !ok || got != want {
		s.srv.t.Errorf("server expected command %q, got %q (ok=%v)", want, got, ok)
	}
}

// stall blocks until the client hangs up or the test ends.
func (s *session) stall() { _, _ = io.Copy(io.Discard, s.br) }

// close drops the connection abruptly.
func (s *session) close() { _ = s.conn.Close() }
