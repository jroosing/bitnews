package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// SessionCookieName is the cookie that carries the session token.
const SessionCookieName = "bitnews_session"

// UIAuth authenticates requests to the UI routes (internal/web).
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
}

// NewUIAuth returns the UI authentication middleware.
func NewUIAuth(sessions *SessionService, settings func() core.Settings) (*UIAuth, error) {
	return &UIAuth{sessions: sessions, settings: settings}, nil
}

// Failure answers a request that authentication or the CSRF check refused:
// status, a machine-readable code and a message. The default (nil) writes
// the API's JSON error; the web UI renders a page, a redirect or a toast.
type Failure func(w http.ResponseWriter, r *http.Request, status int, code, message string)

func orJSON(f Failure) Failure {
	if f != nil {
		return f
	}
	return func(w http.ResponseWriter, _ *http.Request, status int, code, message string) {
		writeError(w, status, code, message)
	}
}

// refusal is why a request gets no Principal.
type refusal struct {
	status        int
	code, message string
}

// Middleware wraps next with the resolution described on UIAuth; fail
// answers a refused request (nil: JSON). Put CSRF inside it.
func (a *UIAuth) Middleware(fail Failure) func(http.Handler) http.Handler {
	fail = orJSON(fail)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ref := a.resolve(r)
			if ref != nil {
				fail(w, r, ref.status, ref.code, ref.message)
				return
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
		})
	}
}

// Principal resolves the Principal of a request the way Middleware does,
// without answering it; false means the request would be refused.
func (a *UIAuth) Principal(r *http.Request) (Principal, bool) {
	p, ref := a.resolve(r)
	return p, ref == nil
}

func (a *UIAuth) resolve(r *http.Request) (Principal, *refusal) {
	s := a.settings()
	bypassOn := s.Auth.DisableAuth || len(s.Auth.LANBypassCIDRs) > 0
	if bypassOn && !hostInList(r.Host, s.Auth.AllowedHosts) {
		return Principal{}, &refusal{http.StatusMisdirectedRequest, "misdirected_request", "this Host is not in the allowed hosts list"}
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		_, user, err := a.sessions.Authenticate(r.Context(), c.Value)
		switch {
		case err == nil:
			return Principal{Kind: PrincipalSession, UserID: user.ID, Username: user.Username}, nil
		case !errors.Is(err, ErrInvalidSession):
			a.sessions.log.Error("session lookup failed", "err", err)
			return Principal{}, &refusal{http.StatusInternalServerError, "internal", "internal error"}
		}
		// A stale cookie falls through: the bypass settings may still apply.
	}
	kind, ok := bypassKind(s.Auth, r)
	if !ok {
		return Principal{}, &refusal{http.StatusUnauthorized, "unauthorized", "authentication required"}
	}
	return Principal{Kind: kind}, nil
}

// bypassKind decides whether the settings let an unauthenticated request in.
func bypassKind(s core.Auth, r *http.Request) (PrincipalKind, bool) {
	if s.DisableAuth {
		return PrincipalAuthDisabled, true
	}
	if len(s.LANBypassCIDRs) == 0 {
		return "", false
	}
	ip := ClientIPFrom(r, s)
	if ip.IsValid() && contains(ParsePrefixes(s.LANBypassCIDRs), ip) {
		return PrincipalLANBypass, true
	}
	return "", false
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
