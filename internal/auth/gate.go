package auth

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
)

// Routes the gate lets through while setup is open.
const (
	setupPath  = "/api/v1/setup"
	healthPath = "/api/v1/health"
	staticPath = "/static/"

	// The web UI's own first-run pages. It answers GET / and GET /login with
	// a redirect to /setup, serves the form on GET /setup and takes its
	// submission on POST /setup.
	rootPath      = "/"
	loginPagePath = "/login"
	setupPagePath = "/setup"
)

// Gate wraps next so that, while first-run setup is open, every request gets
// 503 setup_required except POST /api/v1/setup, GET /api/v1/health, static
// assets and the web UI's setup pages (GET / and GET /login, which redirect
// to /setup, and GET and POST /setup). Once setup is done it costs one atomic load per request and never
// queries the database.
func Gate(next http.Handler, setup *Setup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !setup.Required() || setupExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "setup_required",
			"first-run setup is required: create the admin account with the setup token from the server log")
	})
}

func setupExempt(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == setupPath:
		return r.Method == http.MethodPost
	case p == healthPath:
		return r.Method == http.MethodGet
	case p == rootPath, p == loginPagePath:
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	case p == setupPagePath:
		return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodPost
	case strings.HasPrefix(p, staticPath):
		// A path with dot segments or doubled slashes could climb out of
		// /static/ in a router that cleans it afterwards.
		return canonicalPath(p)
	}
	return false
}

func canonicalPath(p string) bool {
	return path.Clean(p) == p || path.Clean(p)+"/" == p
}

// writeError writes the API's JSON error shape.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
