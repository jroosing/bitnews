package web

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// adaptersView is the "Media manager credentials" card: one credential per
// Sonarr, Radarr, ... so each can be revoked on its own.
type adaptersView struct {
	CSRF        string
	Unavailable bool
	Items       []adapterRow
	// Kind, Name and Username are the add form as typed.
	Kind, Name, Username string
	Kinds                []option
	Errors               map[string]string
	Error                string
	// Secret is set only in the answer to a create.
	Secret *shownSecret
}

// adapterRow is one credential. It never holds the key.
type adapterRow struct {
	CSRF, ID, Name, Username string
	Kind, KindLabel          string
	Created, LastUsed        whenView
}

// The kinds in the form.
const (
	adapterFormSAB  = "sab"
	adapterFormQBit = "qbit"
)

var adapterKinds = map[string]auth.TokenKind{
	adapterFormSAB:  auth.KindAdapterSAB,
	adapterFormQBit: auth.KindAdapterQBit,
}

func adapterKindLabel(k auth.TokenKind) string {
	if k == auth.KindAdapterQBit {
		return "qBittorrent"
	}
	return "SABnzbd"
}

func (s *Server) adaptersViewFor(r *http.Request) (adaptersView, error) {
	v := adaptersView{
		CSRF: settingsCSRF(r), Kind: adapterFormSAB,
		Kinds: []option{
			{Value: adapterFormSAB, Label: "SABnzbd (Usenet)"},
			{Value: adapterFormQBit, Label: "qBittorrent (torrents)"},
		},
	}
	if s.AdapterCredentials == nil {
		v.Unavailable = true
		return v, nil
	}
	items, err := s.AdapterCredentials.List(r.Context())
	if err != nil {
		return adaptersView{}, err
	}
	now := time.Now()
	for _, c := range items {
		kind := adapterFormSAB
		if c.Kind == auth.KindAdapterQBit {
			kind = adapterFormQBit
		}
		row := adapterRow{
			CSRF: v.CSRF, ID: string(c.ID), Name: c.Name, Username: c.Username,
			Kind: kind, KindLabel: adapterKindLabel(c.Kind), Created: histWhen(now, c.CreatedAt),
		}
		if c.LastUsedAt != nil {
			row.LastUsed = histWhen(now, *c.LastUsedAt)
		}
		v.Items = append(v.Items, row)
	}
	return v, nil
}

func (s *Server) registerAdapterRoutes() {
	s.handleAuthed("GET /settings/security/adapters", s.getAdapters)
	s.handleAuthed("POST /settings/security/adapters", s.postAdapterCreate)
	s.handleAuthed("POST /settings/security/adapters/{id}/revoke", s.postAdapterRevoke)
}

func (s *Server) getAdapters(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/security")
		return
	}
	v, err := s.adaptersViewFor(r)
	if err != nil {
		s.settingsFail(w, r, "credential", err)
		return
	}
	s.renderSettingsPart(w, http.StatusOK, settingsSecurityPage, "adapters_card", v)
}

func (s *Server) postAdapterCreate(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.AdapterCredentials != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	f := r.PostForm
	kindName, name, username := f.Get("kind"), f.Get("name"), strings.TrimSpace(f.Get("username"))
	v, err := s.adaptersViewFor(r)
	if err != nil {
		s.settingsFail(w, r, "credential", err)
		return
	}
	v.Kind, v.Name, v.Username, v.Errors = kindName, name, username, map[string]string{}

	kind, known := adapterKinds[kindName]
	if !known {
		v.Errors["kind"] = "Choose SABnzbd or qBittorrent."
	}
	var plain string
	var info auth.AdapterCredentialInfo
	if known {
		plain, info, err = s.AdapterCredentials.Create(r.Context(), kind, name, username)
		switch {
		case errors.Is(err, auth.ErrInvalidAdapterName):
			v.Errors["name"] = "Enter a name of 1 to 100 characters, such as Sonarr or Radarr 4K."
		case errors.Is(err, auth.ErrInvalidAdapterUsername):
			v.Errors["username"] = "Use 1 to 32 characters: lowercase letters, digits, underscore and hyphen."
		case errors.Is(err, auth.ErrAdapterUsernameTaken):
			v.Errors["username"] = "Another qBittorrent credential already uses this username."
		case err != nil:
			s.settingsFail(w, r, "credential", err)
			return
		}
	}
	if len(v.Errors) > 0 {
		v.Error = "The credential was not created. Fix the marked fields and try again."
		s.respondAdapters(w, r, http.StatusUnprocessableEntity, v)
		return
	}

	if v, err = s.adaptersViewFor(r); err != nil {
		s.settingsFail(w, r, "credential", err)
		return
	}
	v.Secret = adapterSecret(r, kind, info, plain)
	if isHTMX(r) {
		settingsTrigger(w, "success", "Credential created. Copy the values now: they are shown only once.", nil)
	}
	s.respondAdapters(w, r, http.StatusOK, v)
}

