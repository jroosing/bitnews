package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Own-API key errors. Use errors.Is.
var (
	// ErrInvalidAPIKey means the key is unknown, revoked or malformed.
	ErrInvalidAPIKey = errors.New("auth: invalid API key")
	// ErrInvalidAPIKeyName means the name is blank, too long or has
	// control characters.
	ErrInvalidAPIKeyName = errors.New("auth: invalid API key name")
	// ErrInvalidAPIKeyScope means the scope is neither read nor write.
	ErrInvalidAPIKeyScope = errors.New("auth: invalid API key scope")
)

const (
	maxAPIKeyNameRunes = 100
	// apiKeyTouchInterval limits how often last_used_at is written.
	apiKeyTouchInterval = time.Minute
)

// APIKeyInfo is what the API shows of a key. It has no hash and never the
// plaintext, which only Create returns, once.
type APIKeyInfo struct {
	ID        core.ID          `json:"id"`
	Name      string           `json:"name"`
	Scope     core.APIKeyScope `json:"scope"`
	CreatedAt time.Time        `json:"created_at"`
	// LastUsedAt is null until the key is first used.
	LastUsedAt *time.Time `json:"last_used_at"`
}

func infoOf(k core.APIKey) APIKeyInfo {
	i := APIKeyInfo{ID: k.ID, Name: k.Name, Scope: k.Scope, CreatedAt: k.CreatedAt}
	if !k.LastUsedAt.IsZero() {
		t := k.LastUsedAt
		i.LastUsedAt = &t
	}
	return i
}

// APIKeyService manages the own-API keys and authenticates requests with
// them. Keys are 256 random bits shown as "bn_" + base32; only the SHA-256
// hash is stored. Keys are created, listed and revoked by a logged-in admin
// only (guard those routes with SessionOnly), and never take effect on UI or
// adapter routes.
type APIKeyService struct {
	repo core.APIKeyRepo
	log  *slog.Logger
	now  func() time.Time
}

// APIKeyOption configures NewAPIKeyService.
type APIKeyOption func(*APIKeyService)

// WithAPIKeyClock sets the time source.
func WithAPIKeyClock(now func() time.Time) APIKeyOption {
	return func(s *APIKeyService) { s.now = now }
}

// WithAPIKeyLogger sets the logger. Keys are never logged.
func WithAPIKeyLogger(l *slog.Logger) APIKeyOption {
	return func(s *APIKeyService) { s.log = l }
}

// NewAPIKeyService returns an APIKeyService on repo.
func NewAPIKeyService(repo core.APIKeyRepo, opts ...APIKeyOption) *APIKeyService {
	s := &APIKeyService{repo: repo, log: slog.Default(), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ValidScope reports whether s is a known API key scope.
func ValidScope(s core.APIKeyScope) bool {
	return s == core.APIKeyScopeRead || s == core.APIKeyScopeWrite
}

// Create makes a key. The plaintext is returned here and nowhere else; show it
// to the admin once.
func (s *APIKeyService) Create(ctx context.Context, name string, scope core.APIKeyScope) (plaintext string, info APIKeyInfo, err error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > maxAPIKeyNameRunes ||
		!utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", APIKeyInfo{}, ErrInvalidAPIKeyName
	}
	if !ValidScope(scope) {
		return "", APIKeyInfo{}, ErrInvalidAPIKeyScope
	}
	plaintext, hash, err := NewKindToken(KindAPI)
	if err != nil {
		return "", APIKeyInfo{}, err
	}
	k := core.APIKey{ID: core.NewID(), Name: name, Scope: scope, Hash: hash, CreatedAt: s.now()}
	if err := s.repo.Create(ctx, k); err != nil {
		return "", APIKeyInfo{}, fmt.Errorf("auth: create API key: %w", err)
	}
	return plaintext, infoOf(k), nil
}

// List returns every key without hashes.
func (s *APIKeyService) List(ctx context.Context) ([]APIKeyInfo, error) {
	keys, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list API keys: %w", err)
	}
	out := make([]APIKeyInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, infoOf(k))
	}
	return out, nil
}

// Revoke deletes the key; it stops working with the next request. An unknown
// ID is core.ErrNotFound.
func (s *APIKeyService) Revoke(ctx context.Context, id core.ID) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("auth: revoke API key: %w", err)
	}
	return nil
}

