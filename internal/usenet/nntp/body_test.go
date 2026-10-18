package nntp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// greetOK sends a greeting; the handlers below build on it.
func greetOK(s *session) { s.line("200 hi") }

// dialOK dials srv with its default config.
func dialOK(t *testing.T, srv *fakeServer) *Conn {
	t.Helper()
	return dialCfg(t, srv.config())
}

func dialCfg(t *testing.T, cfg Config) *Conn {
	t.Helper()
	c, err := Dial(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// collect returns a sink that copies every line it is given.
func collect(lines *[]string) func([]byte) error {
	return func(l []byte) error {
		*lines = append(*lines, string(l))
		return nil
	}
}

// serveBodies answers each command with fn until the client hangs up.
func serveBodies(fn func(s *session, cmd string)) func(*session) {
	return func(s *session) {
		greetOK(s)
		for {
			cmd, ok := s.readCmd()
			if !ok || cmd == "QUIT" {
				return
			}
			fn(s, cmd)
		}
	}
}

func TestBodyStreamsUnstuffedLinesWithoutTerminators(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("222 0 <a@b> body follows")
		s.line("=ybegin line=128")
		s.line("..foo") // stuffed: ".foo"
		s.line("...")   // stuffed: ".."
		s.line("")      // empty line
		s.line(".bar")  // never produced by a conforming server, but harmless
		s.line("a..b")  // dots inside a line are untouched
		s.send("lf\n")  // bare LF tolerated
		s.send(".\r\n") // terminator
		s.line("after") // must stay unread
	}))
	c := dialOK(t, srv)

	var got []string
	err := c.Body(context.Background(), "a@b", collect(&got))
	require.NoError(t, err)
	require.Equal(t, []string{"=ybegin line=128", ".foo", "..", "", "bar", "a..b", "lf"}, got)
	require.False(t, c.Dead())
	require.Equal(t, []string{"BODY <a@b>"}, srv.receivedNoWait()[:1])
}

func TestBodyEmptyBody(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("222 0 <a@b>")
		s.line(".")
	}))
	c := dialOK(t, srv)
	var got []string
	require.NoError(t, c.Body(context.Background(), "a@b", collect(&got)))
	require.Empty(t, got)
}

func TestBodyTerminatorSplitAcrossReads(t *testing.T) {
	for _, parts := range [][]string{
		{"222 ok\r\nline\r\n", ".", "\r\n"},
		{"222 ok\r\nline\r\n.", "\r", "\n"},
		{"22", "2 ok\r", "\nli", "ne\r\n.\r\n"},
	} {
		t.Run(fmt.Sprintf("%q", parts), func(t *testing.T) {
			srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
				for _, p := range parts {
					s.send(p)
					time.Sleep(15 * time.Millisecond)
				}
			}))
			c := dialOK(t, srv)
			var got []string
			require.NoError(t, c.Body(context.Background(), "a@b", collect(&got)))
			require.Equal(t, []string{"line"}, got)
			require.False(t, c.Dead())
		})
	}
}

func TestBodyArticleNotFoundCodesKeepConnectionUsable(t *testing.T) {
	for _, code := range []string{"430", "411", "412", "420", "423"} {
		t.Run(code, func(t *testing.T) {
			srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
				if cmd == "BODY <missing@x>" {
					s.line(code + " no such article")
					return
				}
				s.line("222 ok")
				s.line("data")
				s.line(".")
			}))
			c := dialOK(t, srv)

			err := c.Body(context.Background(), "missing@x", func([]byte) error { return nil })
			require.ErrorIs(t, err, ErrArticleNotFound)
			require.NotErrorIs(t, err, ErrConnection)
			var e *Error
			require.ErrorAs(t, err, &e)
			require.Equal(t, code, fmt.Sprint(e.Code))
			require.False(t, c.Dead())

			var got []string
			require.NoError(t, c.Body(context.Background(), "there@x", collect(&got)))
			require.Equal(t, []string{"data"}, got)
		})
	}
}

