package api

import (
	"errors"
	"net/http"
	"slices"

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

// putSettings replaces every setting except the authentication options:
// they decide who may reach the UI without logging in, and an API key could
// otherwise open the UI to the whole LAN. They are changed in the UI.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var next core.Settings
	if !decodeJSON(w, r, &next) {
		return
	}
	// The guard compares with the stored Settings under the lock, so the
	// auth section cannot change between the check and the write.
	err := s.Settings.Change(r.Context(), func(cur *core.Settings) error {
		if !authEqual(cur.Auth, next.Auth) {
			return errSessionRequired
		}
		*cur = next
		return nil
	})
	if errors.Is(err, errSessionRequired) {
		writeError(w, http.StatusForbidden, "session_required", "the authentication settings are changed in the UI, not through the API")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse(s.Settings.Current()))
}

// errSessionRequired stops the API from changing the auth section.
var errSessionRequired = errors.New("api: session required")

func authEqual(a, b core.Auth) bool {
	return a.DisableAuth == b.DisableAuth &&
		slices.Equal(a.LANBypassCIDRs, b.LANBypassCIDRs) &&
		slices.Equal(a.TrustedProxies, b.TrustedProxies) &&
		slices.Equal(a.AllowedHosts, b.AllowedHosts)
}
