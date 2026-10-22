package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// listParams are the query parameters of GET /api/v1/downloads.
var listParams = map[string]bool{"state": true, "category_id": true, "kind": true, "after": true, "limit": true}

// rejectUnknownParams answers 400 for a query parameter outside allowed, so
// a typo such as "stat=ready" does not silently return everything.
func rejectUnknownParams(w http.ResponseWriter, q url.Values, allowed map[string]bool) bool {
	for name := range q {
		if !allowed[name] {
			writeError(w, http.StatusBadRequest, "unknown_parameter", "unknown query parameter "+strconv.Quote(name))
			return false
		}
	}
	return true
}

func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !rejectUnknownParams(w, q, listParams) {
		return
	}
	f := app.ListFilter{
		State:      core.State(q.Get("state")),
		CategoryID: core.ID(q.Get("category_id")),
		Kind:       core.SourceKind(q.Get("kind")),
		After:      q.Get("after"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		f.Limit = n
	}
	page, err := s.Downloads.List(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := struct {
		Items []downloadJSON `json:"items"`
		Next  *string        `json:"next"`
	}{Items: make([]downloadJSON, 0, len(page.Items))}
	for _, d := range page.Items {
		out.Items = append(out.Items, toDownloadJSON(d))
	}
	if page.Next != "" {
		out.Next = &page.Next
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDownload(w http.ResponseWriter, r *http.Request) {
	d, err := s.Downloads.Get(r.Context(), core.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDownloadJSON(d))
}

func (s *Server) pauseDownload(w http.ResponseWriter, r *http.Request) {
	d, err := s.Downloads.Pause(r.Context(), core.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDownloadJSON(d))
}

func (s *Server) resumeDownload(w http.ResponseWriter, r *http.Request) {
	d, err := s.Downloads.Resume(r.Context(), core.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDownloadJSON(d))
}

// nullableString is a JSON string field that tells "absent" from "null": Set
// is true when the field was present, and Value is empty for null.
type nullableString struct {
	Set   bool
	Value string
}

func (n *nullableString) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(b, []byte("null")) {
		n.Value = ""
		return nil
	}
	return json.Unmarshal(b, &n.Value)
}

func (s *Server) patchDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Priority      *core.DownloadPriority `json:"priority"`
		CategoryID    nullableString         `json:"category_id"`
		QueuePosition *struct {
			Move app.Move `json:"move"`
		} `json:"queue_position"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	p := app.Patch{Priority: body.Priority}
	if body.CategoryID.Set {
		p.Category = app.SetCategory(core.ID(body.CategoryID.Value))
	}
	if body.QueuePosition != nil {
		p.Move = body.QueuePosition.Move
		if p.Move == "" {
			writeError(w, http.StatusBadRequest, "invalid_move", `queue_position needs a "move": top, bottom, up or down`)
			return
		}
	}
	d, err := s.Downloads.Update(r.Context(), core.ID(r.PathValue("id")), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDownloadJSON(d))
}

func (s *Server) deleteDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !rejectUnknownParams(w, q, map[string]bool{"delete_data": true}) {
		return
	}
	deleteData := false
	if raw := q.Get("delete_data"); raw != "" {
		var err error
		if deleteData, err = strconv.ParseBool(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_delete_data", "delete_data must be true or false")
			return
		}
	}
	if err := s.Downloads.Delete(r.Context(), core.ID(r.PathValue("id")), deleteData); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
