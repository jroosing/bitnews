package api

import (
	"net/http"
)

type middleware = func(http.Handler) http.Handler

// routes registers the M1 routes. Authentication per route:
//
//   - health: none;
//   - data routes: the app key (full access) and nothing else: the browser
//     UI has its own routes (internal/web), so a session cookie, LAN bypass
//     or disable auth never open the API;
//   - /sabnzbd/api and /api/v2/: the adapter's own check (the apikey, or the
//     SID session of a qBittorrent login), nothing else.
//
// The key is managed from the UI only: the API has no key endpoints.
func (s *Server) routes() {
	// One key with full access: reading and writing routes authenticate alike.
	key := s.AppKey.KeyOr(func(http.Handler) http.Handler { return keyRequired })

	handle := func(pattern string, wrap middleware, limit int64, h http.HandlerFunc) {
		s.mux.Handle(pattern, maxBody(limit, wrap(h)))
	}
	none := func(h http.Handler) http.Handler { return h }

	handle("GET /api/v1/health", none, smallBodyLimit, s.health)
	handle("GET /api/v1/version", key, smallBodyLimit, s.version)

	handle("GET /api/v1/downloads", key, smallBodyLimit, s.listDownloads)
	handle("GET /api/v1/downloads/{id}", key, smallBodyLimit, s.getDownload)
	// Uploads are streamed, so this route's cap is the NZB limit, not 1 MiB.
	handle("POST /api/v1/downloads", key, addBodyLimit, s.addDownload)
	handle("POST /api/v1/downloads/{id}/pause", key, smallBodyLimit, s.pauseDownload)
	handle("POST /api/v1/downloads/{id}/resume", key, smallBodyLimit, s.resumeDownload)
	handle("PATCH /api/v1/downloads/{id}", key, smallBodyLimit, s.patchDownload)
	handle("DELETE /api/v1/downloads/{id}", key, smallBodyLimit, s.deleteDownload)

	handle("GET /api/v1/categories", key, smallBodyLimit, s.listCategories)
	handle("POST /api/v1/categories", key, smallBodyLimit, s.createCategory)
	handle("PATCH /api/v1/categories/{id}", key, smallBodyLimit, s.patchCategory)
	handle("DELETE /api/v1/categories/{id}", key, smallBodyLimit, s.deleteCategory)

	handle("GET /api/v1/servers", key, smallBodyLimit, s.listServers)
	handle("POST /api/v1/servers", key, smallBodyLimit, s.createServer)
	handle("POST /api/v1/servers/test", key, smallBodyLimit, s.testServerSpec)
	handle("GET /api/v1/servers/{id}", key, smallBodyLimit, s.getServer)
	handle("PATCH /api/v1/servers/{id}", key, smallBodyLimit, s.patchServer)
	handle("DELETE /api/v1/servers/{id}", key, smallBodyLimit, s.deleteServer)
	handle("POST /api/v1/servers/{id}/test", key, smallBodyLimit, s.testServer)

	handle("GET /api/v1/settings", key, smallBodyLimit, s.getSettings)
	handle("PUT /api/v1/settings", key, defaultBodyLimit, s.putSettings)

	// A VPN's port-forwarding hook sets the port with the app key.
	handle("GET /api/v1/torrent/port", key, smallBodyLimit, s.getTorrentPort)
	handle("POST /api/v1/torrent/port", key, smallBodyLimit, s.setTorrentPort)

	// The event stream bounds its own writes instead of using the server's
	// write timeout.
	handle("GET /api/v1/events", key, smallBodyLimit, s.events)

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

// keyRequired answers a request to a data route that carries no API key.
var keyRequired = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
})
