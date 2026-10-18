package nntp

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Sentinel errors. Every error returned by this package matches exactly one
// of the first four with errors.Is, except sink errors from Body (see Body).
var (
	// ErrArticleNotFound means the Server does not have the Article. The
	// connection is still usable.
	ErrArticleNotFound = errors.New("nntp: article not found")
	// ErrAuth means the Server rejected the credentials. It is terminal for
	// that Server until its configuration changes.
	ErrAuth = errors.New("nntp: authentication failed")
	// ErrConnection means the connection failed or the Server reported a
	// condition that makes it unusable. Conn.Dead reports true afterwards.
	ErrConnection = errors.New("nntp: connection error")
	// ErrProtocol means the Server broke the protocol or exceeded a limit.
	// The connection is dead afterwards.
	ErrProtocol = errors.New("nntp: protocol error")

	// ErrInvalidMessageID means a message ID was rejected before anything
	// was sent. The connection is unaffected.
	ErrInvalidMessageID = errors.New("nntp: invalid message id")
	// ErrConfig means the Config is unusable.
	ErrConfig = errors.New("nntp: invalid config")
)

// maxErrorText caps the server text kept in an Error.
const maxErrorText = 200

// Error carries what the Server said (or what went wrong) alongside the
// sentinel classifying it. It never contains credentials.
type Error struct {
	// Kind is one of the sentinel errors.
	Kind error
	// Op names the step that failed, such as "dial", "tls", "greeting",
	// "auth", "body" or "date".
	Op string
	// Code is the three-digit response code, or 0 when there was none.
	Code int
	// Text is the sanitized, truncated response text.
	Text string
	// Err is the underlying cause (I/O error, context error), if any.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Kind.Error())
	if e.Op != "" {
		b.WriteString(" during ")
		b.WriteString(e.Op)
	}
	if e.Code != 0 {
		fmt.Fprintf(&b, ": %d", e.Code)
		if e.Text != "" {
			b.WriteString(" ")
			b.WriteString(e.Text)
		}
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap lets errors.Is match both the Kind and the underlying cause.
func (e *Error) Unwrap() []error {
	if e.Err == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.Err}
}

// sanitize makes server-supplied text safe to log: printable ASCII only,
// at most maxErrorText bytes.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxErrorText {
			break
		}
		switch {
		case r == utf8.RuneError, r < 0x20, r > 0x7e:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
