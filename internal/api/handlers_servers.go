package api

import (
	"net/http"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// serverBody is the request body to create or test a Server. Fields left
// out take the defaults of app.ServerSpec. The password only ever travels
// inwards.
type serverBody struct {
	Name           string      `json:"name"`
	Host           string      `json:"host"`
	Port           int         `json:"port"`
	Username       string      `json:"username"`
	Password       core.Secret `json:"password"`
	MaxConnections int         `json:"max_connections"`
	RetentionDays  int         `json:"retention_days"`
	Priority       int         `json:"priority"`
	Enabled        *bool       `json:"enabled"`
}

func (b serverBody) spec() app.ServerSpec {
	return app.ServerSpec{
		Name: b.Name, Host: b.Host, Port: b.Port, Username: b.Username, Password: b.Password,
		MaxConnections: b.MaxConnections, RetentionDays: b.RetentionDays, Priority: b.Priority,
		Enabled: b.Enabled,
	}
}

func (s *Server) listServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.Servers.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]serverJSON, 0, len(servers))
	for _, srv := range servers {
		items = append(items, toServerJSON(srv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getServer(w http.ResponseWriter, r *http.Request) {
	srv, err := s.Servers.Get(r.Context(), core.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toServerJSON(srv))
}

func (s *Server) createServer(w http.ResponseWriter, r *http.Request) {
	var body serverBody
	if !decodeJSON(w, r, &body) {
		return
	}
	srv, err := s.Servers.Create(r.Context(), body.spec())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toServerJSON(srv))
}

func (s *Server) patchServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           *string      `json:"name"`
		Host           *string      `json:"host"`
		Port           *int         `json:"port"`
		Username       *string      `json:"username"`
		Password       *core.Secret `json:"password"`
		MaxConnections *int         `json:"max_connections"`
		RetentionDays  *int         `json:"retention_days"`
		Priority       *int         `json:"priority"`
		Enabled        *bool        `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	srv, err := s.Servers.Update(r.Context(), core.ID(r.PathValue("id")), app.ServerPatch{
		Name: body.Name, Host: body.Host, Port: body.Port, Username: body.Username, Password: body.Password,
		MaxConnections: body.MaxConnections, RetentionDays: body.RetentionDays, Priority: body.Priority,
		Enabled: body.Enabled,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toServerJSON(srv))
}

func (s *Server) deleteServer(w http.ResponseWriter, r *http.Request) {
	if err := s.Servers.Delete(r.Context(), core.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testServer(w http.ResponseWriter, r *http.Request) {
	res, err := s.Servers.Test(r.Context(), core.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toProbeJSON(res))
}

func (s *Server) testServerSpec(w http.ResponseWriter, r *http.Request) {
	var body serverBody
	if !decodeJSON(w, r, &body) {
		return
	}
	res, err := s.Servers.TestSpec(r.Context(), body.spec())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toProbeJSON(res))
}
