package nntp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// begin starts a command: it fails fast on a dead connection or a cancelled
// context, makes the
// context deadline cap every I/O deadline, and arranges for cancellation to
// abort blocking I/O by closing the connection. The returned func must be
// called when the command is over.
func (c *Conn) begin(ctx context.Context, op string) (func(), error) {
	if c.Dead() {
		return func() {}, &Error{Kind: ErrConnection, Op: op, Err: errConnDead}
	}
	if err := ctx.Err(); err != nil {
		return func() {}, fmt.Errorf("nntp: %w", err)
	}
	c.cmdDeadline, _ = ctx.Deadline()
	stop := context.AfterFunc(ctx, c.kill)
	return func() {
		stop()
		c.cmdDeadline = time.Time{}
	}, nil
}

// kill marks the connection dead and closes it without sending QUIT.
func (c *Conn) kill() {
	c.dead.Store(true)
	_ = c.nc.Close()
}

// fail kills the connection and converts err into the package's error
// types. Protocol errors keep their kind.
func (c *Conn) fail(ctx context.Context, op string, err error) error {
	c.kill()
	if e, ok := errors.AsType[*Error](err); ok {
		if e.Op == "" {
			e.Op = op
		}
		return e
	}
	if cerr := ctx.Err(); cerr != nil {
		err = cerr
	} else if errors.Is(err, os.ErrDeadlineExceeded) && !c.cmdDeadline.IsZero() && !time.Now().Before(c.cmdDeadline) {
		// The I/O deadline is the context deadline and fired a moment
		// before the context noticed.
		err = context.DeadlineExceeded
	}
	return &Error{Kind: ErrConnection, Op: op, Err: err}
}

// readLine returns the next line without its terminator, and the number of
// bytes consumed from the wire. The slice is valid until the next read.
func (c *Conn) readLine() (line []byte, n int, err error) {
	b, err := c.br.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return nil, 0, &Error{Kind: ErrProtocol, Text: "line too long"}
	case errors.Is(err, io.EOF) && len(b) > 0:
		return nil, 0, io.ErrUnexpectedEOF
	case err != nil:
		return nil, 0, err
	}
	n = len(b)
	b = b[:n-1]
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	if len(b) > MaxLineLength {
		return nil, 0, &Error{Kind: ErrProtocol, Text: "line too long"}
	}
	return b, n, nil
}

// readResponse reads a status line: three digits, optionally followed by a
// space and text.
func (c *Conn) readResponse() (code int, text string, err error) {
	l, _, err := c.readLine()
	if err != nil {
		return 0, "", err
	}
	if len(l) < 3 || l[0] < '1' || l[0] > '5' || !isDigit(l[1]) || !isDigit(l[2]) || (len(l) > 3 && l[3] != ' ') {
		return 0, "", &Error{Kind: ErrProtocol, Text: sanitize("malformed response: " + string(l[:min(len(l), 60)]))}
	}
	code = int(l[0]-'0')*100 + int(l[1]-'0')*10 + int(l[2]-'0')
	if len(l) > 4 {
		text = sanitize(redact(string(l[4:min(len(l), 4+4*maxErrorText)]), c.secrets...))
	}
	return code, text, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// classifyUnexpected maps a status code the command did not expect to a
// sentinel: 4xx and 5xx are connection-level refusals, anything else is a
// protocol violation.
func classifyUnexpected(code int) error {
	if code >= 400 {
		return ErrConnection
	}
	return ErrProtocol
}
