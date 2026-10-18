package nntp

import (
	"context"
	"crypto/x509"
	"net"
	"time"
)

// Defaults applied by Dial to zero Config fields.
const (
	DefaultPort           = 563
	DefaultConnectTimeout = 30 * time.Second
	DefaultReadTimeout    = 60 * time.Second
)

// Dialer opens the raw TCP connection. *net.Dialer satisfies it, and
// interface binding can supply its own.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

type dialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f dialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

// Config describes one Server endpoint. TLS is always on, certificates are
// always verified and there is deliberately no way to change either.
type Config struct {
	// Host is the DNS name or IP of the Server; it is also the name the
	// certificate must match.
	Host string
	// Port defaults to DefaultPort.
	Port int
	// User and Pass are the plain credentials. Leave User empty for a
	// Server that needs no authentication. They are never logged or
	// included in errors.
	User, Pass string
	// Dialer opens the TCP connection. Defaults to a net.Dialer.
	Dialer Dialer
	// ConnectTimeout bounds the TCP dial plus TLS handshake. Defaults to
	// DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// ReadTimeout is the idle timeout for each read or write on the
	// connection. Defaults to DefaultReadTimeout.
	ReadTimeout time.Duration

	// rootCAs replaces the system roots. It is unexported so that only
	// tests inside this package can set it.
	rootCAs *x509.CertPool
}