func TestBodyConnectionLevelCodesKillConnection(t *testing.T) {
	// 4xx other than the "absent" codes are treated as connection errors,
	// as are all 5xx.
	for _, code := range []string{"400", "502", "500", "503", "480", "450", "401"} {
		t.Run(code, func(t *testing.T) {
			srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
				s.line(code + " nope")
			}))
			c := dialOK(t, srv)
			called := false
			err := c.Body(context.Background(), "a@b", func([]byte) error { called = true; return nil })
			require.ErrorIs(t, err, ErrConnection)
			require.False(t, called)
			require.True(t, c.Dead())
		})
	}
}

func TestBodyUnexpectedSuccessCodeIsProtocolError(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("220 0 <a@b> that is ARTICLE, not BODY")
	}))
	c := dialOK(t, srv)
	err := c.Body(context.Background(), "a@b", func([]byte) error { return nil })
	require.ErrorIs(t, err, ErrProtocol)
	require.True(t, c.Dead())
}

func TestBodyConnectionLostMidBody(t *testing.T) {
	for name, tail := range map[string]string{
		"clean EOF between lines": "",
		"EOF inside a line":       "partial",
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeServer(t, func(s *session) {
				greetOK(s)
				s.readCmd()
				s.line("222 ok")
				s.line("one")
				s.send(tail)
				s.close()
			})
			c := dialOK(t, srv)
			var got []string
			err := c.Body(context.Background(), "a@b", collect(&got))
			require.ErrorIs(t, err, ErrConnection)
			require.Equal(t, []string{"one"}, got)
			require.True(t, c.Dead())
		})
	}
}

func TestBodyServerStatusMidBodyIsJustData(t *testing.T) {
	// "502 ..." inside the multiline block is body data, not a response.
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("222 ok")
		s.line("502 looks like a status")
		s.line(".")
	}))
	c := dialOK(t, srv)
	var got []string
	require.NoError(t, c.Body(context.Background(), "a@b", collect(&got)))
	require.Equal(t, []string{"502 looks like a status"}, got)
}

func TestBodyLineLengthLimit(t *testing.T) {
	tests := []struct {
		name    string
		n       int
		wantErr bool
	}{
		{"exactly the limit", MaxLineLength, false},
		{"one over", MaxLineLength + 1, true},
		{"larger than the read buffer", 3 * readBufferSize, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
				s.line("222 ok")
				s.line(strings.Repeat("x", tt.n))
				s.line(".")
			}))
			c := dialOK(t, srv)
			var got []string
			err := c.Body(context.Background(), "a@b", collect(&got))
			if tt.wantErr {
				require.ErrorIs(t, err, ErrProtocol)
				require.True(t, c.Dead())
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Len(t, got[0], tt.n)
		})
	}
}

func TestBodyLongStatusLineIsProtocolError(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("222 " + strings.Repeat("x", 3*readBufferSize))
	}))
	c := dialOK(t, srv)
	err := c.Body(context.Background(), "a@b", func([]byte) error { return nil })
	require.ErrorIs(t, err, ErrProtocol)
	require.True(t, c.Dead())
}

func TestBodySizeLimit(t *testing.T) {
	// The server streams 1 KiB lines forever; nothing is ever allocated
	// for the whole body, on either side.
	srv := newFakeServer(t, func(s *session) {
		greetOK(s)
		s.readCmd()
		s.line("222 ok")
		line := append(bytes.Repeat([]byte("y"), 1022), '\r', '\n')
		for {
			if _, err := s.conn.Write(line); err != nil {
				return
			}
		}
	})
	c := dialOK(t, srv)
	var total int
	err := c.Body(context.Background(), "a@b", func(l []byte) error {
		total += len(l) + 2
		return nil
	})
	require.ErrorIs(t, err, ErrProtocol)
	require.True(t, c.Dead())
	require.LessOrEqual(t, total, MaxBodySize)
	require.Greater(t, total, MaxBodySize-2048)
}

