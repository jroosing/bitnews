package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/view"
)

// The queue page lists the Downloads that have not finished: Added through
// PostProcessing. Ready and Failed Downloads are on the History page, and so
// are Ready torrents that keep seeding. A torrent stays in the queue while it
// is Fetched (the engine seeds it) and being post-processed. Every fact of a
// row comes from the Download's View (internal/view).

// maxQueueRows caps the rows of one page render. The rest is counted.
const maxQueueRows = 500

// afterQueueStates are the States shown above a Queue: the Downloads that
// left it and are not finished, in this order.
var afterQueueStates = []core.State{core.StatePostProcessing, core.StateFetched}

// catOption is a Category the row menu can assign.
type catOption struct{ ID, Name string }

// queueRow is one Download as the queue shows it.
type queueRow struct {
	ID        string
	Name      string
	ShortName string // for accessible names and the confirmation dialog
	Torrent   bool
	State     core.State
	Paused    bool

	// Badge is the badge component's state, Status its text.
	Badge  string
	Status string

	Category      string
	CategoryID    string
	Priority      core.DownloadPriority
	PriorityLabel string

	Percent   int
	HasTotal  bool
	Done      string
	Total     string
	Variant   string // progress variant: ok, warn or empty
	Metadata  bool   // magnet link without metadata: there is no size yet
	Rate      string // "-" when there is none
	UpRate    string
	ETA       string
	ShowSwarm bool
	Peers     int
	Seeds     int
	Ratio     string

	CanPause, CanResume bool
	// CanMove is true for a Download that is in its Queue; First and Last
	// say where it is in it.
	CanMove     bool
	First, Last bool
}

// queueRowCtx is what the row templates receive.
type queueRowCtx struct {
	queueRow
	Cats []catOption
	CSRF string
	// OOB renders the row's live cells for an out-of-band swap.
	OOB bool
}

// queueSection is one card of the page: the Usenet or the torrent Queue.
type queueSection struct {
	Kind     string // "usenet" or "torrent"
	Title    string
	Subtitle string
	Rows     []queueRowCtx

	// Controls says the runtime reports this Queue, so it can be paused and
	// limited. Unavailable says the Queue does not exist on this instance.
	Controls    bool
	Unavailable bool
	Paused      bool
	PauseTitle  string
	PauseText   string

	LimitDown, LimitUp     string // text: "5.0 MB/s" or "No limit"
	LimitDownKB, LimitUpKB int64
	HasUpLimit             bool
	CSRF                   string
}

// queueView is what the queue templates receive.
type queueView struct {
	CSRF     string
	Sections []queueSection
	// More counts the Downloads left out because of maxQueueRows.
	More int
}

// queueData is the Data function of the queue page.
func queueData(s *Server, r *http.Request) (any, error) {
	return s.loadQueue(r.Context(), csrfOf(r))
}

// csrfOf is the CSRF token of the request's principal.
func csrfOf(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok {
		return p.CSRFToken()
	}
	return ""
}

