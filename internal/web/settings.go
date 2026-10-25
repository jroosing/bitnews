package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/a-h/templ"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// SettingsSection is one page of the Settings area. The sections are listed
// in settingsSections (the registry); the shell turns every entry into a
// link in the settings navigation and a page at /settings/<Slug>. To add a
// section, give its entry a Template (and a Data and Routes function when it
// needs them); see "Settings sections" in README.md.
type SettingsSection struct {
	// Slug is the last path element: /settings/<Slug>. It is also the ID of
	// the section in tests.
	Slug string
	// Title is the page heading and the label in the navigation.
	Title string
	// Icon is an icon name for the navigation.
	Icon string
	// Description is the one-sentence subtitle of the page.
	Description string
	// Order sorts the navigation; the first section is where /settings leads.
	Order int
	// Template is the file below assets/templates/pages/. Ignored when
	// Component is set.
	Template string
	// Component builds the section page inside the settings frame
	// (ADR-0011). It replaces Template.
	Component func(v *View, p settingsPage) templ.Component
	// Data returns the section's own data, which the template sees as
	// .Page.Data. Optional.
	Data func(s *Server, r *http.Request) (any, error)
	// Routes registers the section's extra routes (form posts, fragments).
	// Optional.
	Routes func(s *Server)
}

// settingsSections is the registry of sections. Add a line here and a
// template to add a section; everything else follows from the table.
func settingsSections() []SettingsSection {
	secs := []SettingsSection{
		{
			Slug: "paths", Title: "Paths & Categories", Icon: "folder", Order: 10,
			Description: "Where downloads are kept and the categories your media managers use.",
			Component:   SettingsPathsPage, Data: settingsPathsData, Routes: (*Server).registerPathRoutes,
		},
		{
			Slug: "servers", Title: "Usenet servers", Icon: "server", Order: 20,
			Description: "Providers, connections, retention and priority.",
			Template:    "settings_servers.html", Data: settingsServersData, Routes: (*Server).registerServerRoutes,
		},
		speedSection(),
		postprocessingSection(),
		torrentSection(),
		securitySection(),
	}
	slices.SortStableFunc(secs, func(a, b SettingsSection) int { return a.Order - b.Order })
	return secs
}

// settingsNavItem is one link of the settings navigation.
type settingsNavItem struct {
	Slug, Title, Icon, Path, Description string
	Active                               bool
}

// settingsPage is what a section template sees as .Page: the section, the
// navigation and the section's own data.
type settingsPage struct {
	Section settingsNavItem
	Nav     []settingsNavItem
	Data    any
}

func settingsNavFor(secs []SettingsSection, active string) []settingsNavItem {
	items := make([]settingsNavItem, 0, len(secs))
	for _, sec := range secs {
		items = append(items, settingsNavItem{
			Slug: sec.Slug, Title: sec.Title, Icon: sec.Icon, Description: sec.Description,
			Path: "/settings/" + sec.Slug, Active: sec.Slug == active,
		})
	}
	return items
}

func settingsPageFor(secs []SettingsSection, sec SettingsSection, data any) settingsPage {
	nav := settingsNavFor(secs, sec.Slug)
	return settingsPage{Section: nav[slices.IndexFunc(nav, func(n settingsNavItem) bool { return n.Slug == sec.Slug })], Nav: nav, Data: data}
}

// settingsComponent adapts a section's Component to the Page one; it is nil
// while the section still uses a template.
func settingsComponent(sec SettingsSection) func(*View, any) templ.Component {
	if sec.Component == nil {
		return nil
	}
	return func(v *View, data any) templ.Component { return sec.Component(v, data.(settingsPage)) }
}

