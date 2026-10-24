package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/jroosing/bitnews/internal/core"
)

// ChangePassword replaces the password of the user. It needs the current
// password, so a stolen session cannot lock the owner out, and applies the
// rules of ValidatePassword to the new one.
//
// Wrong current passwords count against the login limiter like a failed
// login (same keys: the client IP and the username), and a blocked attempt
// is a *RateLimitError without any password work. A wrong current password
// is ErrInvalidCredentials; a bad new password is one of the
// ErrPassword... errors; an unknown user is core.ErrNotFound.
//
// The new hash uses the current argon2 parameters. Afterwards every other
// session of the user ends: keepToken is the plaintext token of the session
// that made the change, which stays signed in (empty keeps none). It returns
// how many sessions it ended. Failing to end them is reported, but the
// password has changed by then.
func (s *SessionService) ChangePassword(ctx context.Context, userID core.ID, current, next core.Secret, keepToken string, clientIP netip.Addr) (ended int, err error) {
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("auth: look up user: %w", err)
	}
	if wait := s.limiter.Check(clientIP, user.Username); wait > 0 {
		return 0, &RateLimitError{RetryAfter: wait}
	}
	if len(current.Reveal()) > MaxPasswordBytes {
		s.loginFailed(user.Username, clientIP, "password change: current password too long")
		return 0, ErrInvalidCredentials
	}
	ok, verr := s.verify(user.PasswordHash, current)
	if verr != nil {
		s.log.Error("stored password hash is unusable", "user_id", user.ID, "err", verr)
	}
	if !ok || verr != nil {
		s.loginFailed(user.Username, clientIP, "password change: wrong current password")
		return 0, ErrInvalidCredentials
	}
	s.limiter.Success(clientIP, user.Username)

	if err := ValidatePassword(next); err != nil {
		return 0, err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return 0, fmt.Errorf("auth: hash new password: %w", err)
	}
	if err := s.users.UpdatePasswordHash(ctx, user.ID, hash); err != nil {
		return 0, fmt.Errorf("auth: store new password: %w", err)
	}
	s.log.Info("password changed", "user_id", user.ID, "client_ip", clientIP.String())

	var keep core.TokenHash
	if kind, valid := KindOf(keepToken); valid && kind == KindSession {
		keep = HashToken(keepToken)
	}
	ended, err = s.sessions.DeleteForUserExcept(ctx, user.ID, keep)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return 0, fmt.Errorf("auth: end other sessions: %w", err)
	}
	return ended, nil
}
