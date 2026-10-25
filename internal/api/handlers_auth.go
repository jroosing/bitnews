package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// login serves POST /api/v1/auth/login. The Origin check runs in front of it
// (the CSRF middleware sees a request without a principal), and the login
// rate limiter inside SessionService.Login (through UIAuth.SignIn).
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string      `json:"username"`
		Password core.Secret `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	token, user, err := s.UIAuth.SignIn(w, r, req.Username, req.Password)
	if limited, ok := errors.AsType[*auth.RateLimitError](err); ok {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(limited.RetryAfter.Seconds())))))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many failed login attempts: try again later")
		return
	}
	if errors.Is(err, auth.ErrInvalidCredentials) {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "wrong username or password")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_mode":  string(auth.PrincipalSession),
		"csrf_token": auth.CSRFToken(token),
		"user":       map[string]any{"id": user.ID, "username": user.Username},
	})
}

// logout serves POST /api/v1/auth/logout. It deletes the session row behind
// the cookie, if any, and always clears the cookie.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		if err := s.Sessions.Logout(r.Context(), c.Value); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.UIAuth.ClearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}