func (s *Server) postAdapterRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.AdapterCredentials != nil) {
		return
	}
	if err := s.AdapterCredentials.Revoke(r.Context(), core.ID(r.PathValue("id"))); err != nil {
		s.settingsFail(w, r, "credential", err)
		return
	}
	v, err := s.adaptersViewFor(r)
	if err != nil {
		s.settingsFail(w, r, "credential", err)
		return
	}
	if isHTMX(r) {
		settingsTrigger(w, "success", "Credential revoked. The media manager is locked out now.", nil)
	}
	s.respondAdapters(w, r, http.StatusOK, v)
}

func (s *Server) respondAdapters(w http.ResponseWriter, r *http.Request, status int, v adaptersView) {
	if v.Secret != nil {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.respondSecurityCard(w, r, status, "adapters_card", v, func(d *securityPageData) { d.Adapters = v })
}

// requestEndpoint is the host and port the browser used to reach bitnews,
// which is what a media manager on the same network can use too. TLS says
// the request itself came in over TLS.
func requestEndpoint(r *http.Request) (host, port string, tls bool) {
	tls = r.TLS != nil
	h, p, err := net.SplitHostPort(r.Host)
	if err != nil {
		h, p = strings.Trim(r.Host, "[]"), "80"
		if tls {
			p = "443"
		}
	}
	return h, p, tls
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}

// adapterSecret builds the show-once block with the values Sonarr, Radarr
// and friends ask for.
func adapterSecret(r *http.Request, kind auth.TokenKind, info auth.AdapterCredentialInfo, plain string) *shownSecret {
	host, port, tls := requestEndpoint(r)
	ssl := "Leave “Use SSL” off."
	if tls {
		ssl = "Turn “Use SSL” on."
	}
	sec := &shownSecret{
		ID: "adapters", Heading: "Settings for " + info.Name,
		Intro: "Enter these in your media manager. The " + map[bool]string{true: "password", false: "API key"}[kind == auth.KindAdapterQBit] +
			" is shown only once: bitnews stores a fingerprint, not the key, so it cannot show it again. If you lose it, revoke this credential and make a new one.",
		Done: "/settings/security/adapters",
	}
	if kind == auth.KindAdapterQBit {
		sec.Fields = []secretField{
			{Label: "Host", Value: host}, {Label: "Port", Value: port},
			{Label: "Username", Value: info.Username}, {Label: "Password", Value: plain, Secret: true},
		}
		sec.Hints = []string{"In Sonarr or Radarr: Settings, Download Clients, add qBittorrent. Leave the URL base empty. " + ssl}
	} else {
		sec.Fields = []secretField{
			{Label: "Host", Value: host}, {Label: "Port", Value: port},
			{Label: "URL base", Value: "sabnzbd"}, {Label: "API key", Value: plain, Secret: true},
		}
		sec.Hints = []string{"In Sonarr or Radarr: Settings, Download Clients, add SABnzbd. " + ssl}
	}
	if isLoopbackHost(host) {
		sec.Hints = append(sec.Hints, "You opened bitnews as “"+host+"”. If your media manager runs on another machine or in a container, use this machine's network address or name instead.")
	}
	return sec
}
