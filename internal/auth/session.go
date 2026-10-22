package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Session lifetimes and the touch throttle from the design.
const (
	// SessionIdleTimeout ends a session that was not used for this long.
	SessionIdleTimeout = 7 * 24 * time.Hour
	// SessionAbsoluteTimeout ends a session this long after login, however
	// active it was.
	SessionAbsoluteTimeout = 30 * 24 * time.Hour
	// sessionTouchInterval limits how often last_seen_at is written, so a
	// busy UI does not turn every request into a database write.
	sessionTouchInterval = time.Minute
)

// ErrInvalidSession means a session token is unknown, malformed or expired.
// Callers should not tell the client which.
var ErrInvalidSession = errors.New("auth: invalid session")

// SessionExpiry returns when s stops being valid if it is not used again:
// the earlier of idle expiry (from last use) and absolute expiry (from login).
func SessionExpiry(s core.Session) time.Time {
	idle := s.LastSeenAt.Add(SessionIdleTimeout)
	abs := s.CreatedAt.Add(SessionAbsoluteTimeout)
	if abs.Before(idle) {
		return abs
	}
	return idle
}

// SessionService creates, checks and ends UI sessions, and owns the login
// flow (see Login). Only the SHA-256 hash of a session token is stored.
type SessionService struct {
	sessions core.SessionRepo
	users    core.UserRepo
	limiter  *LoginLimiter
	log      *slog.Logger
	now      func() time.Time
	verify   func(hash []byte, pw core.Secret) (bool, error)

	// dummyHash is verified when a login names an unknown user, so that
	// path costs the same argon2 run as a wrong password for a real user.
	dummyHash []byte
}

// SessionOption configures NewSessionService.
type SessionOption func(*SessionService)

// WithSessionClock sets the time source.
func WithSessionClock(now func() time.Time) SessionOption {
	return func(s *SessionService) { s.now = now }
}

// WithLoginLimiter sets the limiter; the default is a fresh LoginLimiter on
// the service's clock.
func WithLoginLimiter(l *LoginLimiter) SessionOption {
	return func(s *SessionService) { s.limiter = l }
}

// WithSessionLogger sets the logger for failed logins and background errors.
// Passwords and tokens are never logged.
func WithSessionLogger(l *slog.Logger) SessionOption {
	return func(s *SessionService) { s.log = l }
}

// NewSessionService returns a SessionService. It hashes one random password
// with the current argon2 parameters for the unknown-user login path, so
// build it after the process-wide parameters are settled.
func NewSessionService(sessions core.SessionRepo, users core.UserRepo, opts ...SessionOption) (*SessionService, error) {
	s := &SessionService{
		sessions: sessions,
		users:    users,
		log:      slog.Default(),
		now:      time.Now,
		verify:   VerifyPassword,
	}
	for _, o := range opts {
		o(s)
	}
	if s.limiter == nil {
		s.limiter = NewLoginLimiter(WithLimiterClock(s.now))
	}
	filler, _, err := NewToken()
	if err != nil {
		return nil, err
	}
	s.dummyHash, err = HashPassword(core.Secret(filler))
	if err != nil {
		return nil, fmt.Errorf("auth: dummy hash: %w", err)
	}
	return s, nil
}

// CreateSession starts a session for userID and returns its plaintext token,
// which goes into the cookie and is never stored, and the latest time the
// session can live (absolute expiry; idle expiry may end it sooner).
func (s *SessionService) CreateSession(ctx context.Context, userID core.ID) (token string, expires time.Time, err error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	err = s.sessions.Create(ctx, core.Session{TokenHash: hash, UserID: userID, CreatedAt: now, LastSeenAt: now})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: create session: %w", err)
	}
	return token, now.Add(SessionAbsoluteTimeout), nil
}

// Authenticate resolves a session token to its Session and User. A token that
// is unknown, malformed, expired (7 days idle or 30 days absolute) or whose
// user is gone is ErrInvalidSession; expired and orphaned rows are deleted.
// last_seen_at is renewed at most once a minute. Other errors are repository
// failures.
func (s *SessionService) Authenticate(ctx context.Context, token string) (core.Session, core.User, error) {
	if kind, ok := KindOf(token); !ok || kind != KindSession {
		return core.Session{}, core.User{}, ErrInvalidSession
	}
	hash := HashToken(token)
	sess, err := s.sessions.Get(ctx, hash)
	if errors.Is(err, core.ErrNotFound) {
		return core.Session{}, core.User{}, ErrInvalidSession
	}
	if err != nil {
		return core.Session{}, core.User{}, fmt.Errorf("auth: get session: %w", err)
	}
	if !TokenHashEqual(sess.TokenHash, hash) {
		return core.Session{}, core.User{}, ErrInvalidSession
	}
	now := s.now()
	if !now.Before(SessionExpiry(sess)) {
		s.dropSession(ctx, hash)
		return core.Session{}, core.User{}, ErrInvalidSession
	}
	user, err := s.users.Get(ctx, sess.UserID)
	if errors.Is(err, core.ErrNotFound) {
		s.dropSession(ctx, hash)
		return core.Session{}, core.User{}, ErrInvalidSession
	}
	if err != nil {
		return core.Session{}, core.User{}, fmt.Errorf("auth: get session user: %w", err)
	}
	if now.Sub(sess.LastSeenAt) >= sessionTouchInterval {
		// Best effort: failing to renew must not log the user out.
		if err := s.sessions.Touch(ctx, hash, now); err != nil {
			s.log.Warn("session touch failed", "err", err)
		} else {
			sess.LastSeenAt = now
		}
	}
	return sess, user, nil
}

// Logout ends the session. An unknown token is not an error.
func (s *SessionService) Logout(ctx context.Context, token string) error {
	if kind, ok := KindOf(token); !ok || kind != KindSession {
		return nil
	}
	err := s.sessions.Delete(ctx, HashToken(token))
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("auth: logout: %w", err)
	}
	return nil
}

// Cleanup deletes every expired session and reports how many it removed.
func (s *SessionService) Cleanup(ctx context.Context) (int, error) {
	now := s.now()
	n, err := s.sessions.DeleteExpired(ctx, now.Add(-SessionAbsoluteTimeout), now.Add(-SessionIdleTimeout))
	if err != nil {
		return 0, fmt.Errorf("auth: cleanup sessions: %w", err)
	}
	return n, nil
}

// RunCleanup calls Cleanup every interval until ctx ends.
func (s *SessionService) RunCleanup(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if n, err := s.Cleanup(ctx); err != nil {
				s.log.Warn("session cleanup failed", "err", err)
			} else if n > 0 {
				s.log.Debug("expired sessions removed", "count", n)
			}
		}
	}
}

func (s *SessionService) dropSession(ctx context.Context, h core.TokenHash) {
	if err := s.sessions.Delete(ctx, h); err != nil && !errors.Is(err, core.ErrNotFound) {
		s.log.Warn("delete dead session failed", "err", err)
	}
}
