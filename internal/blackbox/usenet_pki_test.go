package blackbox_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Helpers for the TestUsenet* tests: a throwaway certificate authority and
// the one decision about whether the daemon under test can trust it.
//
// The daemon always uses TLS and verifies certificates (no plaintext mode, no
// skip-verify), so a fake NNTP server needs a certificate the daemon trusts.
// The tests hand the daemon the CA through the process environment
// (SSL_CERT_FILE): Go honours it on Linux, OpenSSL always does. Go on macOS
// ignores it and asks the system keychain, so there the tests that need the
// daemon to trust the CA are skipped, see requireCATrust.

// caEnv is the variable that overrides the trust decision: "1" runs the
// tests that need a trusted CA whatever the binary is, "0" skips them.
const caEnv = "BITNEWS_E2E_CA_TRUST"

// canTrustTestCA decides whether the daemon under test can be made to trust
// the test CA through SSL_CERT_FILE, and says why not. It is the only place
// that makes this decision.
func canTrustTestCA() (ok bool, reason string) {
	switch os.Getenv(caEnv) {
	case "1":
		return true, ""
	case "0":
		return false, caEnv + "=0"
	}
	if binary.isGo && runtime.GOOS != "linux" {
		return false, "the Go daemon on " + runtime.GOOS + " ignores SSL_CERT_FILE and uses the system trust store; run on Linux (Docker) or with the Zig binary"
	}
	return true, ""
}

// requireCATrust skips the test when the daemon cannot trust the test CA.
// Tests that expect the TLS handshake to fail (untrusted certificate, no TLS)
// do not call it.
func requireCATrust(t *testing.T) {
	t.Helper()
	if ok, why := canTrustTestCA(); !ok {
		t.Skipf("the daemon under test cannot be made to trust the test CA: %s", why)
	}
}

// testCA is a certificate authority created for one test.
type testCA struct {
	t    *testing.T
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// File is the PEM file to point SSL_CERT_FILE at.
	File string
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bitnews blackbox test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	return &testCA{t: t, cert: cert, key: key, File: file}
}

// certOptions tune issue.
type certOptions struct {
	notBefore, notAfter time.Time
	selfSigned          bool
}

type certOption func(*certOptions)

// expired makes the certificate's validity end an hour ago.
func expired() certOption {
	return func(o *certOptions) {
		o.notBefore, o.notAfter = time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour)
	}
}

// selfSignedCert makes a certificate that no CA vouches for.
func selfSignedCert() certOption { return func(o *certOptions) { o.selfSigned = true } }

// issue returns a server certificate valid for the DNS names and IP
// addresses in names, signed by the CA.
func (c *testCA) issue(names []string, opts ...certOption) tls.Certificate {
	c.t.Helper()
	o := certOptions{notBefore: time.Now().Add(-time.Hour), notAfter: time.Now().Add(12 * time.Hour)}
	for _, f := range opts {
		f(&o)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(c.t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(c.t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    o.notBefore,
		NotAfter:     o.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	parent, parentKey := c.cert, c.key
	if o.selfSigned {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	require.NoError(c.t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
