package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// appKeyView is the "API key" card (M7-05, ADR-0006): the one key that
// Sonarr, Radarr and scripts use, shown as plain text with a Copy button.
type appKeyView struct {
	CSRF string
	// Unavailable is set when the build has no app key service.
	Unavailable bool
	// Key is the key. It is in the HTML of this card only (the text and the
	// Copy button), for whoever may open the Security page.
	Key string
	// Host and Port are what the browser used to reach bitnews, which is
	// what a media manager on the same network can use too.
	Host, Port string
	TLS        bool
	// Loopback says the browser used localhost, which a media manager on
	// another machine or in a container cannot reach.
	Loopback bool
	// Confirm is set instead of the card while a rotation waits for a yes.
	Confirm *confirmView
}

const appKeyPath = "/settings/security/app-key"

func (s *Server) appKeyViewFor(r *http.Request) appKeyView {
	v := appKeyView{CSRF: settingsCSRF(r)}
	if s.AppKey == nil {
		v.Unavailable = true
		return v
	}
	v.Key = s.AppKey.Current().Reveal()
	v.Host, v.Port, v.TLS = requestEndpoint(r)
	v.Loopback = isLoopbackHost(v.Host)
	return v
}

func (s *Server) registerAppKeyRoutes() {
	s.handleAuthed("GET "+appKeyPath, s.getAppKey)
	s.handleAuthed("POST "+appKeyPath, s.postAppKeyRotate)
}

func (s *Server) getAppKey(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/security")
		return
	}
	s.renderSettingsPart(w, http.StatusOK, settingsSecurityPage, "appkey_card", s.appKeyViewFor(r))
}

// postAppKeyRotate makes a new key. Every client stops working with the old
// one at once, so it asks first: without confirmed=1 it answers with the
// confirm step and changes nothing. The new key is shown in the card, never in
// the toast or the log.
func (s *Server) postAppKeyRotate(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.AppKey != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	v := s.appKeyViewFor(r)
	if r.PostForm.Get("confirmed") != "1" {
		v.Confirm = &confirmView{
			ID: "appkey", Action: appKeyPath, Title: "Rotate the API key",
			Heading: "Check this before you rotate the key", Label: "Yes, rotate the key",
			CSRF: v.CSRF, Cancel: "/settings/security",
			Bullets: []string{
				"bitnews makes a new key and the old one stops working at once, for SABnzbd, qBittorrent and the API.",
				"Right away, every client (Sonarr, Radarr, scripts) stops working until updated with the new key.",
			},
			Fields: []confirmField{{"confirmed", "1"}},
		}
		s.respondAppKey(w, r, http.StatusOK, v)
		return
	}
	if _, err := s.AppKey.Rotate(r.Context()); err != nil {
		s.settingsFail(w, r, "API key", err)
		return
	}
	v = s.appKeyViewFor(r)
	if isHTMX(r) {
		settingsTrigger(w, "success", "API key rotated. Update Sonarr, Radarr and your scripts with the new key.", nil)
	}
	s.respondAppKey(w, r, http.StatusOK, v)
}

func (s *Server) respondAppKey(w http.ResponseWriter, r *http.Request, status int, v appKeyView) {
	s.respondSecurityCard(w, r, status, "appkey_card", v, func(d *securityPageData) { d.Key = v })
}

// requestEndpoint is the host and port the browser used to reach bitnews.
// tls says the request itself came in over TLS.
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
