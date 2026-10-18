package nntp

import (
	"context"
	"errors"
	"fmt"
)

// maxMessageIDLength matches the limit the NZB parser enforces.
const maxMessageIDLength = 250

var errConnDead = errors.New("connection is dead")

// Body fetches the body of the article with the given message ID (without
// angle brackets) and streams it to sink one line at a time.
//
// The lines are dot-unstuffed exactly once and have no CR/LF terminator. The
// slice passed to sink is only valid during the call; copy it to keep it. The
// whole body is never buffered.
//
// Errors:
//   - ErrInvalidMessageID: the ID was rejected before anything was sent.
//   - ErrArticleNotFound (430, 411, 412, 420, 423): the connection stays
//     usable.
//   - ErrConnection: 400, 502, any other 4xx or 5xx, an I/O error, a timeout
//     or context cancellation during the command. The connection is dead.
//   - ErrProtocol: a malformed or unexpected response, a line longer than
//     MaxLineLength or a body larger than MaxBodySize. The connection is
//     dead.
//   - A non-nil error from sink aborts the command and is returned wrapped.
//     It does not match ErrConnection, but the connection is dead because the
//     rest of the body is still in flight.
//
// A context that is already done returns its error without touching the
// connection.
func (c *Conn) Body(ctx context.Context, messageID string, sink func(line []byte) error) error {
	if err := validateMessageID(messageID); err != nil {
		return err
	}
	stop, err := c.begin(ctx, "body")
	if err != nil {
		return err
	}
	defer stop()

	code, text, err := c.roundTrip("BODY <" + messageID + ">")
	if err != nil {
		return c.fail(ctx, "body", err)
	}
	switch code {
	case 222:
	case 430, 411, 412, 420, 423:
		return &Error{Kind: ErrArticleNotFound, Op: "body", Code: code, Text: text}
	default:
		return c.refuse("body", code, text, classifyUnexpected(code))
	}
	return c.readBody(ctx, sink)
}

// readBody streams a dot-terminated block to sink.
func (c *Conn) readBody(ctx context.Context, sink func([]byte) error) error {
	total := 0
	for {
		line, n, err := c.readLine()
		if err != nil {
			return c.fail(ctx, "body", err)
		}
		total += n
		if total > MaxBodySize {
			return c.fail(ctx, "body", &Error{Kind: ErrProtocol, Text: "body too large"})
		}
		if len(line) > 0 && line[0] == '.' {
			if len(line) == 1 {
				return nil
			}
			line = line[1:] // dot-unstuffing, once
		}
		if err := sink(line); err != nil {
			c.kill()
			return fmt.Errorf("nntp: sink: %w", err)
		}
	}
}

// validateMessageID rejects anything that could break out of the BODY
// command: only printable ASCII without space, '<' or '>'.
func validateMessageID(id string) error {
	if id == "" || len(id) > maxMessageIDLength {
		return fmt.Errorf("%w: bad length", ErrInvalidMessageID)
	}
	for i := range len(id) {
		if b := id[i]; b <= ' ' || b >= 0x7f || b == '<' || b == '>' {
			return fmt.Errorf("%w: illegal character at offset %d", ErrInvalidMessageID, i)
		}
	}
	return nil
}
