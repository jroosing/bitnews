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
//   - API key management: a logged-in session only.
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
	handle("POST /api/v1/downloads/{id}/pause", write, smallBodyLimit, s.pauseDownload)
	handle("POST /api/v1/downloads/{id}/resume", write, smallBodyLimit, s.resumeDownload)
	handle("PATCH /api/v1/downloads/{id}", write, smallBodyLimit, s.patchDownload)
	handle("DELETE /api/v1/downloads/{id}", write, smallBodyLimit, s.deleteDownload)

	handle("GET /api/v1/categories", read, smallBodyLimit, s.listCategories)
	handle("POST /api/v1/categories", write, smallBodyLimit, s.createCategory)
	handle("PATCH /api/v1/categories/{id}", write, smallBodyLimit, s.patchCategory)
	handle("DELETE /api/v1/categories/{id}", write, smallBodyLimit, s.deleteCategory)

	handle("GET /api/v1/settings", read, smallBodyLimit, s.getSettings)
	handle("PUT /api/v1/settings", write, defaultBodyLimit, s.putSettings)

	// The event stream bounds its own writes instead of using the server's
	// write timeout.
	handle("GET /api/v1/events", read, smallBodyLimit, s.events)

	handle("GET /api/v1/api-keys", sessionOnly, smallBodyLimit, s.listAPIKeys)
	handle("POST /api/v1/api-keys", sessionOnly, smallBodyLimit, s.createAPIKey)
	handle("DELETE /api/v1/api-keys/{id}", sessionOnly, smallBodyLimit, s.deleteAPIKey)
}
