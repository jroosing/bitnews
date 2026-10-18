package nntp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// MaxLineLength is the longest line (without its CRLF) accepted from a
	// Server.
	MaxLineLength = 8 << 10
	// MaxBodySize is the most bytes of one body (including line
	// terminators) accepted from a Server.
	MaxBodySize = 16 << 20

	readBufferSize = 16 << 10
	quitTimeout    = 2 * time.Second
)

// Conn is one authenticated NNTP connection. It is not safe for concurrent
// use, except that Dead may be called at any time.
type Conn struct {
	nc          net.Conn
	br          *bufio.Reader
	readTimeout time.Duration
	// cmdDeadline is the context deadline of the running command.
	cmdDeadline time.Time
	dead        atomic.Bool
	// secrets are scrubbed from any server text kept in errors, because
	// servers sometimes echo credentials back.
	secrets []string
}

// Dial connects over TLS, reads the greeting and authenticates when cfg.User
// is set. Failures are *Error values; errors.As can reach the underlying
// net, DNS or certificate error.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("%w: empty host", ErrConfig)
	}
	if strings.ContainsAny(cfg.User, "\r\n\x00") || strings.ContainsAny(cfg.Pass, "\r\n\x00") {
		return nil, fmt.Errorf("%w: credentials contain control characters", ErrConfig)
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}
	if cfg.Dialer == nil {
		cfg.Dialer = &net.Dialer{Timeout: cfg.ConnectTimeout}
	}

	connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	raw, err := cfg.Dialer.DialContext(connectCtx, "tcp", addr)
	if err != nil {
		return nil, &Error{Kind: ErrConnection, Op: "dial", Err: err}
	}
	tc := tls.Client(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.Host,
		RootCAs:    cfg.rootCAs,
	})
	if err := tc.HandshakeContext(connectCtx); err != nil {
		_ = raw.Close()
		return nil, &Error{Kind: ErrConnection, Op: "tls", Err: err}
	}

	c := newConn(tc, cfg.ReadTimeout)
	c.secrets = []string{cfg.User, cfg.Pass}
	if err := c.handshake(ctx, cfg); err != nil {
		_ = c.nc.Close()
		return nil, err
	}
	return c, nil
}

func newConn(nc net.Conn, readTimeout time.Duration) *Conn {
	c := &Conn{nc: nc, readTimeout: readTimeout}
	c.br = bufio.NewReaderSize(idleReader{c}, readBufferSize)
	return c
}

// idleReader refreshes the read deadline before every read so that
// ReadTimeout is an idle timeout, not a limit on the whole command.
type idleReader struct{ c *Conn }

func (r idleReader) Read(p []byte) (int, error) {
	_ = r.c.nc.SetReadDeadline(r.c.deadline())
	return r.c.nc.Read(p)
}

// deadline is the earlier of now+readTimeout and the command's context
// deadline.
func (c *Conn) deadline() time.Time {
	d := time.Now().Add(c.readTimeout)
	if !c.cmdDeadline.IsZero() && c.cmdDeadline.Before(d) {
		return c.cmdDeadline
	}
	return d
}

// Dead reports whether the connection can no longer be used.
func (c *Conn) Dead() bool { return c.dead.Load() }

// Close sends QUIT on a best-effort basis and closes the connection. It is
// safe to call more than once.
func (c *Conn) Close() error {
	if !c.dead.Swap(true) {
		_ = c.nc.SetWriteDeadline(time.Now().Add(quitTimeout))
		_, _ = c.nc.Write([]byte("QUIT\r\n"))
	}
	if err := c.nc.Close(); !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// handshake reads the greeting and authenticates.
func (c *Conn) handshake(ctx context.Context, cfg Config) error {
	stop, err := c.begin(ctx, "greeting")
	if err != nil {
		return err
	}
	defer stop()

	code, text, err := c.readResponse()
	if err != nil {
		return c.fail(ctx, "greeting", err)
	}
	if code != 200 && code != 201 {
		return c.refuse("greeting", code, text, classifyUnexpected(code))
	}
	if cfg.User == "" {
		return nil
	}

	// A server may accept the user name alone (281) or ask for a password
	// (381). Failure codes are terminal for this Server until its config
	// changes. 502 is deliberately not one of them: providers also use it
	// for "too many connections".
	code, text, err = c.roundTrip("AUTHINFO USER " + cfg.User)
	if err != nil {
		return c.fail(ctx, "auth", err)
	}
	if code == 381 {
		code, text, err = c.roundTrip("AUTHINFO PASS " + cfg.Pass)
		if err != nil {
			return c.fail(ctx, "auth", err)
		}
	}
	switch code {
	case 281:
		return nil
	case 481, 482, 452:
		return c.refuse("auth", code, text, ErrAuth)
	default:
		return c.refuse("auth", code, text, classifyUnexpected(code))
	}
}

// refuse kills the connection and reports an unexpected response. The
// server text is sanitized; credentials are never part of it.
func (c *Conn) refuse(op string, code int, text string, kind error) error {
	c.kill()
	return &Error{Kind: kind, Op: op, Code: code, Text: text}
}

// roundTrip sends one command and reads its status line.
func (c *Conn) roundTrip(cmd string) (int, string, error) {
	if err := c.send(cmd); err != nil {
		return 0, "", err
	}
	return c.readResponse()
}

// send writes one command line.
func (c *Conn) send(cmd string) error {
	_ = c.nc.SetWriteDeadline(c.deadline())
	_, err := c.nc.Write([]byte(cmd + "\r\n"))
	return err
}

// redact replaces every occurrence of the secrets in text.
func redact(text string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			text = strings.ReplaceAll(text, sec, "***")
		}
	}
	return text
}
