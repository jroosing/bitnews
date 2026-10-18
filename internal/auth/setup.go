package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Setup is the first-run service. While no User exists, the instance is in
// setup mode: Complete creates the admin, but only for a caller that knows
// the one-time setup token. The token lives in memory only and is logged once
// at startup, so someone else on the LAN cannot claim the instance first.
type Setup struct {
	users core.UserRepo
	log   *slog.Logger
	now   func() time.Time

	required atomic.Bool // lock-free read for the gate

	mu    sync.Mutex // serializes Complete; guards token
	token []byte     // nil once setup is closed
}

// SetupOption configures NewSetup.
type SetupOption func(*Setup)

// WithClock sets the time source for the admin's CreatedAt.
func WithClock(now func() time.Time) SetupOption {
	return func(s *Setup) { s.now = now }
}

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSetup checks whether any User exists. If none does, it generates the
// setup token and logs it once at WARN level. That log line is the only place
// a secret is ever logged on purpose.
//
// Call BootstrapAdminFromFile first, so a bootstrapped admin closes setup
// before the token is generated.
func NewSetup(ctx context.Context, users core.UserRepo, logger *slog.Logger, opts ...SetupOption) (*Setup, error) {
	s := &Setup{users: users, log: logger, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	n, err := users.Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: count users: %w", err)
	}
	if n > 0 {
		return s, nil
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error.
	s.token = []byte(tokenEncoding.EncodeToString(raw))
	s.required.Store(true)
	logger.Warn("first-run setup required: no admin exists yet. Use this one-time setup token to create the admin; it is kept in memory only and changes on every restart",
		"setup_token", string(s.token))
	return s, nil
}

// Required reports whether first-run setup is still pending. It is a single
// atomic load, so the gate can call it on every request.
func (s *Setup) Required() bool { return s.required.Load() }

// Setup errors. Callers branch on them with errors.Is.
var (
	// ErrSetupClosed means a User already exists, so setup is over.
	ErrSetupClosed = errors.New("auth: setup is closed")
	// ErrBadSetupToken means the setup token is missing or wrong.
	ErrBadSetupToken = errors.New("auth: bad setup token")
	// ErrInvalidUsername means the username breaks the username rules.
	ErrInvalidUsername = errors.New("auth: invalid username")
)

// MaxUsernameBytes caps the username length.
const MaxUsernameBytes = 64

// ValidateUsername rejects empty, over-long, non-UTF-8 and control-character
// usernames, and names with leading or trailing whitespace. Usernames are
// case-sensitive.
func ValidateUsername(name string) error {
	if name == "" || len(name) > MaxUsernameBytes || !utf8.ValidString(name) ||
		strings.TrimSpace(name) != name ||
		strings.ContainsFunc(name, unicode.IsControl) {
		return ErrInvalidUsername
	}
	return nil
}

// Complete creates the admin User and closes setup. It checks, in order:
// setup is still open (ErrSetupClosed), the token (ErrBadSetupToken, compared
// in constant time), then the username and password rules. Calls are
// serialized, and the User count is checked again inside the critical
// section, so concurrent calls produce exactly one admin.
func (s *Setup) Complete(ctx context.Context, token, username string, password core.Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token == nil {
		return ErrSetupClosed
	}
	if subtle.ConstantTimeCompare([]byte(token), s.token) != 1 {
		return ErrBadSetupToken
	}
	if err := ValidateUsername(username); err != nil {
		return err
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}
	// Another path (the password file, another process) may have created a
	// User since startup.
	n, err := s.users.Count(ctx)
	if err != nil {
		return fmt.Errorf("auth: count users: %w", err)
	}
	if n > 0 {
		s.close()
		return ErrSetupClosed
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	err = s.users.Create(ctx, core.User{
		ID:           core.NewID(),
		Username:     username,
		PasswordHash: hash,
		CreatedAt:    s.now().UTC(),
	})
	if errors.Is(err, core.ErrConflict) {
		s.close()
		return ErrSetupClosed
	}
	if err != nil {
		return fmt.Errorf("auth: create admin: %w", err)
	}
	s.close()
	s.log.Info("first-run setup complete: admin account created", "username", username)
	return nil
}

// close forgets the token. The caller holds s.mu.
func (s *Setup) close() {
	s.token = nil
	s.required.Store(false)
}

func nowUTC() time.Time { return time.Now().UTC() }
