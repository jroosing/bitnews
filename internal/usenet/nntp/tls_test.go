package nntp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func neverGreets(s *session) { s.line("200 should not be reached") }

func TestTLSRejectsSelfSignedServerWithoutInjectedCA(t *testing.T) {
	srv := newFakeServer(t, neverGreets, withCert(selfSigned(t, testHost)))
	cfg := srv.config()
	cfg.rootCAs = nil // system roots only, as in production

	c, err := Dial(context.Background(), cfg)
	require.Nil(t, c)
	require.ErrorIs(t, err, ErrConnection)
	var cve *tls.CertificateVerificationError
	require.ErrorAs(t, err, &cve)
	var ua x509.UnknownAuthorityError
	require.ErrorAs(t, err, &ua)
}

func TestTLSAcceptsSelfSignedServerOnlyThroughInjectedCA(t *testing.T) {
	cert := selfSigned(t, testHost)
	srv := newFakeServer(t, func(s *session) { s.line("200 ok"); s.readCmd() }, withCert(cert))
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	cfg := srv.config()
	cfg.rootCAs = pool

	c, err := Dial(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

func TestTLSRejectsHostnameMismatchEvenWithTrustedCA(t *testing.T) {
	srv := newFakeServer(t, neverGreets, withNames("other.test"))

	c, err := Dial(context.Background(), srv.config())
	require.Nil(t, c)
	require.ErrorIs(t, err, ErrConnection)
	var he x509.HostnameError
	require.ErrorAs(t, err, &he)
}

func TestTLSRejectsServerSpeakingOnlyTLS11(t *testing.T) {
	srv := newFakeServer(t, neverGreets, withTLSConfig(func(c *tls.Config) {
		c.MinVersion = tls.VersionTLS10
		c.MaxVersion = tls.VersionTLS11
	}))

	c, err := Dial(context.Background(), srv.config())
	require.Nil(t, c)
	require.ErrorIs(t, err, ErrConnection)
	var e *Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, "tls", e.Op)
}

func TestTLSRejectsPlaintextServer(t *testing.T) {
	// A server that answers a TLS ClientHello with an NNTP greeting must
	// not be talked to.
	srv := newFakeServer(t, neverGreets, withPlaintext())
	c, err := Dial(context.Background(), srv.config())
	require.Nil(t, c)
	require.ErrorIs(t, err, ErrConnection)
}

func TestConfigHasNoWayToWeakenTLS(t *testing.T) {
	var exported []string
	rt := reflect.TypeFor[Config]()
	for f := range rt.Fields() {
		if f.IsExported() {
			exported = append(exported, f.Name)
		}
	}
	require.ElementsMatch(t,
		[]string{"Host", "Port", "User", "Pass", "Dialer", "ConnectTimeout", "ReadTimeout"}, exported)
}
