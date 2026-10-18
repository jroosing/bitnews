package nntp

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDialAcceptsGreeting200(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		s.line("200 welcome")
		s.readCmd()
	})
	c, err := Dial(context.Background(), srv.config())
	require.NoError(t, err)
	require.False(t, c.Dead())
	require.NoError(t, c.Close())
}

func TestDialAcceptsGreeting201PostingProhibited(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		s.line("201 no posting")
		s.readCmd()
	})
	c, err := Dial(context.Background(), srv.config())
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

func TestDialRejectsBadGreetings(t *testing.T) {
	tests := []struct {
		name     string
		greeting string
		kind     error
		code     int
	}{
		{"too many connections 502", "502 too many connections", ErrConnection, 502},
		{"service unavailable 400", "400 service temporarily unavailable", ErrConnection, 400},
		{"unexpected success code", "211 something else", ErrProtocol, 211},
		{"not a response", "hello there", ErrProtocol, 0},
		{"four digit code", "2000 hi", ErrProtocol, 0},
		{"code glued to text", "200hi", ErrProtocol, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t, func(s *session) { s.line(tt.greeting) })
			c, err := Dial(context.Background(), srv.config())
			require.Nil(t, c)
			require.ErrorIs(t, err, tt.kind)
			var e *Error
			require.ErrorAs(t, err, &e)
			require.Equal(t, tt.code, e.Code)
			require.Equal(t, "greeting", e.Op)
		})
	}
}

func TestDialServerClosesBeforeGreeting(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {})
	_, err := Dial(context.Background(), srv.config())
	require.ErrorIs(t, err, ErrConnection)
}

func TestDialErrorSanitizesAndTruncatesServerText(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		s.line("502 bad\x01\x1b[31m " + strings.Repeat("x", 1000) + "\xff")
	})
	_, err := Dial(context.Background(), srv.config())
	var e *Error
	require.ErrorAs(t, err, &e)
	require.LessOrEqual(t, len(e.Text), 200)
	for _, r := range e.Text {
		require.True(t, r >= 0x20 && r <= 0x7e, "unprintable %q", r)
	}
}

func TestDialRejectsBadConfig(t *testing.T) {
	_, err := Dial(context.Background(), Config{})
	require.ErrorIs(t, err, ErrConfig)
	_, err = Dial(context.Background(), Config{Host: "h", User: "u\r\nQUIT", Pass: "p"})
	require.ErrorIs(t, err, ErrConfig)
	require.NotContains(t, err.Error(), "QUIT")
}
