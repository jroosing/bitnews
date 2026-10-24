package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// Login errors. Use errors.Is.
var (
	// ErrInvalidCredentials means the username or password is wrong. It is
	// the same error for an unknown user and a wrong password, so the
	// response cannot be used to find out which usernames exist.
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	// ErrRateLimited means the limiter is holding the attempt back. The
	// concrete error is a *RateLimitError.
	ErrRateLimited = errors.New("auth: too many failed login attempts")
)

// RateLimitError is returned by Login while the limiter blocks the attempt.
// It matches ErrRateLimited with errors.Is.
type RateLimitError struct {
	// RetryAfter is how long the client must wait; send it as Retry-After.
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%v: retry in %s", ErrRateLimited, e.RetryAfter.Round(time.Second))
}

// Is makes a *RateLimitError match ErrRateLimited.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// Login checks username and password and starts a session, returning its
// plaintext token for the cookie.
//
// The limiter is asked first: a blocked attempt returns a *RateLimitError
// without touching the password, so a blocked guesser costs no argon2 time.
// An unknown username is verified against a dummy hash made with the current
// parameters, so unknown users and wrong passwords take the same path and the
// same time. After a successful verify, a hash made with outdated parameters
// is replaced (best effort), and both limiter keys are cleared. Failures are
// logged with the username and client IP, never the password.
func (s *SessionService) Login(ctx context.Context, username string, password core.Secret, clientIP netip.Addr) (string, error) {
	if wait := s.limiter.Check(clientIP, username); wait > 0 {
		return "", &RateLimitError{RetryAfter: wait}
	}
	if len(password.Reveal()) > MaxPasswordBytes {
		// Cannot be a valid password; do not feed a huge body to argon2.
		s.loginFailed(username, clientIP, "password too long")
		return "", ErrInvalidCredentials
	}

	user, err := s.users.GetByUsername(ctx, username)
	known := err == nil
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return "", fmt.Errorf("auth: look up user: %w", err)
	}
	hash := user.PasswordHash
	if !known {
		var herr error
		if hash, herr = s.unknownUserHash(); herr != nil {
			return "", fmt.Errorf("auth: dummy hash: %w", herr)
		}
	}
	ok, verr := s.verify(hash, password)
	if verr != nil {
		s.log.Error("stored password hash is unusable", "user_id", user.ID, "err", verr)
	}
	if !ok || !known || verr != nil {
		reason := "wrong password"
		switch {
		case !known:
			reason = "unknown user"
		case verr != nil:
			reason = "unusable stored hash"
		}
		s.loginFailed(username, clientIP, reason)
		return "", ErrInvalidCredentials
	}

	s.limiter.Success(clientIP, username)
	if NeedsRehash(user.PasswordHash) {
		s.rehash(ctx, user, password)
	}
	token, _, err := s.CreateSession(ctx, user.ID)
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *SessionService) loginFailed(username string, ip netip.Addr, reason string) {
	s.limiter.Fail(ip, username)
	s.log.Warn("login failed", "username", username, "client_ip", ip.String(), "reason", reason)
}

// rehash upgrades a stored hash to the current parameters. Failing is only
// logged: the user did log in, and the next login tries again.
func (s *SessionService) rehash(ctx context.Context, user core.User, password core.Secret) {
	fresh, err := HashPassword(password)
	if err == nil {
		err = s.users.UpdatePasswordHash(ctx, user.ID, fresh)
	}
	if err != nil {
		s.log.Warn("password rehash failed", "user_id", user.ID, "err", err)
	}
}
