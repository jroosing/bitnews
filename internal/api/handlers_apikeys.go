package api

import (
	"errors"
	"net/http"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.APIKeys.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": keys})
}

// createAPIKey answers with the key's data and, this once, its plaintext.
func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string           `json:"name"`
		Scope core.APIKeyScope `json:"scope"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	plaintext, info, err := s.APIKeys.Create(r.Context(), body.Name, body.Scope)
	switch {
	case errors.Is(err, auth.ErrInvalidAPIKeyName):
		writeError(w, http.StatusBadRequest, "invalid_name", "the name must be 1 to 100 characters without control characters")
		return
	case errors.Is(err, auth.ErrInvalidAPIKeyScope):
		writeError(w, http.StatusBadRequest, "invalid_scope", "the scope must be read or write")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		auth.APIKeyInfo
		Key string `json:"key"`
	}{APIKeyInfo: info, Key: plaintext})
}

func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := s.APIKeys.Revoke(r.Context(), core.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