func TestBodyRejectsInvalidMessageIDsWithoutSending(t *testing.T) {
	ids := map[string]string{
		"empty":         "",
		"space":         "a b@c",
		"LF":            "a\n@c",
		"CR":            "a\r@c",
		"CRLF injected": "a@b>\r\nQUIT",
		"NUL":           "a\x00@c",
		"lt":            "<a@b",
		"gt":            "a@b>",
		"brackets":      "<a@b>",
		"tab":           "a\t@c",
		"DEL":           "a\x7f@c",
		"non-ASCII":     "aé@c",
		"too long":      strings.Repeat("a", 251),
	}
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("222 ok")
		s.line(".")
	}))
	c := dialOK(t, srv)
	for name, id := range ids {
		t.Run(name, func(t *testing.T) {
			err := c.Body(context.Background(), id, func([]byte) error { return nil })
			require.ErrorIs(t, err, ErrInvalidMessageID)
			require.False(t, c.Dead())
		})
	}
	require.NoError(t, c.Body(context.Background(), strings.Repeat("a", 250), func([]byte) error { return nil }))
	require.NoError(t, c.Close())
	require.Equal(t, []string{"BODY <" + strings.Repeat("a", 250) + ">", "QUIT"}, srv.received())
}

func TestBodyContextCancelledMidBodyAbortsBlockedRead(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		greetOK(s)
		s.readCmd()
		s.line("222 ok")
		s.line("first")
		s.stall() // the client must not wait for us
	})
	c := dialOK(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	err := c.Body(ctx, "a@b", func([]byte) error {
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, ErrConnection)
	require.True(t, c.Dead())
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestBodyContextDeadlineBoundsCommand(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		greetOK(s)
		s.readCmd()
		s.stall()
	})
	c := dialOK(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.Body(ctx, "a@b", func([]byte) error { return nil })
	require.ErrorIs(t, err, ErrConnection)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, c.Dead())
}

func TestBodyWithAlreadyCancelledContextDoesNothing(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("222 ok")
		s.line(".")
	}))
	c := dialOK(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Body(ctx, "a@b", func([]byte) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, c.Dead(), "nothing was sent, so the connection is still in sync")
	require.NoError(t, c.Body(context.Background(), "a@b", func([]byte) error { return nil }))
}

func TestBodySinkErrorAbortsAndKillsConnection(t *testing.T) {
	boom := errors.New("disk full")
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("222 ok")
		s.line("one")
		s.line("two")
		s.line(".")
	}))
	c := dialOK(t, srv)
	n := 0
	err := c.Body(context.Background(), "a@b", func([]byte) error {
		n++
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrConnection, "a local sink failure says nothing about the server")
	require.Equal(t, 1, n)
	require.True(t, c.Dead(), "unread body bytes would desync the connection")
}

func TestBodyIdleTimeoutAppliesPerRead(t *testing.T) {
	// The whole body takes longer than ReadTimeout, but no single gap does.
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("222 ok")
		for range 6 {
			time.Sleep(50 * time.Millisecond)
			s.line("tick")
		}
		s.line(".")
	}))
	cfg := srv.config()
	cfg.ReadTimeout = 200 * time.Millisecond
	c := dialCfg(t, cfg)
	var got []string
	require.NoError(t, c.Body(context.Background(), "a@b", collect(&got)))
	require.Len(t, got, 6)
}

func TestBodyIdleTimeoutKillsStalledConnection(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		greetOK(s)
		s.readCmd()
		s.line("222 ok")
		s.stall()
	})
	cfg := srv.config()
	cfg.ReadTimeout = 100 * time.Millisecond
	c := dialCfg(t, cfg)
	start := time.Now()
	err := c.Body(context.Background(), "a@b", func([]byte) error { return nil })
	require.ErrorIs(t, err, ErrConnection)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.True(t, c.Dead())
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestBodyOnDeadConnectionFailsFast(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) { s.line("502 go away") }))
	c := dialOK(t, srv)
	_ = c.Body(context.Background(), "a@b", func([]byte) error { return nil })
	require.True(t, c.Dead())
	err := c.Body(context.Background(), "a@b", func([]byte) error { return nil })
	require.ErrorIs(t, err, ErrConnection)
	require.Equal(t, []string{"BODY <a@b>"}, srv.received())
}

func TestCloseSendsQuitOnlyOnLiveConnection(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) { s.line("502 go away") }))
	c := dialOK(t, srv)
	_ = c.Body(context.Background(), "a@b", func([]byte) error { return nil })
	require.NoError(t, c.Close())
	require.NoError(t, c.Close(), "Close is idempotent")
	require.Equal(t, []string{"BODY <a@b>"}, srv.received())
}
