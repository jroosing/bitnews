package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// signedIn reports whether the request already has a principal.
func (s *Server) signedIn(r *http.Request) bool {
	_, ok := s.UIAuth.Principal(r)
	return ok
}

// getRoot is the front door: setup if no admin exists, the queue if signed
// in, else the login page.
func (s *Server) getRoot(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.Setup.Required():
		s.redirect(w, r, "/setup")
	case s.signedIn(r):
		s.redirect(w, r, "/queue")
	default:
		s.redirect(w, r, "/login")
	}
}

// loginPage is the data of the login form.
type loginPage struct {
	Username string
	Next     string
	Error    string
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.Setup.Required():
		s.redirect(w, r, "/setup")
		return
	case s.signedIn(r):
		s.redirect(w, r, "/queue")
		return
	}
	v := s.newView(r, "Sign in")
	p := loginPage{Next: safeNext(r.URL.Query().Get("next"), s.knownPath)}
	v.Page = p
	s.renderComponent(w, r, http.StatusOK, LoginPage(v, p))
}

// renderLogin shows the form again with a message: only the form for htmx,
// the whole page otherwise.
func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, p loginPage) {
	v := s.newView(r, "Sign in")
	v.Page = p
	if isHTMX(r) {
		s.renderComponent(w, r, status, LoginForm(p))
		return
	}
	s.renderComponent(w, r, status, LoginPage(v, p))
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if s.Setup.Required() {
		s.redirect(w, r, "/setup")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	page := loginPage{Username: r.PostForm.Get("username"), Next: safeNext(r.PostForm.Get("next"), s.knownPath)}
	password := core.Secret(r.PostForm.Get("password"))

	_, _, err := s.UIAuth.SignIn(w, r, page.Username, password)
	if limited, ok := errors.AsType[*auth.RateLimitError](err); ok {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(limited.RetryAfter)))
		page.Error = "Too many failed attempts. Try again in " + waitText(limited.RetryAfter) + "."
		s.renderLogin(w, r, http.StatusTooManyRequests, page)
		return
	}
	if errors.Is(err, auth.ErrInvalidCredentials) {
		page.Error = "Invalid username or password."
		s.renderLogin(w, r, http.StatusUnprocessableEntity, page)
		return
	}
	if errors.Is(err, auth.ErrSessionStart) {
		s.sessionStartFailed(w, r, err)
		return
	}
	if err != nil {
		s.Logger.Error("login failed", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not sign you in. Details are in the server log.")
		return
	}
	target := page.Next
	if target == "" {
		target = "/queue"
	}
	s.redirect(w, r, target)
}

// sessionStartFailed answers a sign-in whose credentials were good but whose
// new session could not be used.
func (s *Server) sessionStartFailed(w http.ResponseWriter, r *http.Request, err error) {
	s.Logger.Error("new session unusable", "err", err)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not start your session. Details are in the server log.")
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		if err := s.Sessions.Logout(r.Context(), c.Value); err != nil {
			s.Logger.Error("logout failed", "err", err)
		}
	}
	s.UIAuth.ClearSessionCookie(w, r)
	s.redirect(w, r, "/login")
}

func (s *Server) clientIP(r *http.Request) netip.Addr {
	return auth.ClientIPFrom(r, s.Settings.Current().Auth)
}

// retryAfterSeconds rounds a wait up to whole seconds, at least one.
func retryAfterSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

// waitText words a wait: seconds up to two minutes, then minutes.
func waitText(d time.Duration) string {
	secs := retryAfterSeconds(d)
	if secs <= 120 {
		return fmt.Sprintf("%d s", secs)
	}
	return fmt.Sprintf("%d min", int(math.Ceil(float64(secs)/60)))
}

// setupPage is the data of the setup form.
type setupPage struct {
	Token, Username string
	Error           string
	Errors          map[string]string
	MinPassword     int
	PasswordHelp    string
}

func newSetupPage() setupPage {
	return setupPage{
		MinPassword: auth.MinPasswordRunes,
		PasswordHelp: fmt.Sprintf("At least %d characters. A few random words make a strong password.",
			auth.MinPasswordRunes),
	}
}

func (s *Server) getSetup(w http.ResponseWriter, r *http.Request) {
	if !s.Setup.Required() {
		if s.signedIn(r) {
			s.redirect(w, r, "/queue")
		} else {
			s.redirect(w, r, "/login")
		}
		return
	}
	v := s.newView(r, "Set up")
	p := newSetupPage()
	v.Page = p
	s.renderComponent(w, r, http.StatusOK, SetupPage(v, p))
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, status int, p setupPage) {
	v := s.newView(r, "Set up")
	v.Page = p
	if isHTMX(r) {
		s.renderComponent(w, r, status, SetupForm(p))
		return
	}
	s.renderComponent(w, r, status, SetupPage(v, p))
}

func (s *Server) postSetup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	page := newSetupPage()
	page.Token = strings.TrimSpace(r.PostForm.Get("token"))
	page.Username = r.PostForm.Get("username")
	password := core.Secret(r.PostForm.Get("password"))
	confirm := r.PostForm.Get("confirm")
	page.Errors = map[string]string{}

	if err := auth.ValidateUsername(page.Username); err != nil {
		page.Errors["username"] = fmt.Sprintf("Choose a username of 1 to %d characters, without spaces at the ends or control characters.", auth.MaxUsernameBytes)
	}
	switch err := auth.ValidatePassword(password); {
	case errors.Is(err, auth.ErrPasswordTooShort):
		page.Errors["password"] = fmt.Sprintf("The password is too short: use at least %d characters.", auth.MinPasswordRunes)
	case errors.Is(err, auth.ErrPasswordTooLong):
		page.Errors["password"] = fmt.Sprintf("The password is too long: use at most %d bytes.", auth.MaxPasswordBytes)
	case err != nil:
		page.Errors["password"] = "The password is not valid text."
	case confirm != password.Reveal():
		page.Errors["confirm"] = "The two passwords do not match."
	}
	if page.Token == "" {
		page.Errors["token"] = "Enter the setup token from the bitnews log."
	}
	if len(page.Errors) > 0 {
		s.renderSetup(w, r, http.StatusUnprocessableEntity, page)
		return
	}

	err := s.Setup.Complete(r.Context(), page.Token, page.Username, password)
	switch {
	case errors.Is(err, auth.ErrBadSetupToken):
		page.Errors["token"] = "That setup token is not right. Copy it again from the bitnews log."
		s.renderSetup(w, r, http.StatusUnprocessableEntity, page)
		return
	case errors.Is(err, auth.ErrSetupClosed):
		s.redirect(w, r, "/login")
		return
	case errors.Is(err, auth.ErrInvalidUsername), errors.Is(err, auth.ErrPasswordTooShort), errors.Is(err, auth.ErrPasswordTooLong), errors.Is(err, auth.ErrPasswordInvalid):
		page.Error = "The username or password was refused."
		s.renderSetup(w, r, http.StatusUnprocessableEntity, page)
		return
	case err != nil:
		s.Logger.Error("setup failed", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not create the account. Details are in the server log.")
		return
	}

	// The admin exists. Sign them in; if the login itself fails, the login
	// page is the fallback.
	_, _, err = s.UIAuth.SignIn(w, r, page.Username, password)
	if errors.Is(err, auth.ErrSessionStart) {
		s.sessionStartFailed(w, r, err)
		return
	}
	if err != nil {
		s.redirect(w, r, "/login")
		return
	}
	s.redirect(w, r, "/queue")
}
