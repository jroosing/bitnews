package api

import (
	"net/http"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

type middleware = func(http.Handler) http.Handler

// routes registers the M1 routes. Authentication per route:
//
//   - health and setup: none (setup has its own token);
//   - login: Origin check and the login rate limiter;
//   - session routes: UIAuth, then CSRF;
//   - data routes: an API key of the right scope, or else the UI chain;
//   - API key management: a logged-in session only;
//   - adapter credential management: a UI principal (session, LAN bypass or
//     disable-auth) but never an API key;
//   - /sabnzbd/api and /api/v2/: the adapter's own credential check (the
//     apikey, or the SID session of a qBittorrent login), nothing else.
func (s *Server) routes() {
	csrf := auth.CSRF(s.Settings.Current)
	ui := func(h http.Handler) http.Handler { return s.UIAuth.Middleware(csrf(h)) }
	read := s.APIKeys.KeyOr(core.APIKeyScopeRead, ui)
	write := s.APIKeys.KeyOr(core.APIKeyScopeWrite, ui)
	sessionOnly := func(h http.Handler) http.Handler { return ui(auth.SessionOnly(h)) }

	handle := func(pattern string, wrap middleware, limit int64, h http.HandlerFunc) {
		s.mux.Handle(pattern, maxBody(limit, wrap(h)))
	}
	none := func(h http.Handler) http.Handler { return h }

	handle("GET /api/v1/health", none, smallBodyLimit, s.health)
	s.mux.Handle("POST /api/v1/setup", maxBody(smallBodyLimit, auth.SetupHandler(s.Setup)))
	handle("POST /api/v1/auth/login", csrf, smallBodyLimit, s.login)
	handle("POST /api/v1/auth/logout", ui, smallBodyLimit, s.logout)
	s.mux.Handle("GET /api/v1/auth/session", ui(s.UIAuth.SessionInfo()))
	handle("GET /api/v1/version", read, smallBodyLimit, s.version)

	handle("GET /api/v1/downloads", read, smallBodyLimit, s.listDownloads)
	handle("GET /api/v1/downloads/{id}", read, smallBodyLimit, s.getDownload)
	// Uploads are streamed, so this route's cap is the NZB limit, not 1 MiB.
	handle("POST /api/v1/downloads", write, addBodyLimit, s.addDownload)
	handle("POST /api/v1/downloads/{id}/pause", write, smallBodyLimit, s.pauseDownload)
	handle("POST /api/v1/downloads/{id}/resume", write, smallBodyLimit, s.resumeDownload)
	handle("PATCH /api/v1/downloads/{id}", write, smallBodyLimit, s.patchDownload)
	handle("DELETE /api/v1/downloads/{id}", write, smallBodyLimit, s.deleteDownload)

	handle("GET /api/v1/categories", read, smallBodyLimit, s.listCategories)
	handle("POST /api/v1/categories", write, smallBodyLimit, s.createCategory)
	handle("PATCH /api/v1/categories/{id}", write, smallBodyLimit, s.patchCategory)
	handle("DELETE /api/v1/categories/{id}", write, smallBodyLimit, s.deleteCategory)

	handle("GET /api/v1/servers", read, smallBodyLimit, s.listServers)
	handle("POST /api/v1/servers", write, smallBodyLimit, s.createServer)
	handle("POST /api/v1/servers/test", write, smallBodyLimit, s.testServerSpec)
	handle("GET /api/v1/servers/{id}", read, smallBodyLimit, s.getServer)
	handle("PATCH /api/v1/servers/{id}", write, smallBodyLimit, s.patchServer)
	handle("DELETE /api/v1/servers/{id}", write, smallBodyLimit, s.deleteServer)
	handle("POST /api/v1/servers/{id}/test", write, smallBodyLimit, s.testServer)

	handle("GET /api/v1/settings", read, smallBodyLimit, s.getSettings)
	handle("PUT /api/v1/settings", write, defaultBodyLimit, s.putSettings)

	// The event stream bounds its own writes instead of using the server's
	// write timeout.
	handle("GET /api/v1/events", read, smallBodyLimit, s.events)

	handle("GET /api/v1/api-keys", sessionOnly, smallBodyLimit, s.listAPIKeys)
	handle("POST /api/v1/api-keys", sessionOnly, smallBodyLimit, s.createAPIKey)
	handle("DELETE /api/v1/api-keys/{id}", sessionOnly, smallBodyLimit, s.deleteAPIKey)

	if s.AdapterCredentials != nil {
		// Media managers must stay configurable under disable-auth and LAN
		// bypass, so these routes take the UI principals sessionOnly refuses.
		// KeyOr makes a request that carries an API key a key request,
		// which UIPrincipalOnly refuses, rather than letting the bypass
		// rescue it.
		uiOnly := func(h http.Handler) http.Handler {
			return s.APIKeys.KeyOr(core.APIKeyScopeWrite, ui)(auth.UIPrincipalOnly(h))
		}
		handle("GET /api/v1/adapter-credentials", uiOnly, smallBodyLimit, s.listAdapterCredentials)
		handle("POST /api/v1/adapter-credentials", uiOnly, smallBodyLimit, s.createAdapterCredential)
		handle("DELETE /api/v1/adapter-credentials/{id}", uiOnly, smallBodyLimit, s.deleteAdapterCredential)
	}

	if s.SABnzbd != nil {
		// Uploads are streamed, so the cap is the NZB limit. The handler
		// lowers it for every other mode.
		s.mux.Handle("/sabnzbd/api", maxBody(addBodyLimit, s.SABnzbd))
		s.mux.Handle("/sabnzbd/api/", maxBody(addBodyLimit, s.SABnzbd))
	}

	if s.QBit != nil {
		// torrents/add uploads .torrent files, so the cap is the NZB limit
		// here too. The handler applies its own, lower limits.
		s.mux.Handle("/api/v2/", maxBody(addBodyLimit, s.QBit))
	}
}
