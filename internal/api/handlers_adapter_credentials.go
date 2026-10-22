package api

import (
	"errors"
	"net/http"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func (s *Server) listAdapterCredentials(w http.ResponseWriter, r *http.Request) {
	items, err := s.AdapterCredentials.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// createAdapterCredential answers with the credential's data and, this once,
// its plaintext key. For adapter_qbit the key is the password to configure
// next to the username.
func (s *Server) createAdapterCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind     auth.TokenKind `json:"kind"`
		Name     string         `json:"name"`
		Username string         `json:"username"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	plaintext, info, err := s.AdapterCredentials.Create(r.Context(), body.Kind, body.Name, body.Username)
	switch {
	case errors.Is(err, auth.ErrInvalidAdapterKind):
		writeError(w, http.StatusBadRequest, "invalid_kind", "the kind must be adapter_sab or adapter_qbit")
		return
	case errors.Is(err, auth.ErrInvalidAdapterName):
		writeError(w, http.StatusBadRequest, "invalid_name", "the name must be 1 to 100 characters without control characters")
		return
	case errors.Is(err, auth.ErrInvalidAdapterUsername):
		writeError(w, http.StatusBadRequest, "invalid_username", "the username must be 1 to 32 characters of a-z, 0-9, '_' and '-'")
		return
	case errors.Is(err, auth.ErrAdapterUsernameTaken):
		writeError(w, http.StatusConflict, "username_taken", "another credential already uses this username")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		auth.AdapterCredentialInfo
		Key string `json:"key"`
	}{AdapterCredentialInfo: info, Key: plaintext})
}

func (s *Server) deleteAdapterCredential(w http.ResponseWriter, r *http.Request) {
	if err := s.AdapterCredentials.Revoke(r.Context(), core.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
