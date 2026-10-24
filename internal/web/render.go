package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jroosing/bitnews/internal/auth"
)

// templates holds one parsed set per page file. Every set contains the
// layouts, the shared partials and all components, plus that one page, so
// pages can each define "content" without clashing.
type templates struct {
	sets map[string]*template.Template
}

// sharedTemplates are parsed into every set, besides components/*.html.
var sharedTemplates = []string{"head.html", "nav.html", "layout_app.html", "layout_bare.html"}

func loadTemplates(fsys fs.FS, funcs template.FuncMap) (*templates, error) {
	base := template.New("base").Funcs(funcs).Option("missingkey=zero")
	for _, f := range sharedTemplates {
		if err := parseFile(base, fsys, f); err != nil {
			return nil, err
		}
	}
	components, err := fs.Glob(fsys, "components/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range components {
		if err := parseFile(base, fsys, f); err != nil {
			return nil, err
		}
	}
	pages, err := fs.Glob(fsys, "pages/*.html")
	if err != nil {
		return nil, err
	}
	t := &templates{sets: map[string]*template.Template{}}
	for _, f := range pages {
		set, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if err := parseFile(set, fsys, f); err != nil {
			return nil, err
		}
		t.sets[path.Base(f)] = set
	}
	return t, nil
}

func parseFile(t *template.Template, fsys fs.FS, name string) error {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	if _, err := t.New(name).Parse(string(b)); err != nil {
		return fmt.Errorf("web: parse %s: %w", name, err)
	}
	return nil
}

// execute renders the layout of a page ("layout_app" or "layout_bare"), or a
// named fragment of it when fragment is not empty.
func (t *templates) execute(page, name string, data any) ([]byte, error) {
	set, ok := t.sets[page]
	if !ok {
		return nil, fmt.Errorf("web: unknown page template %q", page)
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("web: render %s/%s: %w", page, name, err)
	}
	return buf.Bytes(), nil
}

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
	v := &View{Title: title, Path: r.URL.Path, Version: s.Version}
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

// render writes a full page. Rendering happens into a buffer first, so a
// template error becomes a clean 500 instead of a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, layout string, v *View) {
	out, err := s.tpl.execute(page, layout, v)
	if err != nil {
		s.Logger.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setHTMLHeaders(w)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out)
	}
}

// renderFragment writes one named template of a page, for htmx swaps.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, status int, page, fragment string, v *View) {
	s.render(w, r, status, page, fragment, v)
}
