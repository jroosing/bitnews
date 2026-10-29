package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// seedState is what a Ready torrent is doing.
type seedState string

const (
	seedSeeding seedState = "seeding"
	// seedPaused is Seeding paused: the user stopped it (ADR-0012).
	seedPaused seedState = "paused"
	// seedLimit is a torrent its Share Limit stopped.
	seedLimit seedState = "limit"
)

// seedView is the seeding part of a History row.
type seedView struct {
	State seedState
	Label string
	// Rate is the upload speed, only while seeding.
	Rate string
	// Line is the ratio and time against the Share Limit.
	Line string
	// LineTitle says where the Share Limit comes from.
	LineTitle string
}

// CanPause and CanResume say which button the row has. A torrent its Share
// Limit stopped has none: the limit is changed on the detail page.
func (s *seedView) CanPause() bool  { return s != nil && s.State == seedSeeding }
func (s *seedView) CanResume() bool { return s != nil && s.State == seedPaused }

// seedOf describes the Seeding of a Ready torrent, nil for anything else
// and for a torrent the engine does not have (its data is gone).
func seedOf(v view.View, def core.SeedingSettings) *seedView {
	d, t := v.Download, v.Torrent
	if d.SourceKind != core.SourceTorrent || d.State != core.StateReady || t == nil || !t.Registered {
		return nil
	}
	out := &seedView{Line: shareProgress(t, def)}
	if t.ShareLimit.Global {
		out.LineTitle = "Share Limit from the Settings"
	} else {
		out.LineTitle = "Share Limit of this download"
	}
	switch {
	case t.LimitPaused:
		out.State, out.Label = seedLimit, "Share Limit reached"
	case d.Paused:
		out.State, out.Label = seedPaused, "Seeding paused"
	default:
		out.State, out.Label = seedSeeding, "Seeding"
		out.Rate = rateOr(v.UpRate, "0 B/s")
	}
	return out
}

// shareProgress is "Ratio 1.4 of 2.0", "3h of 1d", both, or "Ratio 1.4 · no
// limit" for a torrent that may seed forever.
func shareProgress(t *view.Torrent, def core.SeedingSettings) string {
	ratio, seeding := t.ShareLimit.Ratio, t.ShareLimit.Seeding
	if t.ShareLimit.Global {
		l := def.ShareLimit()
		ratio, seeding = l.Ratio, l.SeedingTime
	}
	var parts []string
	if ratio >= 0 {
		parts = append(parts, fmt.Sprintf("Ratio %.1f of %.1f", t.Ratio, ratio))
	}
	if seeding >= 0 {
		parts = append(parts, shortSpan(t.SeedingTime)+" of "+shortSpan(seeding))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Ratio %.1f · no limit", t.Ratio)
	}
	return strings.Join(parts, " · ")
}

// shortSpan is a compact duration: "45s", "12m", "3h", "1d 2h".
func shortSpan(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		if m := int(d % time.Hour / time.Minute); m > 0 {
			return fmt.Sprintf("%dh %dm", int(d/time.Hour), m)
		}
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if h := int(d % (24 * time.Hour) / time.Hour); h > 0 {
		return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), h)
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// maxSeedingPoll bounds the rows one poll refreshes.
const maxSeedingPoll = 200

func (s *Server) registerHistoryRoutes() {
	s.handleAuthed("GET /history/seeding", s.getHistorySeeding)
	s.handleAuthed("POST /history/{id}/pause", s.historySeedingToggle(true))
	s.handleAuthed("POST /history/{id}/resume", s.historySeedingToggle(false))
}

// historyRowByID builds the History row of one Download.
func (s *Server) historyRowByID(r *http.Request, id core.ID) (historyRow, error) {
	d, err := s.Downloads.Get(r.Context(), id)
	if err != nil {
		return historyRow{}, err
	}
	names := map[core.ID]string{}
	if s.Categories != nil {
		cats, err := s.Categories.List(r.Context())
		if err != nil {
			return historyRow{}, err
		}
		for _, c := range cats {
			names[c.ID] = c.Name
		}
	}
	return historyRowOf(time.Now(), s.View.Of(r.Context(), d), names, s.Settings.Current().Seeding), nil
}

// historySeedingToggle pauses or resumes the Seeding of a Ready torrent and
// answers with its row.
func (s *Server) historySeedingToggle(pause bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := core.ID(r.PathValue("id"))
		if s.Downloads == nil || s.View == nil || !validDownloadID(string(id)) {
			s.queueError(w, r, core.ErrNotFound)
			return
		}
		var err error
		if pause {
			_, err = s.Downloads.Pause(r.Context(), id)
		} else {
			_, err = s.Downloads.Resume(r.Context(), id)
		}
		if err != nil {
			s.queueError(w, r, err)
			return
		}
		if !isHTMX(r) {
			s.redirect(w, r, "/history")
			return
		}
		row, err := s.historyRowByID(r, id)
		if err != nil {
			s.queueError(w, r, err)
			return
		}
		s.renderComponent(w, r, http.StatusOK, HistoryRow(row))
	}
}

// getHistorySeeding answers the History page's poll: the seeding cells and
// buttons of the rows it asks for, as out-of-band swaps. Rows that are not a
// Ready torrent are left out.
func (s *Server) getHistorySeeding(w http.ResponseWriter, r *http.Request) {
	if s.Downloads == nil || s.View == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var rows []historyRow
	seen := map[string]bool{}
	for _, id := range r.URL.Query()["id"] {
		if seen[id] || !validDownloadID(id) {
			continue
		}
		seen[id] = true
		if len(seen) > maxSeedingPoll {
			break
		}
		row, err := s.historyRowByID(r, core.ID(id))
		if err != nil || row.Seed == nil {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderComponent(w, r, http.StatusOK, HistorySeedingOOB(rows))
}
