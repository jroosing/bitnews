package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// The queue's action routes. Each answers htmx with the HTML to swap in:
// the updated row for an action on one Download that keeps its place, the
// whole lists when the order or the set of rows changed. An error becomes a
// toast (HX-Trigger) with the matching status; htmx does not swap those. A
// plain form post is redirected back to the page.

// queueOutcome is what a successful action reports.
type queueOutcome struct {
	// lists says the action changed the order or the rows, so the answer is
	// the whole lists instead of one row.
	lists bool
	// toast is shown to the user when not empty.
	toast string
}

// queueOp performs one action on the Download id with the posted form.
type queueOp func(ctx context.Context, id core.ID, form url.Values) (queueOutcome, error)

func (s *Server) registerQueueActions() {
	s.handleAuthed("POST /queue/{id}/pause", s.queueAction(s.opPause))
	s.handleAuthed("POST /queue/{id}/resume", s.queueAction(s.opResume))
	s.handleAuthed("POST /queue/{id}/priority", s.queueAction(s.opPriority))
	s.handleAuthed("POST /queue/{id}/move", s.queueAction(s.opMove))
	s.handleAuthed("POST /queue/{id}/category", s.queueAction(s.opCategory))
	s.handleAuthed("POST /queue/{id}/remove", s.queueAction(s.opRemove))
	s.handleAuthed("POST /queues/{kind}/pause", s.queueControl(s.opPauseQueue))
	s.handleAuthed("POST /queues/{kind}/resume", s.queueControl(s.opResumeQueue))
	s.handleAuthed("POST /queues/{kind}/speed", s.queueControl(s.opSpeed))
}

func (s *Server) opPause(ctx context.Context, id core.ID, _ url.Values) (queueOutcome, error) {
	_, err := s.Downloads.Pause(ctx, id)
	return queueOutcome{}, err
}

func (s *Server) opResume(ctx context.Context, id core.ID, _ url.Values) (queueOutcome, error) {
	_, err := s.Downloads.Resume(ctx, id)
	return queueOutcome{}, err
}

func (s *Server) opPriority(ctx context.Context, id core.ID, form url.Values) (queueOutcome, error) {
	p := core.DownloadPriority(form.Get("priority"))
	_, err := s.Downloads.Update(ctx, id, app.Patch{Priority: &p})
	return queueOutcome{}, err
}

func (s *Server) opMove(ctx context.Context, id core.ID, form url.Values) (queueOutcome, error) {
	_, err := s.Downloads.Update(ctx, id, app.Patch{Move: app.Move(form.Get("to"))})
	return queueOutcome{lists: true}, err
}

func (s *Server) opCategory(ctx context.Context, id core.ID, form url.Values) (queueOutcome, error) {
	cat := core.ID(form.Get("category_id"))
	if cat == "none" {
		cat = ""
	}
	_, err := s.Downloads.Update(ctx, id, app.Patch{Category: app.SetCategory(cat)})
	return queueOutcome{}, err
}

func (s *Server) opRemove(ctx context.Context, id core.ID, form url.Values) (queueOutcome, error) {
	deleteData := form.Get("delete_data") == "true"
	if err := s.Downloads.Delete(ctx, id, deleteData); err != nil {
		return queueOutcome{}, err
	}
	msg := "Removed. Downloaded files were kept."
	if deleteData {
		msg = "Removed with its files."
	}
	return queueOutcome{lists: true, toast: msg}, nil
}

// queueAction wraps an action on one Download.
func (s *Server) queueAction(op queueOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := core.ID(r.PathValue("id"))
		if s.Downloads == nil || !idValue.MatchString(string(id)) {
			s.queueError(w, r, core.ErrNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			s.queueError(w, r, invalidForm())
			return
		}
		out, err := op(r.Context(), id, r.PostForm)
		if err != nil {
			s.queueError(w, r, err)
			return
		}
		if !isHTMX(r) {
			s.redirect(w, r, "/queue")
			return
		}
		if out.lists {
			s.answerLists(w, r, out.toast)
			return
		}
		s.answerRow(w, r, id)
	}
}

// answerLists answers an action with the whole lists.
func (s *Server) answerLists(w http.ResponseWriter, r *http.Request, toast string) {
	if toast != "" {
		setTrigger(w, map[string]any{"toast": toastData("success", toast)})
	}
	s.renderQueueLists(w, r, http.StatusOK)
}

// answerRow answers an action with the row of the Download. A Download that
// is not in the queue any more (it finished meanwhile) answers an empty row,
// which removes it, and asks for a refresh of the lists.
func (s *Server) answerRow(w http.ResponseWriter, r *http.Request, id core.ID) {
	v, err := s.loadQueue(r.Context(), csrfOf(r))
	if err != nil {
		s.Logger.Error("queue row failed", "err", err)
		s.queueError(w, r, err)
		return
	}
	for _, sec := range v.Sections {
		for _, row := range sec.Rows {
			if core.ID(row.ID) == id {
				pv := s.newView(r, "Queue")
				pv.Page = row
				s.renderFragment(w, r, http.StatusOK, "queue.html", "queue_row_fragment", pv)
				return
			}
		}
	}
	setTrigger(w, map[string]any{"queue-refresh": true})
	setHTMLHeaders(w)
	w.WriteHeader(http.StatusOK)
}