// loadQueue reads the Downloads and the runtime state and builds the view.
func (s *Server) loadQueue(ctx context.Context, csrf string) (*queueView, error) {
	cats, names, err := s.categoryOptions(ctx)
	if err != nil {
		return nil, err
	}
	v := &queueView{CSRF: csrf}
	left := maxQueueRows
	for _, kind := range []core.SourceKind{core.SourceNZB, core.SourceTorrent} {
		sec := s.newSection(kind, csrf)
		views, err := s.queueViews(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, dv := range views {
			if !idValue.MatchString(string(dv.Download.ID)) {
				continue // never put an unexpected ID into markup or a URL
			}
			if left == 0 {
				v.More++
				continue
			}
			left--
			sec.Rows = append(sec.Rows, queueRowCtx{queueRow: newQueueRow(dv, names[dv.Download.CategoryID]), Cats: cats, CSRF: csrf})
		}
		markEnds(sec.Rows)
		v.Sections = append(v.Sections, sec)
	}
	return v, nil
}

// markEnds flags the first and last row that can move, so the menu can
// disable moves that would do nothing.
func markEnds(rows []queueRowCtx) {
	first, last := -1, -1
	for i := range rows {
		if rows[i].CanMove {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first >= 0 {
		rows[first].First, rows[last].Last = true, true
	}
}

// categoryOptions lists the Categories, sorted by name, and a name lookup.
func (s *Server) categoryOptions(ctx context.Context) ([]catOption, map[core.ID]string, error) {
	names := map[core.ID]string{}
	if s.Categories == nil {
		return nil, names, nil
	}
	cats, err := s.Categories.List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("web: list categories: %w", err)
	}
	slices.SortFunc(cats, func(a, b core.Category) int { return strings.Compare(a.Name, b.Name) })
	opts := make([]catOption, 0, len(cats))
	for _, c := range cats {
		if !idValue.MatchString(string(c.ID)) {
			continue
		}
		opts = append(opts, catOption{ID: string(c.ID), Name: c.Name})
		names[c.ID] = c.Name
	}
	return opts, names, nil
}

// queueViews returns the Views of one Source kind's section in display
// order: the Downloads in Post-processing, then the Fetched ones, each in
// queue order, then the Queue itself.
func (s *Server) queueViews(ctx context.Context, kind core.SourceKind) ([]view.View, error) {
	if s.Downloads == nil {
		return nil, nil
	}
	var out []view.View
	for _, st := range afterQueueStates {
		var ds []core.Download
		for after := ""; ; {
			page, err := s.Downloads.List(ctx, app.ListFilter{State: st, Kind: kind, After: after})
			if err != nil {
				return nil, fmt.Errorf("web: list %s downloads: %w", st, err)
			}
			ds = append(ds, page.Items...)
			if page.Next == "" {
				break
			}
			after = page.Next
		}
		slices.SortStableFunc(ds, core.CompareQueueOrder)
		for _, d := range ds {
			out = append(out, s.View.Of(ctx, d))
		}
	}
	queued, err := s.View.Queue(ctx, kind)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	return append(out, queued...), nil
}

// newSection builds the card header state of one Queue.
func (s *Server) newSection(kind core.SourceKind, csrf string) queueSection {
	sec := queueSection{CSRF: csrf}
	switch kind {
	case core.SourceTorrent:
		sec.Kind, sec.Title, sec.Subtitle = "torrent", "Torrents", "Magnet links and .torrent files. Finished torrents that keep seeding are in the history."
		sec.HasUpLimit = true
	default:
		sec.Kind, sec.Title, sec.Subtitle = "usenet", "Usenet", "NZB downloads"
	}
	if s.Runtime == nil {
		return sec
	}
	st := s.Runtime.QueueState(kind)
	if !st.Available {
		sec.Unavailable = true
		return sec
	}
	sec.Controls = true
	sec.Paused = st.Paused
	if st.Paused {
		sec.PauseTitle, sec.PauseText = pauseText(sec.Title, st.Reason)
	}
	down, up := s.Runtime.SpeedLimits(kind)
	sec.LimitDown, sec.LimitUp = limitText(down), limitText(up)
	sec.LimitDownKB, sec.LimitUpKB = down/1024, up/1024
	return sec
}

func limitText(bytesPerSecond int64) string {
	if bytesPerSecond <= 0 {
		return "No limit"
	}
	return formatRate(bytesPerSecond)
}

// pauseText explains why a Queue is paused.
func pauseText(queue, reason string) (title, text string) {
	title = queue + " queue paused"
	switch reason {
	case PauseReasonUser:
		return title, "You paused this queue. Nothing new starts until you resume it."
	case PauseReasonDiskFull:
		return title + ": disk almost full", "Free space is below the reserve, so bitnews stopped starting downloads. It resumes by itself once enough space is free."
	case PauseReasonBindUnavailable:
		return title + ": network not available", "The network interface or address this protocol is bound to is gone, for example a VPN that dropped. Nothing is sent over another route. The queue resumes when the interface is back."
	}
	if reason == "" {
		return title, "Nothing new starts until it resumes."
	}
	return title, "Nothing new starts until it resumes. Reason: " + reason + "."
}

// newQueueRow builds the row of a Download from its View. category is the
// Category's name, empty for none.
func newQueueRow(v view.View, category string) queueRow {
	d := v.Download
	r := queueRow{
		ID: string(d.ID), Name: d.Name, ShortName: truncateRunes(d.Name, 60),
		Torrent: d.SourceKind == core.SourceTorrent, State: d.State, Paused: d.Paused,
		Category: category, CategoryID: string(d.CategoryID),
		Priority: d.Priority, PriorityLabel: view.PriorityLabel(d.Priority),
		Metadata: v.Phase == view.PhaseMetadata,
		HasTotal: v.Total > 0, Percent: progressPercent(v),
		Done: formatBytes(v.Done), Total: formatBytes(v.Total),
		Rate: "-", UpRate: "-", ETA: "-",
	}
	r.Badge, r.Status = badgeOf(v)
	switch r.Badge {
	case "paused":
		r.Variant = "warn"
	case "ready": // a torrent seeding
		r.Variant = "ok"
	}
	if v.Phase == view.PhaseDownloading {
		r.Rate = formatRate(v.DownRate)
	}
	if v.ETAKnown {
		r.ETA = formatETA(v.ETA)
	}
	if r.Torrent {
		r.UpRate = formatRate(v.UpRate)
		if t := v.Torrent; t != nil && t.Registered && (d.State == core.StateFetched || d.State == core.StateFetching) {
			r.ShowSwarm, r.Peers, r.Seeds = true, t.Peers, t.Seeds
			r.Ratio = fmt.Sprintf("%.2f", t.Ratio)
		}
	}
	if v.InQueue {
		r.CanMove = true
		r.CanPause, r.CanResume = !d.Paused, d.Paused
	}
	return r
}

// registerQueue registers the queue's fragment, action and stream routes.
// The page itself is in builtinPages.
func (s *Server) registerQueue() {
	s.handleAuthed("GET /queue/fragment", s.getQueueFragment)
	s.registerQueueActions()
	s.registerLive()
}

// getQueueFragment answers the lists alone, for htmx to swap in after a
// resync or reconnect.
func (s *Server) getQueueFragment(w http.ResponseWriter, r *http.Request) {
	s.renderQueueLists(w, r, http.StatusOK)
}

// renderQueueLists writes the two Queues as a fragment.
func (s *Server) renderQueueLists(w http.ResponseWriter, r *http.Request, status int) {
	data, err := s.loadQueue(r.Context(), csrfOf(r))
	if err != nil {
		s.Logger.Error("queue fragment failed", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not load the queue. Details are in the server log.")
		return
	}
	v := s.newView(r, "Queue")
	v.Page = data
	s.renderFragment(w, r, status, "queue.html", "queue_fragment", v)
}
