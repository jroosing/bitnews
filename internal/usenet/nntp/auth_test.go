package nntp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthUserThenPass(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		s.line("200 hi")
		s.expect("AUTHINFO USER alice")
		s.line("381 password required")
		s.expect("AUTHINFO PASS s3cret")
		s.line("281 ok")
		s.readCmd()
	})
	cfg := srv.config()
	cfg.User, cfg.Pass = "alice", "s3cret"
	c, err := Dial(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, c.Close())
	require.Equal(t, []string{"AUTHINFO USER alice", "AUTHINFO PASS s3cret", "QUIT"}, srv.received())
}

func TestAuthAcceptedAfterUserAlone(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		s.line("200 hi")
		s.expect("AUTHINFO USER alice")
		s.line("281 no password needed")
		s.readCmd()
	})
	cfg := srv.config()
	cfg.User, cfg.Pass = "alice", "ignored"
	c, err := Dial(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, c.Close())
	require.Equal(t, []string{"AUTHINFO USER alice", "QUIT"}, srv.received())
}

func TestNoAuthServerNeverSeesAuthinfo(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		s.line("200 open server")
		s.readCmd()
	})
	c, err := Dial(context.Background(), srv.config())
	require.NoError(t, err)
	require.NoError(t, c.Close())
	require.Equal(t, []string{"QUIT"}, srv.received())
}

func TestAuthFailureCodesAreErrAuth(t *testing.T) {
	for _, code := range []string{"481 bad credentials", "482 rejected", "452 not allowed"} {
		for _, atPass := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s at pass=%v", code, atPass), func(t *testing.T) {
				srv := newFakeServer(t, func(s *session) {
					s.line("200 hi")
					s.readCmd()
					if atPass {
						s.line("381 pass")
						s.readCmd()
					}
					s.line(code)
				})
				cfg := srv.config()
				cfg.User, cfg.Pass = "alice", "hunter2-secret"
				c, err := Dial(context.Background(), cfg)
				require.Nil(t, c)
				require.ErrorIs(t, err, ErrAuth)
				require.NotErrorIs(t, err, ErrConnection)
				require.NotContains(t, err.Error(), "hunter2-secret")
				require.NotContains(t, err.Error(), "alice")
				var e *Error
				require.ErrorAs(t, err, &e)
				require.Equal(t, "auth", e.Op)
			})
		}
	}
}

func TestAuthOtherCodes(t *testing.T) {
	tests := []struct {
		name string
		resp string
		kind error
	}{
		{"502 may mean too many connections", "502 try later", ErrConnection},
		{"400 shutting down", "400 bye", ErrConnection},
		{"unexpected success code", "200 what", ErrProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t, func(s *session) {
				s.line("200 hi")
				s.readCmd()
				s.line(tt.resp)
			})
			cfg := srv.config()
			cfg.User, cfg.Pass = "u", "p"
			_, err := Dial(context.Background(), cfg)
			require.ErrorIs(t, err, tt.kind)
			require.NotErrorIs(t, err, ErrAuth)
		})
	}
}

func TestAuthErrorRedactsEchoedCredentials(t *testing.T) {
	srv := newFakeServer(t, func(s *session) {
		s.line("200 hi")
		s.readCmd()
		s.line("381 pass")
		s.readCmd()
		s.line("481 wrong password hunter2-secret for alice")
	})
	cfg := srv.config()
	cfg.User, cfg.Pass = "alice", "hunter2-secret"
	_, err := Dial(context.Background(), cfg)
	require.ErrorIs(t, err, ErrAuth)
	require.NotContains(t, err.Error(), "hunter2-secret")
	require.NotContains(t, err.Error(), "alice")
}
