package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Adapter credential errors. Use errors.Is.
var (
	// ErrInvalidAdapterCredential means the key (or the username and key) is
	// unknown, revoked, malformed or of the wrong kind.
	ErrInvalidAdapterCredential = errors.New("auth: invalid adapter credential")
	// ErrInvalidAdapterKind means the kind is not adapter_sab or adapter_qbit.
	ErrInvalidAdapterKind = errors.New("auth: invalid adapter credential kind")
	// ErrInvalidAdapterName means the name is blank, too long or has control
	// characters.
	ErrInvalidAdapterName = errors.New("auth: invalid adapter credential name")
	// ErrInvalidAdapterUsername means the username is not [a-z0-9_-]{1,32}.
	ErrInvalidAdapterUsername = errors.New("auth: invalid adapter credential username")
	// ErrAdapterUsernameTaken means another credential of the kind has the
	// username.
	ErrAdapterUsernameTaken = errors.New("auth: adapter credential username taken")
)

const (
	maxAdapterNameRunes     = 100
	maxAdapterUsernameBytes = 32
	// adapterTouchInterval limits how often last_used_at is written.
	adapterTouchInterval = time.Minute
	defaultAdapterUser   = "media-manager"
)

// AdapterCredentialInfo is what the API shows of an adapter credential. It
// has no hash and never the plaintext, which only Create returns, once.
type AdapterCredentialInfo struct {
	ID   core.ID   `json:"id"`
	Kind TokenKind `json:"kind"`
	Name string    `json:"name"`
	// Username is set for adapter_qbit credentials only.
	Username  string    `json:"username,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt is null until the credential is first used.
	LastUsedAt *time.Time `json:"last_used_at"`
}

func adapterInfoOf(c core.AdapterCredential) AdapterCredentialInfo {
	i := AdapterCredentialInfo{
		ID: c.ID, Kind: TokenKind(c.Kind), Name: c.Name, Username: c.Username, CreatedAt: c.CreatedAt,
	}
	if !c.LastUsedAt.IsZero() {
		t := c.LastUsedAt
		i.LastUsedAt = &t
	}
	return i
}

// AdapterCredentialService manages the credentials of the media-manager
// adapters (/sabnzbd/api, and the qBittorrent adapter) and authenticates
// requests with them. There is one credential per media manager, so each
// can be revoked on its own.
//
// A credential is a 256-bit token of kind adapter_sab ("bns_") or
// adapter_qbit ("bnq_"); only its SHA-256 hash is stored. It is valid on the
// routes of its own adapter only: the own API never accepts it (APIKeyService
// checks the kind), and the adapters accept no other kind. Unlike the UI, the
// adapters never honour the disable-auth or LAN bypass settings.
//
// Credentials are created, listed and revoked through the own API by a UI
// principal (see UIPrincipalOnly).
type AdapterCredentialService struct {
	repo core.AdapterCredentialRepo
	log  *slog.Logger
	now  func() time.Time
}

// AdapterOption configures NewAdapterCredentialService.
type AdapterOption func(*AdapterCredentialService)

// WithAdapterClock sets the time source.
func WithAdapterClock(now func() time.Time) AdapterOption {
	return func(s *AdapterCredentialService) { s.now = now }
}

// WithAdapterLogger sets the logger. Keys are never logged.
func WithAdapterLogger(l *slog.Logger) AdapterOption {
	return func(s *AdapterCredentialService) { s.log = l }
}

// NewAdapterCredentialService returns an AdapterCredentialService on repo.
func NewAdapterCredentialService(repo core.AdapterCredentialRepo, opts ...AdapterOption) *AdapterCredentialService {
	s := &AdapterCredentialService{repo: repo, log: slog.Default(), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// IsAdapterKind reports whether k is KindAdapterSAB or KindAdapterQBit.
func IsAdapterKind(k TokenKind) bool { return k == KindAdapterSAB || k == KindAdapterQBit }

// Create makes a credential of the adapter kind. The plaintext is returned
// here and nowhere else; show it to the admin once.
//
// username matters for KindAdapterQBit only (a SAB credential has none and
// the argument is ignored). It must match [a-z0-9_-]{1,32} and be unused by
// another qBit credential (ErrAdapterUsernameTaken); empty derives one from
// name ("My Radarr" becomes "my-radarr"). For qBit the plaintext is the
// password to configure next to the username.
func (s *AdapterCredentialService) Create(ctx context.Context, kind TokenKind, name, username string) (plaintext string, info AdapterCredentialInfo, err error) {
	if !IsAdapterKind(kind) {
		return "", AdapterCredentialInfo{}, ErrInvalidAdapterKind
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > maxAdapterNameRunes ||
		!utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", AdapterCredentialInfo{}, ErrInvalidAdapterName
	}
	if kind == KindAdapterQBit {
		if username == "" {
			username = deriveUsername(name)
		}
		if !validAdapterUsername(username) {
			return "", AdapterCredentialInfo{}, ErrInvalidAdapterUsername
		}
	} else {
		username = ""
	}
	plaintext, hash, err := NewKindToken(kind)
	if err != nil {
		return "", AdapterCredentialInfo{}, err
	}
	c := core.AdapterCredential{
		ID: core.NewID(), Kind: core.AdapterKind(kind), Name: name, Username: username,
		SecretHash: hash, CreatedAt: s.now(),
	}
	if err := s.repo.Create(ctx, c); err != nil {
		if errors.Is(err, core.ErrConflict) && username != "" {
			return "", AdapterCredentialInfo{}, ErrAdapterUsernameTaken
		}
		return "", AdapterCredentialInfo{}, fmt.Errorf("auth: create adapter credential: %w", err)
	}
	return plaintext, adapterInfoOf(c), nil
}

// deriveUsername makes a valid username from a display name: lowercase,
// every run of other characters becomes one '-', at most 32 characters.
func deriveUsername(name string) string {
	var b strings.Builder
	dash := true // no leading dash
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	u := strings.Trim(b.String(), "-")
	if len(u) > maxAdapterUsernameBytes {
		u = strings.TrimRight(u[:maxAdapterUsernameBytes], "-")
	}
	if u == "" {
		return defaultAdapterUser
	}
	return u
}

func validAdapterUsername(u string) bool {
	if len(u) < 1 || len(u) > maxAdapterUsernameBytes {
		return false
	}
	for i := range len(u) {
		switch c := u[i]; {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// List returns every credential without hashes, oldest first.
func (s *AdapterCredentialService) List(ctx context.Context) ([]AdapterCredentialInfo, error) {
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list adapter credentials: %w", err)
	}
	out := make([]AdapterCredentialInfo, 0, len(rows))
	for _, c := range rows {
		out = append(out, adapterInfoOf(c))
	}
	return out, nil
}

// Revoke deletes the credential; it stops working with the next request. An
// unknown ID is core.ErrNotFound.
func (s *AdapterCredentialService) Revoke(ctx context.Context, id core.ID) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("auth: revoke adapter credential: %w", err)
	}
	return nil
}

// Authenticate resolves a plaintext key for the adapter of the given kind.
// Like APIKeyService.Authenticate it checks the key's shape and kind before
// hashing, looks it up by hash, compares the stored hash again in constant
// time and writes last_used_at at most once a minute. A key of any other
// kind (own-API key, session token, the other adapter's key) is
// ErrInvalidAdapterCredential, as is a revoked one. The error never contains
// the key.
func (s *AdapterCredentialService) Authenticate(ctx context.Context, kind TokenKind, plaintext string) (AdapterCredentialInfo, error) {
	if !IsAdapterKind(kind) {
		return AdapterCredentialInfo{}, ErrInvalidAdapterKind
	}
	if got, ok := KindOf(plaintext); !ok || got != kind {
		return AdapterCredentialInfo{}, ErrInvalidAdapterCredential
	}
	return s.lookup(ctx, kind, plaintext, func(h core.TokenHash) (core.AdapterCredential, error) {
		return s.repo.GetByHash(ctx, h)
	})
}

// AuthenticateQBit resolves the username and password of a qBittorrent
// adapter login. The password is the credential's generated key. An unknown
// username and a wrong password give the same error.
func (s *AdapterCredentialService) AuthenticateQBit(ctx context.Context, username, password string) (AdapterCredentialInfo, error) {
	if got, ok := KindOf(password); !ok || got != KindAdapterQBit || !validAdapterUsername(username) {
		return AdapterCredentialInfo{}, ErrInvalidAdapterCredential
	}
	return s.lookup(ctx, KindAdapterQBit, password, func(h core.TokenHash) (core.AdapterCredential, error) {
		c, err := s.repo.GetByUsername(ctx, core.AdapterQBit, username)
		if err != nil {
			return core.AdapterCredential{}, err
		}
		if !TokenHashEqual(c.SecretHash, h) {
			return core.AdapterCredential{}, core.ErrNotFound
		}
		return c, nil
	})
}

func (s *AdapterCredentialService) lookup(ctx context.Context, kind TokenKind, plaintext string, get func(core.TokenHash) (core.AdapterCredential, error)) (AdapterCredentialInfo, error) {
	hash := HashToken(plaintext)
	c, err := get(hash)
	if errors.Is(err, core.ErrNotFound) {
		return AdapterCredentialInfo{}, ErrInvalidAdapterCredential
	}
	if err != nil {
		return AdapterCredentialInfo{}, fmt.Errorf("auth: look up adapter credential: %w", err)
	}
	if c.Kind != core.AdapterKind(kind) || !TokenHashEqual(c.SecretHash, hash) {
		return AdapterCredentialInfo{}, ErrInvalidAdapterCredential
	}
	if now := s.now(); c.LastUsedAt.IsZero() || now.Sub(c.LastUsedAt) >= adapterTouchInterval {
		switch err := s.repo.TouchLastUsed(ctx, c.ID, now); {
		case errors.Is(err, core.ErrNotFound):
			return AdapterCredentialInfo{}, ErrInvalidAdapterCredential // revoked while we looked
		case err != nil:
			s.log.Warn("adapter credential last_used update failed", "credential_id", c.ID, "err", err)
		default:
			c.LastUsedAt = now
		}
	}
	return adapterInfoOf(c), nil
}
