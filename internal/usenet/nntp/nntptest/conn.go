package nntptest

import (
	"context"
	"fmt"
	"io"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
)

// maxMessageIDLength matches the limit nntp.Conn.Body enforces.
const maxMessageIDLength = 250

var errConnDead = fmt.Errorf("connection is dead")

// begin mirrors nntp.Conn: a dead connection or a done context fails before
// anything is sent.
func (c *conn) begin(ctx context.Context, op string) error {
	if c.Dead() {
		return &nntp.Error{Kind: nntp.ErrConnection, Op: op, Err: errConnDead}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("nntp: %w", err)
	}
	return nil
}

func (c *conn) Body(ctx context.Context, id string, sink func(line []byte) error) error {
	if err := validateMessageID(id); err != nil {
		return err
	}
	if err := c.begin(ctx, "body"); err != nil {
		return err
	}
	c.s.mu.Lock()
	c.s.total++
	c.s.requests[id]++
	c.s.connReqs[c.num]++
	lines, ok := c.s.articles[id]
	ok = ok && !c.s.missing[id]
	if ok && c.s.corrupt[id] {
		lines = corruptLines(lines)
	}
	latency := c.s.latency
	cut := -1
	if d := c.s.disconnects[id]; d != nil && d.remaining != 0 {
		cut = d.after
		if d.remaining > 0 {
			d.remaining--
		}
	}
	c.s.mu.Unlock()
	if err := c.wait(ctx, "body", latency); err != nil {
		return err
	}
	if !ok {
		return &nntp.Error{Kind: nntp.ErrArticleNotFound, Op: "body", Code: 430, Text: "No such article"}
	}

	var buf []byte
	for i, l := range lines {
		if i == cut {
			break
		}
		// Reuse one buffer, as the real client does, so sinks that keep
		// the slice are caught.
		buf = append(buf[:0], l...)
		if err := sink(buf); err != nil {
			c.kill()
			return fmt.Errorf("nntp: sink: %w", err)
		}
		clear(buf)
	}
	if cut >= 0 && cut <= len(lines) {
		c.kill()
		return &nntp.Error{Kind: nntp.ErrConnection, Op: "body", Err: io.ErrUnexpectedEOF}
	}
	return nil
}

// wait sleeps for the scripted latency. Like the real client, a command
// aborted by its context leaves the connection dead.
func (c *conn) wait(ctx context.Context, op string, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		c.kill()
		return &nntp.Error{Kind: nntp.ErrConnection, Op: op, Err: ctx.Err()}
	}
}

func (c *conn) Date(ctx context.Context) (time.Time, error) {
	if err := c.begin(ctx, "date"); err != nil {
		return time.Time{}, err
	}
	c.s.mu.Lock()
	latency := c.s.latency
	c.s.mu.Unlock()
	if err := c.wait(ctx, "date", latency); err != nil {
		return time.Time{}, err
	}
	return time.Now().UTC().Truncate(time.Second), nil
}

// validateMessageID applies the same rule as nntp.Conn.Body.
func validateMessageID(id string) error {
	if id == "" || len(id) > maxMessageIDLength {
		return fmt.Errorf("%w: bad length", nntp.ErrInvalidMessageID)
	}
	for i := range len(id) {
		if b := id[i]; b <= ' ' || b >= 0x7f || b == '<' || b == '>' {
			return fmt.Errorf("%w: illegal character at offset %d", nntp.ErrInvalidMessageID, i)
		}
	}
	return nil
}
