package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// SessionCookieName is the cookie that carries the session token.
const SessionCookieName = "bitnews_session"

// UIAuth authenticates requests to the UI routes: the HTML pages and the
// own-API calls the UI makes with its session cookie.
//
// A request resolves to a Principal in this order:
//
//  1. a valid session cookie: PrincipalSession;
//  2. else, if the disable_auth setting is on: PrincipalAuthDisabled;
//  3. else, if the client IP is inside lan_bypass_cidrs: PrincipalLANBypass;
//  4. else 401 unauthorized.
//
// While disable_auth or any LAN bypass CIDR is configured, a request whose
// Host is not in allowed_hosts gets 421, even with a valid session. That
// stops DNS rebinding, where a hostile page makes the browser reach the LAN
// instance under the attacker's own name.
//
// DISABLE AUTH AND LAN BYPASS ARE UI-ONLY. Mount UIAuth on UI routes and
// nowhere else. Routes that authenticate with the app key (KeyOr),
// the adapters (/sabnzbd/api, qBit) and first-run setup must never sit
// behind it, so those can never be reached without their credential. The
// settings are read through a function on every request, so a change applies
// at once.
type UIAuth struct {
	sessions *SessionService
	settings func() core.Settings
	// anonCSRF is the CSRF token for requests without a session. With no
	// cookie there is nothing to bind it to, so it is one random value per
	// process, readable only from same-origin session info responses.
	anonCSRF string
}

// NewUIAuth returns the UI authentication middleware.
func NewUIAuth(sessions *SessionService, settings func() core.Settings) (*UIAuth, error) {
	secret, _, err := NewToken()
	if err != nil {
		return nil, err
	}
	return &UIAuth{sessions: sessions, settings: settings, anonCSRF: CSRFToken(secret)}, nil
}

// Middleware wraps next with the resolution described on UIAuth. Put CSRF
// inside it.
func (a *UIAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := a.settings()
		bypassOn := s.Auth.DisableAuth || len(s.Auth.LANBypassCIDRs) > 0
		if bypassOn && !hostInList(r.Host, s.Auth.AllowedHosts) {
			writeError(w, http.StatusMisdirectedRequest, "misdirected_request",
				"this Host is not in the allowed hosts list")
			return
		}

		if c, err := r.Cookie(SessionCookieName); err == nil {
			_, user, err := a.sessions.Authenticate(r.Context(), c.Value)
			switch {
			case err == nil:
				p := Principal{Kind: PrincipalSession, UserID: user.ID, Username: user.Username, csrf: CSRFToken(c.Value)}
				next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
				return
			case !errors.Is(err, ErrInvalidSession):
				a.sessions.log.Error("session lookup failed", "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			// A stale cookie falls through: the bypass settings may still apply.
		}

		kind, ok := bypassKind(s.Auth, r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		p := Principal{Kind: kind, csrf: a.anonCSRF}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

// bypassKind decides whether the settings let an unauthenticated request in.
func bypassKind(s core.Auth, r *http.Request) (PrincipalKind, bool) {
	if s.DisableAuth {
		return PrincipalAuthDisabled, true
	}
	if len(s.LANBypassCIDRs) == 0 {
		return "", false
	}
	ip := ClientIP(r, ParsePrefixes(s.TrustedProxies))
	if ip.IsValid() && contains(ParsePrefixes(s.LANBypassCIDRs), ip) {
		return PrincipalLANBypass, true
	}
	return "", false
}

// SessionInfo serves GET /api/v1/auth/session behind Middleware. The body is
//
//	{"auth_mode": "session"|"bypass"|"disabled",
//	 "csrf_token": "...",
//	 "user": {"id": "...", "username": "..."} | null}
//
// The UI sends csrf_token as X-CSRF-Token on state-changing requests and
// shows a permanent warning banner unless auth_mode is "session". Other
// principals (API keys) get 401: this is a UI route.
func (a *UIAuth) SessionInfo() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok || !p.ambient() {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		type userInfo struct {
			ID       core.ID `json:"id"`
			Username string  `json:"username"`
		}
		body := struct {
			AuthMode  string    `json:"auth_mode"`
			CSRFToken string    `json:"csrf_token"`
			User      *userInfo `json:"user"`
		}{AuthMode: p.AuthMode(), CSRFToken: p.csrf}
		if p.Kind == PrincipalSession {
			body.User = &userInfo{ID: p.UserID, Username: p.Username}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// SetSessionCookie sends the session cookie: HttpOnly, SameSite=Strict,
// Path=/, and Secure when the request arrived over HTTPS (directly, or
// through a trusted proxy that says so in X-Forwarded-Proto). expires is the
// session's absolute expiry; the server enforces the idle limit itself.
func (a *UIAuth) SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie tells the browser to drop the session cookie.
func (a *UIAuth) ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// isHTTPS reports whether the client reached us over HTTPS.
func (a *UIAuth) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return false
	}
	peer := peerAddr(r.RemoteAddr)
	return peer.IsValid() && contains(ParsePrefixes(a.settings().Auth.TrustedProxies), peer)
}
