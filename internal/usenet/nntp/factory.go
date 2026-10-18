package nntp

import (
	"context"
	"time"
)

// ArticleConn is the part of a Conn that the connection pool and everything
// above it need. *Conn implements it; tests above the client use the
// in-memory implementation from package nntptest, so they need no TLS.
//
// Implementations follow the contract documented on Conn: one command at a
// time, and only Dead may be called concurrently.
type ArticleConn interface {
	// Body streams the article body to sink, as Conn.Body does.
	Body(ctx context.Context, messageID string, sink func(line []byte) error) error
	// Date asks the Server for its clock, as Conn.Date does.
	Date(ctx context.Context) (time.Time, error)
	// Close ends the connection. It is safe to call more than once.
	Close() error
	// Dead reports whether the connection can no longer be used.
	Dead() bool
}

var _ ArticleConn = (*Conn)(nil)

// ConnFactory opens one authenticated connection to a Server. Failures
// follow the package's error kinds; in particular an error matching ErrAuth
// disables the pool that called it.
type ConnFactory func(ctx context.Context) (ArticleConn, error)

// DialFactory returns a ConnFactory that calls Dial with cfg.
func DialFactory(cfg Config) ConnFactory {
	return func(ctx context.Context) (ArticleConn, error) {
		c, err := Dial(ctx, cfg)
		if err != nil {
			return nil, err // not c: a nil *Conn would be a non-nil interface
		}
		return c, nil
	}
}
