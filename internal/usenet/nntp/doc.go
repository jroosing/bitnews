// Package nntp is a strict NNTP client over TLS with per-Server connection pools.
//
// Dial opens one Conn: TLS 1.2 or newer with the system roots and the
// certificate checked against Config.Host, then the greeting and, when
// Config.User is set, AUTHINFO. There is deliberately no option for plain text
// or skipping certificate verification.
//
// A Conn runs one command at a time (Body, Date, Close). Failures are
// *Error values matching exactly one of ErrArticleNotFound, ErrAuth,
// ErrConnection or ErrProtocol with errors.Is. Everything except
// ErrArticleNotFound (and a rejected message ID or an already-cancelled
// context, which send nothing) leaves the Conn dead: Conn.Dead reports true and
// the caller must discard it.
//
// Body streams a dot-terminated block line by line to a callback. The dot-
// unstuffing happens here, once; the lines handed to the callback are the
// literal article body lines, which is what the yenc package expects. Lines
// are limited to MaxLineLength bytes and a body to MaxBodySize bytes.
//
// Every command is bounded by the context deadline, and by Config.ReadTimeout
// as an idle timeout on each read and write. Cancelling the context aborts
// blocking I/O by closing the connection.
package nntp
