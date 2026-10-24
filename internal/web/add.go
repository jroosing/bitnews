package web

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strings"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

const (
	// addBodyLimit caps POST /downloads/add like the API caps POST
	// /api/v1/downloads: an NZB of up to 64 MiB plus the multipart framing
	// and the form fields.
	addBodyLimit = app.DefaultMaxNZBBytes + 1<<20
	// addMaxFieldBytes caps each text field of the form, and addMaxParts the
	// parts of the form, so a flood of tiny parts cannot keep the handler busy.
	addMaxFieldBytes = 4096
	addMaxParts      = 16
	// addMaxTorrentBytes is the limit shown to people for a .torrent file.
	addMaxTorrentBytes = core.MaxMetainfoSize
)

// addForm is the "Add download" form with its values and problems. It is
// also what the fragment renders after a successful add (an empty form).
type addForm struct {
	// Mode is the tab: "file" or "link".
	Mode     string
	Link     string
	Name     string
	Category string
	Priority string
	Paused   bool

	Categories []option
	Priorities []option

	// Problems. Error is the general one; the others belong to a field.
	Error         string
	SourceError   string
	NameError     string
	CategoryError string
	PriorityError string
	PassError     string
	// Duplicate is set when the torrent is already a Download.
	DuplicateID   string
	DuplicateName string
	// Unavailable is set when the form cannot be used at all.
	Unavailable string

	// Hint says which files are accepted.
	Hint            string
	MaxNZBBytes     int64
	MaxTorrentBytes int64
}

func (f addForm) HasError() bool {
	return f.Error != "" || f.SourceError != "" || f.NameError != "" || f.CategoryError != "" ||
		f.PriorityError != "" || f.PassError != "" || f.DuplicateID != ""
}

// newAddForm is the blank form.
func (s *Server) newAddForm(ctx context.Context) addForm {
	f := addForm{
		Mode: "file", Priority: string(core.DownloadPriorityNormal),
		Priorities: []option{
			{string(core.DownloadPriorityNormal), "Normal"},
			{string(core.DownloadPriorityHigh), "High"},
			{string(core.DownloadPriorityLow), "Low"},
			{string(core.DownloadPriorityForce), "Force (ignore the queue limit)"},
		},
		Categories:      []option{{"", "No category"}},
		MaxNZBBytes:     app.DefaultMaxNZBBytes,
		MaxTorrentBytes: addMaxTorrentBytes,
		Hint:            "NZB files up to " + histBytes(app.DefaultMaxNZBBytes) + ", torrent files up to " + histBytes(addMaxTorrentBytes),
	}
	if s.Categories != nil {
		if cats, err := s.Categories.List(ctx); err == nil {
			for _, c := range cats {
				f.Categories = append(f.Categories, option{string(c.ID), histClean(c.Name, 100)})
			}
		} else {
			s.Logger.Error("add dialog: list categories failed", "err", err)
		}
	}
	if s.Adder == nil {
		f.Unavailable = "Adding downloads is not available: the add service is not running."
	}
	return f
}

// getAdd serves the dialog's form. It is only meant for htmx; a browser
// that opens the address gets sent to the queue.
func (s *Server) getAdd(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Redirect(w, r, "/queue", http.StatusSeeOther)
		return
	}
	s.renderAddForm(w, r, http.StatusOK, s.newAddForm(r.Context()))
}

func (s *Server) renderAddForm(w http.ResponseWriter, r *http.Request, status int, f addForm) {
	v := s.newView(r, "")
	v.Page = f
	// The form is a component, which every page's template set has.
	s.renderFragment(w, r, status, "history.html", "add_form", v)
}

// addInput is what the form posted.
type addInput struct {
	link     string
	name     string
	category string
	priority string
	password string
	paused   bool
	mode     string
	fileName string
	upload   *app.Upload
}

func (in *addInput) discard() { in.upload.Discard() }

// postAdd adds a Download from an uploaded NZB or torrent file, or from a
// link (a magnet link, or the URL of an NZB or torrent), mirroring
// POST /api/v1/downloads.
func (s *Server) postAdd(w http.ResponseWriter, r *http.Request) {
	form := s.newAddForm(r.Context())
	if s.Adder == nil {
		s.renderAddForm(w, r, http.StatusServiceUnavailable, form)
		return
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		form.Error = "The form was not sent as a file upload. Reload the page and try again."
		s.renderAddForm(w, r, http.StatusUnsupportedMediaType, form)
		return
	}
	in, err := s.readAddForm(r)
	defer in.discard()
	form.Link, form.Name, form.Category, form.Paused = in.link, in.name, in.category, in.paused
	if in.priority != "" {
		form.Priority = in.priority
	}
	if in.mode == "link" {
		form.Mode = "link"
	}
	if err != nil {
		s.renderAddForm(w, r, s.addProblem(&form, err), form)
		return
	}

	req := app.AddRequest{
		Name: in.name, FileName: in.fileName, CategoryID: core.ID(in.category),
		Priority: core.DownloadPriority(in.priority), Password: core.Secret(in.password), Paused: in.paused,
	}
	d, err := s.addDownload(r.Context(), in, req)
	if err != nil {
		s.renderAddForm(w, r, s.addProblem(&form, err), form)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/downloads/"+string(d.ID), http.StatusSeeOther)
		return
	}
	detailTrigger(w, "success", "Added “"+histClean(d.Name, 80)+"”", map[string]any{
		"download-added": map[string]string{"id": string(d.ID)},
		// The queue page refetches itself on this event.
		"queue-refresh": true,
	})
	s.renderAddForm(w, r, http.StatusOK, s.newAddForm(r.Context()))
}

