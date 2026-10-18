package nntp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// replayConn is a net.Conn whose read side is a fixed byte stream and whose
// writes are discarded.
type replayConn struct {
	net.Conn // nil; only the methods below are used
	r        *bytes.Reader
}

func (c *replayConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *replayConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *replayConn) Close() error                     { return nil }
func (c *replayConn) SetDeadline(time.Time) error      { return nil }
func (c *replayConn) SetReadDeadline(time.Time) error  { return nil }
func (c *replayConn) SetWriteDeadline(time.Time) error { return nil }

func newReplayConn(server []byte) *Conn {
	return newConn(&replayConn{r: bytes.NewReader(server)}, time.Second)
}

func FuzzReadResponse(f *testing.F) {
	for _, s := range []string{
		"200 hi\r\n", "222 0 <a@b>\r\n", "430\r\n", "", "\r\n", "99 x\r\n",
		"2000 x\r\n", "200hi\r\n", "111 20260102150405\r\n", "502 \xff\x00\r\n",
		"200 " + string(bytes.Repeat([]byte("x"), 9000)) + "\r\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, server []byte) {
		c := newReplayConn(server)
		code, text, err := c.readResponse()
		if err != nil {
			return
		}
		if code < 100 || code > 599 {
			t.Fatalf("code %d out of range", code)
		}
		if len(text) > maxErrorText {
			t.Fatalf("text not truncated: %d bytes", len(text))
		}
		for _, r := range text {
			if r < 0x20 || r > 0x7e {
				t.Fatalf("unprintable %q in text", r)
			}
		}
	})
}

func FuzzBody(f *testing.F) {
	for _, s := range []string{
		"222 ok\r\nline\r\n..stuffed\r\n.\r\n",
		"222 ok\r\n.\r\n",
		"222 ok\r\nno terminator\r\n",
		"222 ok\r\npartial",
		"222 ok\nlf only\n.\n",
		"430 gone\r\n",
		"502 bye\r\n",
		"garbage",
		"",
		"222 ok\r\n" + string(bytes.Repeat([]byte("y"), 9000)) + "\r\n.\r\n",
	} {
		f.Add([]byte(s))
	}
	known := []error{ErrArticleNotFound, ErrConnection, ErrProtocol}
	f.Fuzz(func(t *testing.T, server []byte) {
		c := newReplayConn(server)
		var total int
		err := c.Body(context.Background(), "a@b", func(line []byte) error {
			if bytes.IndexByte(line, '\n') >= 0 {
				t.Fatalf("line contains LF: %q", line)
			}
			if len(line) > MaxLineLength {
				t.Fatalf("line of %d bytes passed the limit", len(line))
			}
			total += len(line) + 2
			return nil
		})
		if total > MaxBodySize {
			t.Fatalf("delivered %d bytes, over the body limit", total)
		}
		if err == nil {
			return
		}
		ok := false
		for _, k := range known {
			ok = ok || errors.Is(err, k)
		}
		if !ok {
			t.Fatalf("unclassified error: %v", err)
		}
		if !errors.Is(err, ErrArticleNotFound) && !c.Dead() {
			t.Fatalf("connection alive after %v", err)
		}
	})
}
