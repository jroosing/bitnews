package api

import (
	"net/http"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
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
		Name       string `json:"name"`
		UsenetDir  string `json:"usenet_dir"`
		TorrentDir string `json:"torrent_dir"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	c, err := s.Categories.Create(r.Context(), body.Name, body.UsenetDir, body.TorrentDir)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toCategoryJSON(c))
}

func (s *Server) patchCategory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       *string        `json:"name"`
		UsenetDir  nullableString `json:"usenet_dir"`
		TorrentDir nullableString `json:"torrent_dir"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	p := app.CategoryPatch{Name: body.Name}
	if body.UsenetDir.Set {
		p.UsenetDir = &body.UsenetDir.Value
	}
	if body.TorrentDir.Set {
		p.TorrentDir = &body.TorrentDir.Value
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
