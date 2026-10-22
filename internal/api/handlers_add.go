package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

const (
	// maxFormFieldBytes caps each text field of an upload.
	maxFormFieldBytes = 4096
	// maxFormParts caps the parts of an upload, so a flood of tiny parts
	// cannot keep the handler busy.
	maxFormParts = 16
)

// addDownload serves POST /api/v1/downloads: an NZB as a multipart upload
// (part "nzb", plus the optional fields name, category_id, priority,
// password and paused) or as a URL in a JSON body. Other content types are
// refused with 415.
func (s *Server) addDownload(w http.ResponseWriter, r *http.Request) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case err == nil && mt == "multipart/form-data":
		s.addFromUpload(w, r)
	case err == nil && mt == "application/json":
		s.addFromURL(w, r)
	default:
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"send multipart/form-data with an nzb part, or application/json with a url")
	}
}

func (s *Server) addFromURL(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, smallBodyLimit)
	var body struct {
		URL        string                `json:"url"`
		Name       string                `json:"name"`
		CategoryID core.ID               `json:"category_id"`
		Priority   core.DownloadPriority `json:"priority"`
		Password   core.Secret           `json:"password"`
		Paused     bool                  `json:"paused"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.URL == "" {
		writeError(w, http.StatusBadRequest, "missing_url", "give the NZB's url")
		return
	}
	req := app.AddRequest{
		Name: body.Name, CategoryID: body.CategoryID, Priority: body.Priority,
		Password: body.Password, Paused: body.Paused,
	}
	// Check the cheap fields before fetching anything.
	if err := s.Adder.Check(r.Context(), req); err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.Adder.FetchNZB(r.Context(), body.URL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.finishAdd(w, r, st, req)
}

func (s *Server) addFromUpload(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_multipart", "the multipart body is not valid")
		return
	}
	var (
		req    app.AddRequest
		st     *app.StagedNZB
		seen   = map[string]bool{}
		fields = map[string]*string{}
		paused string
		prio   string
		cat    string
		pass   string
	)
	fields["name"], fields["category_id"], fields["priority"], fields["password"], fields["paused"] =
		&req.Name, &cat, &prio, &pass, &paused
	defer func() { st.Discard() }() // after finishAdd this does nothing

	for n := 0; ; n++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.uploadReadFailed(w, r, err)
			return
		}
		if n >= maxFormParts {
			writeError(w, http.StatusBadRequest, "too_many_parts", "the form has too many parts")
			return
		}
		name := part.FormName()
		if seen[name] {
			writeError(w, http.StatusBadRequest, "duplicate_field", "the form field "+strconv.Quote(name)+" is given twice")
			return
		}
		seen[name] = true
		if name == "nzb" {
			req.FileName = part.FileName()
			if st, err = s.Adder.Stage(r.Context(), part); err != nil {
				s.fail(w, r, err)
				return
			}
			continue
		}
		dst, ok := fields[name]
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown_field", "unknown form field "+strconv.Quote(name))
			return
		}
		raw, err := io.ReadAll(io.LimitReader(part, maxFormFieldBytes+1))
		if err != nil {
			s.uploadReadFailed(w, r, err)
			return
		}
		if len(raw) > maxFormFieldBytes {
			writeError(w, http.StatusBadRequest, "field_too_long", "the form field "+strconv.Quote(name)+" is too long")
			return
		}
		*dst = string(raw)
	}
	if st == nil {
		writeError(w, http.StatusBadRequest, "missing_nzb", `the form needs an "nzb" file part`)
		return
	}
	req.CategoryID, req.Priority, req.Password = core.ID(cat), core.DownloadPriority(prio), core.Secret(pass)
	if paused != "" {
		if req.Paused, err = strconv.ParseBool(paused); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_paused", "paused must be true or false")
			return
		}
	}
	s.finishAdd(w, r, st, req)
}

// uploadReadFailed answers for a multipart read error: too large when the
// body cap was hit, else a malformed form.
func (s *Server) uploadReadFailed(w http.ResponseWriter, r *http.Request, err error) {
	if isTooLarge(err) {
		s.fail(w, r, err)
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_multipart", "the multipart body is not valid")
}

func (s *Server) finishAdd(w http.ResponseWriter, r *http.Request, st *app.StagedNZB, req app.AddRequest) {
	d, err := s.Adder.Add(r.Context(), st, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDownloadJSON(d))
}
