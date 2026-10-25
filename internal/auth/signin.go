package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/jroosing/bitnews/internal/core"
)

// ErrSessionStart wraps a failure to use a session right after Login made it.
// The credentials were good; the session could not be read back.
var ErrSessionStart = errors.New("auth: new session unusable")

// ClientIPFrom returns the client address of r under the trusted proxies of
// the given auth settings. It is ClientIP with the settings parsed.
func ClientIPFrom(r *http.Request, s core.Auth) netip.Addr {
	return ClientIP(r, ParsePrefixes(s.TrustedProxies))
}

// SignIn checks the credentials through SessionService.Login (rate limited
// per client IP), opens the session and sets its cookie, which expires with
// the session's absolute timeout. Login errors come back unchanged
// (*RateLimitError, ErrInvalidCredentials, ...); a session that cannot be
// read back comes back wrapped in ErrSessionStart. On error no cookie is set.
func (a *UIAuth) SignIn(w http.ResponseWriter, r *http.Request, username string, password core.Secret) (token string, user core.User, err error) {
	token, err = a.sessions.Login(r.Context(), username, password, ClientIPFrom(r, a.settings().Auth))
	if err != nil {
		return "", core.User{}, err
	}
	sess, user, err := a.sessions.Authenticate(r.Context(), token)
	if err != nil {
		return "", core.User{}, fmt.Errorf("%w: %w", ErrSessionStart, err)
	}
	a.SetSessionCookie(w, r, token, sess.CreatedAt.Add(SessionAbsoluteTimeout))
	return token, user, nil
}