// ---- whole-Queue controls -----------------------------------------------

// queueControlOp acts on the Queue of one Source kind.
type queueControlOp func(kind core.SourceKind, form url.Values) (toast string, err error)

func (s *Server) queueControl(op queueControlOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var kind core.SourceKind
		switch r.PathValue("kind") {
		case "usenet":
			kind = core.SourceNZB
		case "torrent":
			kind = core.SourceTorrent
		default:
			s.queueError(w, r, core.ErrNotFound)
			return
		}
		if s.Runtime == nil || !s.Runtime.QueueState(kind).Available {
			s.queueError(w, r, &app.Error{Kind: core.ErrConflict, Code: "queue_unavailable", Message: "That queue is not available on this instance."})
			return
		}
		if err := r.ParseForm(); err != nil {
			s.queueError(w, r, invalidForm())
			return
		}
		toast, err := op(kind, r.PostForm)
		if err != nil {
			s.queueError(w, r, err)
			return
		}
		if !isHTMX(r) {
			s.redirect(w, r, "/queue")
			return
		}
		s.answerLists(w, r, toast)
	}
}

func queueName(kind core.SourceKind) string {
	if kind == core.SourceTorrent {
		return "Torrent"
	}
	return "Usenet"
}

func (s *Server) opPauseQueue(kind core.SourceKind, _ url.Values) (string, error) {
	s.Runtime.PauseQueue(kind)
	return queueName(kind) + " queue paused.", nil
}

func (s *Server) opResumeQueue(kind core.SourceKind, _ url.Values) (string, error) {
	s.Runtime.ResumeQueue(kind)
	return queueName(kind) + " queue resumed.", nil
}

// maxLimitKBps bounds a speed limit (1 TB/s is far beyond any link).
const maxLimitKBps = 1 << 30

func (s *Server) opSpeed(kind core.SourceKind, form url.Values) (string, error) {
	down, err := parseKBps(form.Get("down_kbps"))
	if err != nil {
		return "", err
	}
	var up int64
	if kind == core.SourceTorrent {
		if up, err = parseKBps(form.Get("up_kbps")); err != nil {
			return "", err
		}
	}
	s.Runtime.SetSpeedLimit(kind, down*1024, up*1024)
	if down == 0 && up == 0 {
		return queueName(kind) + " speed limit removed.", nil
	}
	return queueName(kind) + " speed limit set. It applies until bitnews restarts.", nil
}

// parseKBps reads a limit in KB/s; empty means unlimited.
func parseKBps(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > maxLimitKBps {
		return 0, &app.Error{Kind: app.ErrInvalidInput, Code: "invalid_limit", Message: "A speed limit is a whole number of KB/s, 0 or more. Use 0 for no limit."}
	}
	return n, nil
}

func invalidForm() error {
	return &app.Error{Kind: app.ErrInvalidInput, Code: "invalid_form", Message: "The form could not be read."}
}

// ---- errors and toasts --------------------------------------------------

func toastData(level, message string) map[string]string {
	return map[string]string{"level": level, "message": message}
}

// setTrigger sets the HX-Trigger header: events htmx fires on the page.
func setTrigger(w http.ResponseWriter, events map[string]any) {
	b, err := json.Marshal(events)
	if err != nil {
		return
	}
	w.Header().Set("HX-Trigger", string(b))
}

// queueError answers a failed action. The message shown comes from the
// services' own errors, which are written for users; anything else is a
// generic message and a log line. The page is asked to refresh, because the
// row the user clicked may be out of date.
func (s *Server) queueError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		status int
		msg    string
		ae     *app.Error
	)
	switch {
	case errors.Is(err, core.ErrNotFound):
		status, msg = http.StatusNotFound, "That download no longer exists."
	case errors.As(err, &ae) && errors.Is(err, core.ErrConflict):
		status, msg = http.StatusConflict, ae.Message
	case errors.As(err, &ae) && errors.Is(err, app.ErrInvalidInput):
		status, msg = http.StatusBadRequest, ae.Message
	default:
		s.Logger.Error("queue action failed", "path", r.URL.Path, "err", err)
		status, msg = http.StatusInternalServerError, "That did not work. Details are in the server log."
	}
	if isHTMX(r) {
		setTrigger(w, map[string]any{"toast": toastData("danger", msg), "queue-refresh": true})
		setHTMLHeaders(w)
		w.WriteHeader(status)
		return
	}
	s.renderError(w, r, status, "Could not do that", fmt.Sprint(msg))
}
