package api

import "net/http"

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version":    s.Version.Version,
		"commit":     s.Version.Commit,
		"go_version": s.Version.GoVersion,
	})
}
