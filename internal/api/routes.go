package api

import (
	"net/http"

	"github.com/jroosing/bitnews/internal/auth"
)

type middleware = func(http.Handler) http.Handler

// routes registers the M1 routes. Authentication per route:
//
//   - health and setup: none (setup has its own token);
//   - login: Origin check and the login rate limiter;
//   - session routes: UIAuth, then CSRF;
//   - data routes: the app key (full access), or else the UI chain;
//   - /sabnzbd/api and /api/v2/: the adapter's own check (the apikey, or the
//     SID session of a qBittorrent login), nothing else.
//
// The key is managed from the UI only: the API has no key endpoints.
func (s *Server) routes() {
	csrf := auth.CSRF(s.Settings.Current)
	ui := func(h http.Handler) http.Handler { return s.UIAuth.Middleware(csrf(h)) }
	// One key with full access: reading and writing routes authenticate alike.
	keyOrUI := s.AppKey.KeyOr(ui)

	handle := func(pattern string, wrap middleware, limit int64, h http.HandlerFunc) {
		s.mux.Handle(pattern, maxBody(limit, wrap(h)))
	}
	none := func(h http.Handler) http.Handler { return h }

	handle("GET /api/v1/health", none, smallBodyLimit, s.health)
	s.mux.Handle("POST /api/v1/setup", maxBody(smallBodyLimit, auth.SetupHandler(s.Setup)))
	handle("POST /api/v1/auth/login", csrf, smallBodyLimit, s.login)
	handle("POST /api/v1/auth/logout", ui, smallBodyLimit, s.logout)
	s.mux.Handle("GET /api/v1/auth/session", ui(s.UIAuth.SessionInfo()))
	handle("GET /api/v1/version", keyOrUI, smallBodyLimit, s.version)

	handle("GET /api/v1/downloads", keyOrUI, smallBodyLimit, s.listDownloads)
	handle("GET /api/v1/downloads/{id}", keyOrUI, smallBodyLimit, s.getDownload)
	// Uploads are streamed, so this route's cap is the NZB limit, not 1 MiB.
	handle("POST /api/v1/downloads", keyOrUI, addBodyLimit, s.addDownload)
	handle("POST /api/v1/downloads/{id}/pause", keyOrUI, smallBodyLimit, s.pauseDownload)
	handle("POST /api/v1/downloads/{id}/resume", keyOrUI, smallBodyLimit, s.resumeDownload)
	handle("PATCH /api/v1/downloads/{id}", keyOrUI, smallBodyLimit, s.patchDownload)
	handle("DELETE /api/v1/downloads/{id}", keyOrUI, smallBodyLimit, s.deleteDownload)

	handle("GET /api/v1/categories", keyOrUI, smallBodyLimit, s.listCategories)
	handle("POST /api/v1/categories", keyOrUI, smallBodyLimit, s.createCategory)
	handle("PATCH /api/v1/categories/{id}", keyOrUI, smallBodyLimit, s.patchCategory)
	handle("DELETE /api/v1/categories/{id}", keyOrUI, smallBodyLimit, s.deleteCategory)

	handle("GET /api/v1/servers", keyOrUI, smallBodyLimit, s.listServers)
	handle("POST /api/v1/servers", keyOrUI, smallBodyLimit, s.createServer)
	handle("POST /api/v1/servers/test", keyOrUI, smallBodyLimit, s.testServerSpec)
	handle("GET /api/v1/servers/{id}", keyOrUI, smallBodyLimit, s.getServer)
	handle("PATCH /api/v1/servers/{id}", keyOrUI, smallBodyLimit, s.patchServer)
	handle("DELETE /api/v1/servers/{id}", keyOrUI, smallBodyLimit, s.deleteServer)
	handle("POST /api/v1/servers/{id}/test", keyOrUI, smallBodyLimit, s.testServer)

	handle("GET /api/v1/settings", keyOrUI, smallBodyLimit, s.getSettings)
	handle("PUT /api/v1/settings", keyOrUI, defaultBodyLimit, s.putSettings)

	// A VPN's port-forwarding hook sets the port with the app key.
	handle("GET /api/v1/torrent/port", keyOrUI, smallBodyLimit, s.getTorrentPort)
	handle("POST /api/v1/torrent/port", keyOrUI, smallBodyLimit, s.setTorrentPort)

	// The event stream bounds its own writes instead of using the server's
	// write timeout.
	handle("GET /api/v1/events", keyOrUI, smallBodyLimit, s.events)

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
