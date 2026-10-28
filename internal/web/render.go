package web

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/a-h/templ"

	"github.com/jroosing/bitnews/internal/auth"
)

// View is the data every layout receives. Page holds the page's own data.
type View struct {
	Title    string
	Path     string
	CSRF     string // the session's CSRF token; empty for anonymous pages
	AuthMode string // "session", "bypass", "disabled" or "" (anonymous page)
	Username string
	Version  string
	Page     any

	NavDesktop NavData
	NavMobile  NavData
	UserMenu   UserMenu

	Head *headAssets // the hashed asset URLs of the document head
}

// NavData is what the "nav" partial renders.
type NavData struct {
	NavLabel string
	Nav      []NavItem
}

// NavItem is one sidebar entry.
type NavItem struct {
	Path, Label, Icon string
	Active            bool
}

// UserMenu is what the "user_menu" partial renders.
type UserMenu struct {
	CSRF       string
	Initial    string
	UserLabel  string
	ModeLabel  string
	CanSignOut bool
}

// newView builds the layout data for a request. The principal (if the
// request went through UIAuth) decides the menu, banner and CSRF token.
func (s *Server) newView(r *http.Request, title string) *View {
	v := &View{Title: title, Path: r.URL.Path, Version: s.Version, Head: &s.head}
	menu := UserMenu{}
	if p, ok := auth.FromContext(r.Context()); ok {
		v.CSRF = p.CSRFToken()
		v.AuthMode = p.AuthMode()
		v.Username = p.Username
		menu.CSRF = v.CSRF
		switch p.Kind {
		case auth.PrincipalSession:
			menu.UserLabel, menu.ModeLabel, menu.CanSignOut = p.Username, "Signed in", true
		case auth.PrincipalLANBypass:
			menu.UserLabel, menu.ModeLabel = "Local network", "LAN bypass: no sign-in"
		default:
			menu.UserLabel, menu.ModeLabel = "Open access", "Authentication is off"
		}
	}
	if menu.UserLabel == "" {
		menu.UserLabel = "Guest"
	}
	menu.Initial = initial(menu.UserLabel)
	v.UserMenu = menu
	items := s.navItems(r.URL.Path)
	v.NavDesktop = NavData{NavLabel: "Main", Nav: items}
	v.NavMobile = NavData{NavLabel: "Main (mobile)", Nav: items}
	return v
}

// initial is the avatar letter: the first letter or digit of s, upper-cased.
func initial(s string) string {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return strings.ToUpper(string(r))
		}
	}
	if s == "" {
		return "?"
	}
	r, _ := utf8.DecodeRuneInString(s)
	return string(r)
}

// pageHeaders are set on every rendered HTML response.
func setHTMLHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
}

// renderComponent writes a templ component as a full response (a page or an
// htmx fragment). Rendering happens into a buffer first, so a render error
// (including an invalid Hx, which panics) becomes a clean 500.
func (s *Server) renderComponent(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	out, err := renderBytes(r, c)
	if err != nil {
		s.Logger.Error("render failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setHTMLHeaders(w)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out)
	}
}

// renderBytes renders a component to bytes. An hxError panic from
// Hx.MustAttrs is returned as an error; any other panic is passed on.
func renderBytes(r *http.Request, c templ.Component) (out []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			he, ok := p.(hxError)
			if !ok {
				panic(p)
			}
			out, err = nil, he
		}
	}()
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		return nil, fmt.Errorf("web: render: %w", err)
	}
	return buf.Bytes(), nil
}