// registerSettings adds the settings shell: /settings (a redirect to the
// first section), a page per section and the section's own routes.
func (s *Server) registerSettings() {
	secs := settingsSections()
	// The sidebar entry. It has no page of its own, so it is added to the
	// list without a route; the redirect below serves the path.
	s.pages = append(s.pages, Page{Path: "/settings", Title: "Settings", NavLabel: "Settings", NavIcon: "settings"})
	s.handleAuthed("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		s.redirect(w, r, "/settings/"+secs[0].Slug)
	})
	for _, sec := range secs {
		s.addPage(Page{
			Path: "/settings/" + sec.Slug, Template: sec.Template, Title: sec.Title,
			Component: settingsComponent(sec),
			Data: func(s *Server, r *http.Request) (any, error) {
				var data any
				if sec.Data != nil {
					var err error
					if data, err = sec.Data(s, r); err != nil {
						return nil, err
					}
				}
				return settingsPageFor(secs, sec, data), nil
			},
		})
		if sec.Routes != nil {
			sec.Routes(s)
		}
	}
}

// settingsSectionBySlug finds a section of the registry.
func settingsSectionBySlug(slug string) (SettingsSection, bool) {
	secs := settingsSections()
	i := slices.IndexFunc(secs, func(sec SettingsSection) bool { return sec.Slug == slug })
	if i < 0 {
		return SettingsSection{}, false
	}
	return secs[i], true
}

// renderSettingsPage renders a whole section page with data, for form posts
// that did not come from htmx (or that must reload the page).
func (s *Server) renderSettingsPage(w http.ResponseWriter, r *http.Request, status int, slug string, data any) {
	sec, ok := settingsSectionBySlug(slug)
	if !ok {
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not load this page. Details are in the server log.")
		return
	}
	v := s.newView(r, sec.Title)
	v.Page = settingsPageFor(settingsSections(), sec, data)
	if sec.Component != nil {
		s.renderComponent(w, r, status, sec.Component(v, v.Page.(settingsPage)))
		return
	}
	s.render(w, r, status, sec.Template, "layout_app", v)
}

// renderSettingsPart renders one named template of a page with data that is
// not a *View. The partial sees data as its dot, so data carries whatever it
// needs (the CSRF token for plain-form fallbacks).
func (s *Server) renderSettingsPart(w http.ResponseWriter, status int, page, name string, data any) {
	out, err := s.tpl.execute(page, name, data)
	if err != nil {
		s.Logger.Error("render failed", "page", page, "fragment", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setHTMLHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// settingsCSRF is the CSRF token of the request's principal, for the hidden
// _csrf field of forms that also work without JavaScript.
func settingsCSRF(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok {
		return p.CSRFToken()
	}
	return ""
}

// settingsTrigger sets the HX-Trigger header: a toast (level success, info,
// warn or danger) plus any other client events. Everything is written as
// ASCII, because browsers read header bytes as Latin-1.
func settingsTrigger(w http.ResponseWriter, level, message string, events map[string]any) {
	m := map[string]any{}
	maps.Copy(m, events)
	if message != "" {
		m["toast"] = map[string]string{"level": level, "message": message}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x7f:
			sb.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	w.Header().Set("HX-Trigger", sb.String())
}

// settingsFail answers a service error the form cannot show next to a field:
// 404 for an unknown ID, otherwise a logged 500.
func (s *Server) settingsFail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, core.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Not found", "That "+what+" no longer exists. Reload the page.")
		return
	}
	s.Logger.Error("settings request failed", "what", what, "path", r.URL.Path, "err", err)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not complete the request. Details are in the server log.")
}

// settingsAppError returns the app.Error inside err, if there is one.
func settingsAppError(err error) (*app.Error, bool) {
	return errors.AsType[*app.Error](err)
}

// settingsServices reports whether the services a section needs were given
// in Deps; if not it answers 503 and returns false.
func (s *Server) settingsServices(w http.ResponseWriter, r *http.Request, ok bool) bool {
	if ok {
		return true
	}
	s.renderError(w, r, http.StatusServiceUnavailable, "Not available", "This part of bitnews is not available in this build.")
	return false
}
