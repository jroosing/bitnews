package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// passwordView is the "Password" card. The password fields are write-only:
// nothing typed is ever sent back.
type passwordView struct {
	CSRF   string
	Errors map[string]string
	Error  string
	Dirty  bool
	Saved  bool
	// Note is shown after a plain-POST success (htmx shows a toast).
	Note string
	// Unavailable is set when nobody is signed in with a password (sign-in is
	// off or the device is on the LAN bypass), so there is no password to
	// change from this session.
	Unavailable bool
}

func (s *Server) passwordViewFor(r *http.Request) passwordView {
	p, ok := auth.FromContext(r.Context())
	return passwordView{CSRF: settingsCSRF(r), Unavailable: !ok || p.Kind != auth.PrincipalSession}
}

func (s *Server) registerPasswordRoutes() {
	s.handleAuthed("POST /settings/security/password", s.postPassword)
}

const (
	passwordCurrentMsg  = "That is not your current password."
	passwordShortMsg    = "Use at least 12 characters. A few random words make a good, easy to remember passphrase."
	passwordLongMsg     = "Use at most 1024 characters."
	passwordInvalidMsg  = "The password contains characters that cannot be stored."
	passwordMismatchMsg = "The two new passwords do not match."
)

func (s *Server) postPassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	v := s.passwordViewFor(r)
	v.Errors = map[string]string{}
	respond := func(status int) { s.respondPassword(w, r, status, v) }

	p, _ := auth.FromContext(r.Context())
	if v.Unavailable {
		v.Error = "You are not signed in with a password, so there is none to change from here."
		respond(http.StatusUnprocessableEntity)
		return
	}

	f := r.PostForm
	current, next, again := f.Get("current_password"), f.Get("new_password"), f.Get("confirm_password")
	if current == "" {
		v.Errors["current_password"] = "Enter your current password."
	}
	switch {
	case next == "":
		v.Errors["new_password"] = passwordShortMsg
	case again != next:
		v.Errors["confirm_password"] = passwordMismatchMsg
	}
	if len(v.Errors) > 0 {
		v.Dirty, v.Error = true, "The password was not changed. Fix the marked fields and try again."
		respond(http.StatusUnprocessableEntity)
		return
	}

	var token string
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		token = c.Value
	}
	ended, err := s.Sessions.ChangePassword(r.Context(), p.UserID, core.Secret(current), core.Secret(next), token, s.clientIP(r))
	if limited, ok := errors.AsType[*auth.RateLimitError](err); ok {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(limited.RetryAfter)))
		v.Dirty, v.Error = true, "Too many wrong attempts. Try again in "+waitText(limited.RetryAfter)+"."
		respond(http.StatusTooManyRequests)
		return
	}
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		v.Errors["current_password"] = passwordCurrentMsg
	case errors.Is(err, auth.ErrPasswordTooShort):
		v.Errors["new_password"] = passwordShortMsg
	case errors.Is(err, auth.ErrPasswordTooLong):
		v.Errors["new_password"] = passwordLongMsg
	case errors.Is(err, auth.ErrPasswordInvalid):
		v.Errors["new_password"] = passwordInvalidMsg
	case err != nil:
		s.settingsFail(w, r, "password", err)
		return
	}
	if len(v.Errors) > 0 {
		v.Dirty, v.Error = true, "The password was not changed. Fix the marked fields and try again."
		respond(http.StatusUnprocessableEntity)
		return
	}

	msg := "Password changed."
	switch ended {
	case 0:
	case 1:
		msg += " 1 other browser was signed out."
	default:
		msg += " " + strconv.Itoa(ended) + " other browsers were signed out."
	}
	v = s.passwordViewFor(r)
	if isHTMX(r) {
		settingsTrigger(w, "success", msg, nil)
	} else {
		v.Saved, v.Note = true, msg
	}
	respond(http.StatusOK)
}

func (s *Server) respondPassword(w http.ResponseWriter, r *http.Request, status int, v passwordView) {
	s.respondSecurityCard(w, r, status, "password_card", v, func(d *securityPageData) { d.Password = v })
}
