package api

import (
	"net/http"
	"slices"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// settingsResponse is the Settings with empty lists instead of nulls.
func settingsResponse(s core.Settings) core.Settings {
	nonNil := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}
	s.Auth.LANBypassCIDRs = nonNil(s.Auth.LANBypassCIDRs)
	s.Auth.TrustedProxies = nonNil(s.Auth.TrustedProxies)
	s.Auth.AllowedHosts = nonNil(s.Auth.AllowedHosts)
	return s
}

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, settingsResponse(s.Settings.Current()))
}

// putSettings replaces every setting. Because the authentication options
// decide who may reach the UI without logging in, a request that comes in
// with an API key may not change them: a script key could otherwise open the
// UI to the whole LAN.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var next core.Settings
	if !decodeJSON(w, r, &next) {
		return
	}
	if p, ok := auth.FromContext(r.Context()); ok && p.Kind == auth.PrincipalAPIKey &&
		!authEqual(s.Settings.Current().Auth, next.Auth) {
		writeError(w, http.StatusForbidden, "session_required", "changing the authentication settings needs a logged-in session")
		return
	}
	if err := s.Settings.Update(r.Context(), next); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse(s.Settings.Current()))
}

func authEqual(a, b core.Auth) bool {
	return a.DisableAuth == b.DisableAuth &&
		slices.Equal(a.LANBypassCIDRs, b.LANBypassCIDRs) &&
		slices.Equal(a.TrustedProxies, b.TrustedProxies) &&
		slices.Equal(a.AllowedHosts, b.AllowedHosts)
}
