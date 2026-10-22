package web

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// The queue page lists the Downloads that have not finished: Added through
// PostProcessing. Ready and Failed Downloads are on the History page, and so
// are Ready torrents that keep seeding. A torrent stays in the queue while it
// is Fetched (the engine seeds it) and being post-processed.

// maxQueueRows caps the rows of one page render. The rest is counted.
const maxQueueRows = 500

// queueStates are the States shown on the queue page.
var queueStates = []core.State{
	core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching,
	core.StateFetched, core.StatePostProcessing,
}

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
	usenet := s.newSection(core.SourceNZB, csrf)
	torrent := s.newSection(core.SourceTorrent, csrf)
	downloads, more, err := s.activeDownloads(ctx)
	if err != nil {
		return nil, err
	}
	v.More = more
	sortQueue(downloads)
	for _, d := range downloads {
		if !idValue.MatchString(string(d.ID)) {
			continue // never put an unexpected ID into markup or a URL
		}
		sec := &usenet
		if d.SourceKind == core.SourceTorrent {
			sec = &torrent
		}
		var live DownloadLive
		if s.Runtime != nil {
			live = s.Runtime.Live(d.ID)
		}
		sec.Rows = append(sec.Rows, queueRowCtx{queueRow: newQueueRow(d, live, names[d.CategoryID]), Cats: cats, CSRF: csrf})
	}
	for _, sec := range []*queueSection{&usenet, &torrent} {
		markEnds(sec.Rows)
	}
	v.Sections = []queueSection{usenet, torrent}
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

// activeDownloads returns the Downloads the queue shows, at most
// maxQueueRows, and how many more there are.
func (s *Server) activeDownloads(ctx context.Context) (out []core.Download, more int, err error) {
	if s.Downloads == nil {
		return nil, 0, nil
	}
	for _, st := range queueStates {
		for after := ""; ; {
			page, err := s.Downloads.List(ctx, app.ListFilter{State: st, After: after})
			if err != nil {
				return nil, 0, fmt.Errorf("web: list %s downloads: %w", st, err)
			}
			for _, d := range page.Items {
				if len(out) < maxQueueRows {
					out = append(out, d)
				} else {
					more++
				}
			}
			if page.Next == "" {
				break
			}
			after = page.Next
		}
	}
	return out, more, nil
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

// sortQueue orders Downloads for display: post-processing first, then the
// fetched ones, then the Queue itself by position.
func sortQueue(ds []core.Download) {
	group := func(d core.Download) int {
		switch d.State {
		case core.StatePostProcessing:
			return 0
		case core.StateFetched:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(ds, func(a, b core.Download) int {
		return cmp.Or(
			cmp.Compare(group(a), group(b)),
			cmp.Compare(a.QueuePosition, b.QueuePosition),
			a.AddedAt.Compare(b.AddedAt),
			strings.Compare(string(a.ID), string(b.ID)),
		)
	})
}

// newQueueRow builds the row of a Download. category is the Category's name,
// empty for none.
func newQueueRow(d core.Download, live DownloadLive, category string) queueRow {
	r := queueRow{
		ID: string(d.ID), Name: d.Name, ShortName: truncateRunes(d.Name, 60),
		Torrent: d.SourceKind == core.SourceTorrent, State: d.State, Paused: d.Paused,
		Category: category, CategoryID: string(d.CategoryID),
		Priority: d.Priority, PriorityLabel: priorityLabel(d.Priority),
		Rate: "-", UpRate: "-", ETA: "-",
	}
	done, total := d.DoneBytes, d.TotalBytes
	switch d.State {
	case core.StateFetched, core.StatePostProcessing:
		if total > 0 {
			done = total
		}
	}
	r.fill(done, total)
	var rate int64
	switch d.State {
	case core.StateAdded, core.StateQueued:
		r.Badge, r.Status = "queued", "Queued"
	case core.StateFetchingMetadata:
		r.Badge, r.Status, r.Metadata = "downloading", "Metadata", true
	case core.StateFetching:
		r.Badge, r.Status = "downloading", "Downloading"
		if !d.Paused {
			rate = live.DownRate
		}
	case core.StateFetched:
		if r.Torrent {
			r.Badge, r.Status, r.Variant = "ready", "Seeding", "ok"
		} else {
			r.Badge, r.Status = "processing", "Fetched"
		}
	case core.StatePostProcessing:
		r.Badge, r.Status = "processing", stageLabel(live.Stage)
	}
	if d.Paused {
		r.Badge, r.Status, r.Variant = "paused", "Paused", "warn"
	}
	r.Rate = formatRate(rate)
	r.ETA = formatETA(total-done, rate)
	if r.Torrent {
		r.UpRate = formatRate(live.UpRate)
		if live.Torrent && (d.State == core.StateFetched || d.State == core.StateFetching) {
			r.ShowSwarm, r.Peers, r.Seeds = true, live.Peers, live.Seeds
			r.Ratio = fmt.Sprintf("%.2f", live.Ratio)
		}
	}
	switch d.State {
	case core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching:
		r.CanMove = true
		r.CanPause, r.CanResume = !d.Paused, d.Paused
	}
	return r
}

// fill sets the progress fields.
func (r *queueRow) fill(done, total int64) {
	r.HasTotal = total > 0
	r.Percent = percent(done, total)
	r.Done, r.Total = formatBytes(done), formatBytes(total)
}

func priorityLabel(p core.DownloadPriority) string {
	switch p {
	case core.DownloadPriorityForce:
		return "Force"
	case core.DownloadPriorityHigh:
		return "High"
	case core.DownloadPriorityLow:
		return "Low"
	}
	return "Normal"
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
