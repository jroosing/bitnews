package api

import (
	"net/http"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func (s *Server) listCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.Categories.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]categoryJSON, 0, len(cats))
	for _, c := range cats {
		items = append(items, toCategoryJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createCategory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		DirOverride string `json:"dir_override"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	c, err := s.Categories.Create(r.Context(), body.Name, body.DirOverride)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toCategoryJSON(c))
}

func (s *Server) patchCategory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        *string        `json:"name"`
		DirOverride nullableString `json:"dir_override"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	p := app.CategoryPatch{Name: body.Name}
	if body.DirOverride.Set {
		p.DirOverride = &body.DirOverride.Value
	}
	c, err := s.Categories.Update(r.Context(), core.ID(r.PathValue("id")), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toCategoryJSON(c))
}

func (s *Server) deleteCategory(w http.ResponseWriter, r *http.Request) {
	if err := s.Categories.Delete(r.Context(), core.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
