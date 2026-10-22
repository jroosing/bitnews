package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// writeJSON sends v as the response body. API responses are never cached,
// because they hold user data and session state.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorBody is the error shape of the whole API.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// fail answers with the status and code that match err. Errors the client
// can act on come from the app layer; anything else is a bug or an outage,
// logged with the request ID and reported generically.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if appErr, ok := errors.AsType[*app.Error](err); ok {
		writeError(w, statusOf(appErr.Kind), appErr.Code, appErr.Message)
		return
	}
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, core.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "the request conflicts with the current state")
	default:
		logFrom(r.Context()).Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func statusOf(kind error) int {
	switch {
	case errors.Is(kind, app.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(kind, core.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(kind, core.ErrConflict):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// Body size limits. A route that needs more or less than the default says
// so when it is registered.
const (
	defaultBodyLimit = 1 << 20
	smallBodyLimit   = 16 << 10
)

// decodeJSON reads the request body into dst. It is strict: the body must be
// one JSON value of the expected shape, with no unknown fields and nothing
// after it. On failure it writes the error response and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send the body as Content-Type: application/json")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		if _, extra := dec.Token(); !errors.Is(extra, io.EOF) {
			err = errors.New("unexpected data after the JSON value")
		}
	}
	if err == nil {
		return true
	}
	switch {
	case isTooLarge(err):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
	case errors.Is(err, io.EOF):
		writeError(w, http.StatusBadRequest, "invalid_json", "the request body is empty")
	default:
		writeError(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid request body: %v", err))
	}
	return false
}

func isTooLarge(err error) bool {
	_, ok := errors.AsType[*http.MaxBytesError](err)
	return ok
}

// maxBody caps the request body of a route.
func maxBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next.ServeHTTP(w, r)
	})
}