// Authenticate resolves a plaintext key. The key is checked for shape first
// (so garbage is never hashed or looked up), looked up by its SHA-256 hash,
// and the stored hash is compared again in constant time. last_used_at is
// written at most once a minute. A bad key is ErrInvalidAPIKey; the error
// never contains the key.
func (s *APIKeyService) Authenticate(ctx context.Context, plaintext string) (APIKeyInfo, error) {
	if kind, ok := KindOf(plaintext); !ok || kind != KindAPI {
		return APIKeyInfo{}, ErrInvalidAPIKey
	}
	hash := HashToken(plaintext)
	k, err := s.repo.GetByHash(ctx, hash)
	if errors.Is(err, core.ErrNotFound) {
		return APIKeyInfo{}, ErrInvalidAPIKey
	}
	if err != nil {
		return APIKeyInfo{}, fmt.Errorf("auth: look up API key: %w", err)
	}
	if !TokenHashEqual(k.Hash, hash) {
		return APIKeyInfo{}, ErrInvalidAPIKey
	}
	if now := s.now(); k.LastUsedAt.IsZero() || now.Sub(k.LastUsedAt) >= apiKeyTouchInterval {
		switch err := s.repo.TouchLastUsed(ctx, k.ID, now); {
		case errors.Is(err, core.ErrNotFound):
			return APIKeyInfo{}, ErrInvalidAPIKey // revoked while we looked
		case err != nil:
			s.log.Warn("api key last_used update failed", "key_id", k.ID, "err", err)
		default:
			k.LastUsedAt = now
		}
	}
	return infoOf(k), nil
}

// credential extracts a key from "Authorization: Bearer <key>" or
// "X-Api-Key: <key>". Bearer wins when both are present. present reports
// whether the request carries a key at all, valid or not.
func credential(r *http.Request) (key string, present bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		if scheme, rest, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(rest), true
		}
	}
	if k := strings.TrimSpace(r.Header.Get("X-Api-Key")); k != "" {
		return k, true
	}
	return "", false
}

// RequireScope returns middleware for routes that take API keys only. It
// reads the key from Authorization: Bearer or X-Api-Key, answers 401 if it is
// missing or invalid, and 403 insufficient_scope if the key may not do this.
//
// A read key may use GET, HEAD and OPTIONS only; a write key may use every
// method. scope is what the route needs: a route that needs write refuses a
// read key even for GET. On success the request carries a PrincipalAPIKey.
//
// This middleware never looks at the disable-auth or LAN bypass settings: a
// key route is always authenticated.
func (s *APIKeyService) RequireScope(scope core.APIKeyScope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, _ := credential(r)
			s.serveWithKey(w, r, next, key, scope)
		})
	}
}

// KeyOr is RequireScope for routes that both scripts (keys) and the UI
// (cookie) use, such as GET /api/v1/downloads. A request that carries a key
// is judged as a key request and never falls back, so a wrong key is not
// rescued by LAN bypass. A request without one goes through fallback, which
// is the UI chain (UIAuth.Middleware, then CSRF).
func (s *APIKeyService) KeyOr(scope core.APIKeyScope, fallback func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		ui := fallback(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, present := credential(r)
			if !present {
				ui.ServeHTTP(w, r)
				return
			}
			s.serveWithKey(w, r, next, key, scope)
		})
	}
}

func (s *APIKeyService) serveWithKey(w http.ResponseWriter, r *http.Request, next http.Handler, key string, scope core.APIKeyScope) {
	info, err := s.Authenticate(r.Context(), key)
	switch {
	case errors.Is(err, ErrInvalidAPIKey):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
		return
	case err != nil:
		s.log.Error("api key authentication failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !scopeAllows(info.Scope, scope, r.Method) {
		writeError(w, http.StatusForbidden, "insufficient_scope", "this API key's scope does not allow this request")
		return
	}
	p := Principal{Kind: PrincipalAPIKey, Scope: info.Scope, KeyID: info.ID}
	next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
}

// scopeAllows applies the scope rules: the key's scope must cover what the
// route needs, and a read key is limited to safe methods whatever the route
// says.
func scopeAllows(have, need core.APIKeyScope, method string) bool {
	switch have {
	case core.APIKeyScopeWrite:
		return true
	case core.APIKeyScopeRead:
		return need == core.APIKeyScopeRead && safeMethod(method)
	}
	return false
}

// SessionOnly guards the routes only a logged-in admin may use, such as
// /api/v1/api-keys. It lets through PrincipalSession and nothing else: API
// keys cannot manage API keys, and neither can disable-auth or LAN bypass
// requests, which would otherwise let anyone on the LAN mint a lasting
// credential. Mount it inside the authentication middleware. 401 without any
// principal, 403 session_required otherwise.
func SessionOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		switch {
		case !ok:
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		case p.Kind != PrincipalSession:
			writeError(w, http.StatusForbidden, "session_required", "log in with a user session to do this")
		default:
			next.ServeHTTP(w, r)
		}
	})
}
