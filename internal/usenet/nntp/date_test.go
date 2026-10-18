package nntp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDateParsesServerTimeAsUTC(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) {
		s.line("111 20260102150405")
	}))
	c := dialOK(t, srv)
	got, err := c.Date(context.Background())
	require.NoError(t, err)
	require.True(t, got.Equal(time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)), got)
	require.Equal(t, time.UTC, got.Location())
	require.False(t, c.Dead())
	require.Equal(t, []string{"DATE"}, srv.receivedNoWait())
}

func TestDateMalformedTimestampIsProtocolError(t *testing.T) {
	for _, resp := range []string{
		"111",
		"111 ",
		"111 notadate",
		"111 2026010215040",    // 13 digits
		"111 202601021504055",  // 15 digits
		"111 20261301000000",   // month 13
		"111 20260230000000",   // Feb 30
		"111 2026-01-02T15:04", // wrong format
		"111 +0260102150405",
	} {
		t.Run(resp, func(t *testing.T) {
			srv := newFakeServer(t, serveBodies(func(s *session, cmd string) { s.line(resp) }))
			c := dialOK(t, srv)
			_, err := c.Date(context.Background())
			require.ErrorIs(t, err, ErrProtocol)
			require.True(t, c.Dead())
		})
	}
}

func TestDateOtherResponses(t *testing.T) {
	tests := []struct {
		resp string
		kind error
	}{
		{"500 what?", ErrConnection},
		{"502 go away", ErrConnection},
		{"400 shutting down", ErrConnection},
		{"200 not a date", ErrProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.resp, func(t *testing.T) {
			srv := newFakeServer(t, serveBodies(func(s *session, cmd string) { s.line(tt.resp) }))
			c := dialOK(t, srv)
			_, err := c.Date(context.Background())
			require.ErrorIs(t, err, tt.kind)
			require.True(t, c.Dead())
		})
	}
}

func TestDateConnectionLost(t *testing.T) {
	srv := newFakeServer(t, func(s *session) { greetOK(s); s.readCmd(); s.close() })
	c := dialOK(t, srv)
	_, err := c.Date(context.Background())
	require.ErrorIs(t, err, ErrConnection)
	require.True(t, c.Dead())
}

func TestDateContextDeadline(t *testing.T) {
	srv := newFakeServer(t, func(s *session) { greetOK(s); s.readCmd(); s.stall() })
	c := dialOK(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Date(ctx)
	require.ErrorIs(t, err, ErrConnection)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, c.Dead())
}

func TestDateOnDeadConnection(t *testing.T) {
	srv := newFakeServer(t, serveBodies(func(s *session, cmd string) { s.line("502 bye") }))
	c := dialOK(t, srv)
	_, _ = c.Date(context.Background())
	_, err := c.Date(context.Background())
	require.ErrorIs(t, err, ErrConnection)
	require.Equal(t, []string{"DATE"}, srv.received())
}
