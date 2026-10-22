package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// QBitUsername is the fixed username of the qBittorrent adapter login; the
// password is the app key.
const QBitUsername = "bitnews"

// AppKeyService holds the app key: the one secret that authenticates every
// client (SABnzbd's apikey, the qBittorrent password and the own API's
// Authorization: Bearer, see ADR-0006). It has full access, with no scopes.
//
// The key is generated on first start, stored sealed (core.AppKeyRepo), and
// decrypted once at start. Requests are compared against the copy in memory in
// constant time, with no database access. Refresh replaces the key for every
// surface at once.
type AppKeyService struct {
	repo core.AppKeyRepo
	log  *slog.Logger

	mu  sync.Mutex // serialises Refresh
	cur atomic.Pointer[appKey]
}

// appKey is the key in memory with its SHA-256, so that Authenticate compares
// two fixed-length values whatever the length of the candidate.
type appKey struct {
	secret core.Secret
	hash   core.TokenHash
	gen    uint64
}

// AppKeyOption configures NewAppKeyService.
type AppKeyOption func(*AppKeyService)

// WithAppKeyLogger sets the logger. The key is never logged.
func WithAppKeyLogger(l *slog.Logger) AppKeyOption {
	return func(s *AppKeyService) { s.log = l }
}

// NewAppKeyService loads the key from repo, or makes and stores one when there
// is none (the first start). A stored key that cannot be decrypted, because
// bitnews.key was lost, is replaced by a new one; every client then needs the
// new key.
func NewAppKeyService(ctx context.Context, repo core.AppKeyRepo, opts ...AppKeyOption) (*AppKeyService, error) {
	s := &AppKeyService{repo: repo, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	key, err := repo.Get(ctx)
	switch {
	case err == nil:
		s.cur.Store(newAppKey(key, 1))
		return s, nil
	case errors.Is(err, core.ErrNotFound):
	case errors.Is(err, core.ErrAppKeyUnreadable):
		s.log.Warn("auth: the stored API key is unreadable (was the master key replaced?); making a new one")
	default:
		return nil, fmt.Errorf("auth: load app key: %w", err)
	}
	fresh, _, err := NewKindToken(KindAppKey)
	if err != nil {
		return nil, err
	}
	if err := repo.Set(ctx, core.Secret(fresh)); err != nil {
		return nil, fmt.Errorf("auth: store app key: %w", err)
	}
	s.cur.Store(newAppKey(core.Secret(fresh), 1))
	return s, nil
}

func newAppKey(k core.Secret, gen uint64) *appKey {
	return &appKey{secret: k, hash: HashToken(k.Reveal()), gen: gen}
}

// Current returns the key, for the Security page. It is a core.Secret, so it
// stays out of logs and JSON unless someone calls Reveal.
func (s *AppKeyService) Current() core.Secret { return s.cur.Load().secret }

// Generation counts the keys this service has had. Sessions that were opened
// with a key (the qBittorrent SID) record it and end when it changes.
func (s *AppKeyService) Generation() uint64 { return s.cur.Load().gen }

// Refresh makes a new key, stores it and swaps it in. The old key stops
// working with the next request on every surface. If storing fails, the old
// key stays.
func (s *AppKeyService) Refresh(ctx context.Context) (core.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh, _, err := NewKindToken(KindAppKey)
	if err != nil {
		return "", err
	}
	if err := s.repo.Set(ctx, core.Secret(fresh)); err != nil {
		return "", fmt.Errorf("auth: store app key: %w", err)
	}
	next := newAppKey(core.Secret(fresh), s.cur.Load().gen+1)
	s.cur.Store(next)
	s.log.Info("auth: the API key was refreshed")
	return next.secret, nil
}

// Authenticate reports whether candidate is the current key. The comparison
// is constant time, on the SHA-256 of both values.
func (s *AppKeyService) Authenticate(candidate string) bool {
	return TokenHashEqual(s.cur.Load().hash, HashToken(candidate))
}

// AuthenticateQBit checks a qBittorrent login: the fixed username and the key.
func (s *AppKeyService) AuthenticateQBit(username, password string) bool {
	// Both checks always run, so timing says nothing about which failed.
	okKey := s.Authenticate(password)
	okUser := TokenHashEqual(HashToken(username), HashToken(QBitUsername))
	return okKey && okUser
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

// KeyOr returns middleware for the own API's routes, which both scripts (the
// key) and the UI (cookie) use. A request that carries a key is judged as a
// key request and never falls back, so a wrong key is not rescued by LAN
// bypass; a good one is let through with a PrincipalAPIKey. A request without
// a key goes through fallback, which is the UI chain (UIAuth.Middleware, then
// CSRF).
//
// The key routes never look at the disable-auth or LAN bypass settings: a
// request with a key is always authenticated.
func (s *AppKeyService) KeyOr(fallback func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		ui := fallback(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, present := credential(r)
			switch {
			case !present:
				ui.ServeHTTP(w, r)
			case !s.Authenticate(key):
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
			default:
				next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), Principal{Kind: PrincipalAPIKey})))
			}
		})
	}
}
