package nntp

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDialUsesPluggableDialerWithHostAndPort(t *testing.T) {
	srv := newFakeServer(t, func(s *session) { greetOK(s); s.readCmd() })
	cfg := srv.config()
	inner := cfg.Dialer
	var gotNetwork, gotAddr string
	cfg.Dialer = dialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		gotNetwork, gotAddr = network, addr
		return inner.DialContext(ctx, network, addr)
	})
	c := dialCfg(t, cfg)
	require.NoError(t, c.Close())
	require.Equal(t, "tcp", gotNetwork)
	require.Equal(t, net.JoinHostPort(testHost, strconv.Itoa(cfg.Port)), gotAddr)
}

func TestDialDefaultsToPort563(t *testing.T) {
	var gotAddr string
	_, err := Dial(context.Background(), Config{
		Host: testHost,
		Dialer: dialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			gotAddr = addr
			return nil, errors.New("stop here")
		}),
	})
	require.ErrorIs(t, err, ErrConnection)
	require.Equal(t, net.JoinHostPort(testHost, "563"), gotAddr)
}

func TestDialDialerFailureKeepsUnderlyingError(t *testing.T) {
	cause := &net.DNSError{Err: "no such host", Name: testHost, IsNotFound: true}
	_, err := Dial(context.Background(), Config{
		Host: testHost,
		Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
			return nil, cause
		}),
	})
	require.ErrorIs(t, err, ErrConnection)
	var dns *net.DNSError
	require.ErrorAs(t, err, &dns)
	var e *Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, "dial", e.Op)
}

func TestDialConnectTimeoutBoundsDialer(t *testing.T) {
	start := time.Now()
	_, err := Dial(context.Background(), Config{
		Host:           testHost,
		ConnectTimeout: 100 * time.Millisecond,
		Dialer: dialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	})
	require.ErrorIs(t, err, ErrConnection)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestDialConnectTimeoutBoundsTLSHandshake(t *testing.T) {
	// A server that accepts TCP but never answers the TLS handshake.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err == nil {
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	start := time.Now()
	_, err = Dial(context.Background(), Config{
		Host:           testHost,
		ConnectTimeout: 150 * time.Millisecond,
		Dialer: dialerFunc(func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, ln.Addr().String())
		}),
	})
	require.ErrorIs(t, err, ErrConnection)
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestDialContextCancelAbortsWaitForGreeting(t *testing.T) {
	srv := newFakeServer(t, func(s *session) { s.stall() })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := Dial(ctx, srv.config())
	require.ErrorIs(t, err, ErrConnection)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestDialReadTimeoutAppliesToGreeting(t *testing.T) {
	srv := newFakeServer(t, func(s *session) { s.stall() })
	cfg := srv.config()
	cfg.ReadTimeout = 100 * time.Millisecond
	start := time.Now()
	_, err := Dial(context.Background(), cfg)
	require.ErrorIs(t, err, ErrConnection)
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestDialStalledAuthIsBoundedByReadTimeout(t *testing.T) {
	srv := newFakeServer(t, func(s *session) { greetOK(s); s.readCmd(); s.stall() })
	cfg := srv.config()
	cfg.User, cfg.Pass = "u", "p"
	cfg.ReadTimeout = 100 * time.Millisecond
	_, err := Dial(context.Background(), cfg)
	require.ErrorIs(t, err, ErrConnection)
	var e *Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, "auth", e.Op)
}
