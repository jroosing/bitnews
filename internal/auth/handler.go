package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// maxSetupBody caps the POST /api/v1/setup body.
const maxSetupBody = 1 << 20

// SetupHandler serves POST /api/v1/setup. The body is
// {"token","username","password"}. It answers 201 on success, 400 for
// invalid input, 403 for a wrong or missing token, 409 once setup is closed
// and 413 for an oversized body. It never echoes the password or the token.
func SetupHandler(setup *Setup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			return
		}
		var req struct {
			Token    string      `json:"token"`
			Username string      `json:"username"`
			Password core.Secret `json:"password"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSetupBody))
		dec.DisallowUnknownFields()
		err := dec.Decode(&req)
		if err == nil {
			// Exactly one JSON value: anything after it is a malformed request.
			if _, extra := dec.Token(); !errors.Is(extra, io.EOF) {
				err = errors.New("trailing data")
			}
		}
		if err != nil {
			if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
				writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request", "body must be a JSON object with token, username and password")
			return
		}

		err = setup.Complete(r.Context(), req.Token, req.Username, req.Password)
		switch {
		case err == nil:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"username": req.Username})
		case errors.Is(err, ErrBadSetupToken):
			writeError(w, http.StatusForbidden, "invalid_setup_token", "the setup token is missing or wrong")
		case errors.Is(err, ErrSetupClosed):
			writeError(w, http.StatusConflict, "setup_closed", "setup is already complete")
		case errors.Is(err, ErrInvalidUsername):
			writeError(w, http.StatusBadRequest, "invalid_username", err.Error())
		case errors.Is(err, ErrPasswordTooShort), errors.Is(err, ErrPasswordTooLong), errors.Is(err, ErrPasswordInvalid):
			writeError(w, http.StatusBadRequest, "invalid_password", err.Error())
		default:
			setup.log.Error("setup failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
	})
}