// errAddRequest is an error of the request itself, with the status to send.
type errAddRequest struct {
	msg   string
	field string
}

func (e *errAddRequest) Error() string { return e.msg }

// readAddForm reads the multipart body. A file part is staged as it arrives
// (it can come before the fields that describe it); the caller discards
// whatever is still staged.
func (s *Server) readAddForm(r *http.Request) (*addInput, error) {
	in := &addInput{}
	mr, err := r.MultipartReader()
	if err != nil {
		return in, &errAddRequest{msg: "The upload could not be read. Reload the page and try again."}
	}
	seen := map[string]bool{}
	for n := 0; ; n++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return in, addReadError(err)
		}
		if n >= addMaxParts {
			return in, &errAddRequest{msg: "The form has too many parts."}
		}
		name := part.FormName()
		if seen[name] {
			return in, &errAddRequest{msg: "The form sent the field " + histClean(name, 40) + " twice."}
		}
		seen[name] = true
		if name == "file" {
			if err := s.stageUpload(r.Context(), in, part); err != nil {
				return in, err
			}
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(part, addMaxFieldBytes+1))
		if err != nil {
			return in, addReadError(err)
		}
		if len(raw) > addMaxFieldBytes {
			return in, &errAddRequest{msg: "The field " + histClean(name, 40) + " is too long."}
		}
		value := string(raw)
		switch name {
		case "link":
			in.link = strings.TrimSpace(value)
		case "name":
			in.name = strings.TrimSpace(value)
		case "category_id":
			in.category = strings.TrimSpace(value)
		case "priority":
			in.priority = strings.TrimSpace(value)
		case "password":
			in.password = value
		case "paused":
			in.paused = value != "" && value != "0" && value != "false"
		case "mode":
			in.mode = value
		default:
			return in, &errAddRequest{msg: "The form sent an unknown field."}
		}
	}
	return in, nil
}

func addReadError(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return err
	}
	return &errAddRequest{msg: "The upload could not be read. Reload the page and try again."}
}

// stageUpload receives the "file" part. The AddService decides by the
// content whether it is an NZB or a torrent, and checks that the file
// extension, when it is one of ours, agrees.
func (s *Server) stageUpload(ctx context.Context, in *addInput, part *multipart.Part) error {
	if part.FileName() == "" {
		// A browser sends an empty file part when no file was chosen.
		return nil
	}
	if in.upload != nil {
		return &errAddRequest{msg: "Choose one file.", field: "source"}
	}
	in.fileName = path.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
	up, err := s.Adder.Receive(ctx, part, in.fileName, "")
	if err != nil {
		if _, ok := errors.AsType[*app.Error](err); !ok {
			return addReadError(err)
		}
		return err
	}
	in.upload = up
	return nil
}

// addDownload adds the received file or the link.
func (s *Server) addDownload(ctx context.Context, in *addInput, req app.AddRequest) (core.Download, error) {
	switch {
	case in.upload != nil && in.link != "":
		return core.Download{}, &errAddRequest{msg: "Choose a file or a link, not both.", field: "source"}
	case in.upload == nil && in.link == "":
		return core.Download{}, &errAddRequest{msg: "Choose an NZB or torrent file, or paste a link.", field: "source"}
	}
	req.Upload, req.URL = in.upload, in.link
	return s.Adder.Add(ctx, req)
}

// addProblem records err in the form and returns the status to answer with.
func (s *Server) addProblem(f *addForm, err error) int {
	if dup, ok := errors.AsType[app.ErrDuplicate](err); ok {
		if validDownloadID(string(dup.ExistingID)) {
			f.DuplicateID = string(dup.ExistingID)
			f.DuplicateName = histClean(dup.Name, 120)
		}
		f.SourceError = "This torrent is already in bitnews."
		return http.StatusConflict
	}
	if req, ok := errors.AsType[*errAddRequest](err); ok {
		if req.field == "source" {
			f.SourceError = req.msg
		} else {
			f.Error = req.msg
		}
		return http.StatusBadRequest
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		f.SourceError = "That upload is too large. NZB files can be up to " + histBytes(app.DefaultMaxNZBBytes) + ", torrent files up to " + histBytes(addMaxTorrentBytes) + "."
		return http.StatusRequestEntityTooLarge
	}
	if e, ok := errors.AsType[*app.Error](err); ok {
		msg := histTruncate(histClean(e.Message, 400), 300)
		status := http.StatusBadRequest
		switch {
		case errors.Is(e.Kind, app.ErrTooLarge):
			status = http.StatusRequestEntityTooLarge
		case errors.Is(e.Kind, app.ErrUpstream):
			status = http.StatusBadGateway
		case errors.Is(e.Kind, app.ErrUnavailable):
			status = http.StatusServiceUnavailable
		case errors.Is(e.Kind, core.ErrConflict):
			status = http.StatusConflict
		}
		switch e.Code {
		case "invalid_priority":
			f.PriorityError = "Choose one of the priorities in the list."
		case "unknown_category":
			f.CategoryError = "That category does not exist any more. Choose another one."
		case "invalid_password":
			f.PassError = msg
		case "torrents_unavailable":
			f.Error = "Torrents are not available on this instance, so only NZB files can be added."
		case "nzb_too_large", "torrent_too_large", "invalid_nzb", "invalid_torrent", "invalid_magnet", "invalid_url", "fetch_failed",
			"empty_file", "not_a_torrent", "not_an_nzb":
			f.SourceError = msg
		default:
			f.Error = msg
		}
		return status
	}
	s.Logger.Error("add download failed", "err", err)
	f.Error = "bitnews could not add the download. Details are in the server log."
	return http.StatusInternalServerError
}
