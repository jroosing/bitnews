package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/view"
)

const (
	// historyPageSize is how many rows one page of the history shows.
	historyPageSize = 25
	// historyScanCap bounds how many Downloads one request reads to sort the
	// history. It is far above what a person accumulates; past it the oldest
	// entries are left out of the list.
	historyScanCap = 50_000
)

// historyFilter is the filter bar of the history page, as parsed from the
// query string. Unknown values fall back to "everything", so a stale link
// never breaks the page.
type historyFilter struct {
	// State is "" (ready and failed), "ready" or "failed".
	State string
	// Kind is "" (any), "nzb" or "torrent".
	Kind string
	// Category is "" (any) or the ID of an existing Category.
	Category string
	// Before is the paging cursor: only entries older than it are shown.
	Before string
}

// Active reports whether any filter is set.
func (f historyFilter) Active() bool { return f.State != "" || f.Kind != "" || f.Category != "" }

// query is the filter as a query string (without Before).
func (f historyFilter) values() url.Values {
	v := url.Values{}
	if f.State != "" {
		v.Set("state", f.State)
	}
	if f.Kind != "" {
		v.Set("kind", f.Kind)
	}
	if f.Category != "" {
		v.Set("category", f.Category)
	}
	return v
}

func (f historyFilter) href(before string) string {
	v := f.values()
	if before != "" {
		v.Set("before", before)
	}
	if len(v) == 0 {
		return "/history"
	}
	return "/history?" + v.Encode()
}

// parseHistoryFilter reads and validates the query.
func parseHistoryFilter(q url.Values, cats []core.Category) historyFilter {
	var f historyFilter
	switch s := q.Get("state"); s {
	case "ready", "failed":
		f.State = s
	}
	switch k := q.Get("kind"); k {
	case "nzb", "torrent":
		f.Kind = k
	}
	if c := q.Get("category"); c != "" && slices.ContainsFunc(cats, func(x core.Category) bool { return string(x.ID) == c }) {
		f.Category = c
	}
	if _, _, ok := parseCursor(q.Get("before")); ok {
		f.Before = q.Get("before")
	}
	return f
}

var cursorRE = regexp.MustCompile(`^(\d{1,19})-([A-Za-z0-9_]{1,64})$`)

// parseCursor splits "<unix nanoseconds>-<id>".
func parseCursor(s string) (at int64, id string, ok bool) {
	m := cursorRE.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return n, m[2], true
}

func cursorOf(d core.Download) string {
	return strconv.FormatInt(historyTime(d).UnixNano(), 10) + "-" + string(d.ID)
}

// historyTime is when a Download finished, which is what the history is
// ordered by: ReadyAt for a Ready one, else the last update (the moment it
// Failed; Failed is terminal, so nothing updates it afterwards but an edit of
// its Category or Priority), else when it was added.
func historyTime(d core.Download) time.Time {
	t := d.UpdatedAt
	if d.State == core.StateReady && !d.ReadyAt.IsZero() {
		t = d.ReadyAt
	}
	if t.IsZero() {
		t = d.AddedAt
	}
	return t
}

// historyRow is one entry of the list.
type historyRow struct {
	ID         string
	Name       string
	ShortName  string // for accessible names and confirmation texts
	Kind       string // nzb | torrent
	KindLabel  string // "NZB" or "Torrent"
	KindIcon   string
	Torrent    bool
	State      string
	Badge      string // badge class suffix
	BadgeLabel string
	Category   string
	Size       string
	WhenLabel  string // "Completed" or "Failed"
	When       whenView
	Ready      bool
	Failed     bool
	OutputPath string
	// Reason is the headline of a failure and Detail its redacted detail.
	Reason string
	Detail string
	// Seed is set for a Ready torrent: whether it seeds, with the upload
	// speed and the Share Limit progress.
	Seed *seedView
}

// historyPage is what HistoryPage renders.
type historyPage struct {
	Filter    historyFilter
	StateOpts []option
	KindOpts  []option
	CatOpts   []option
	Rows      []historyRow
	Total     int
	First     int // 1-based index of the first row shown
	Last      int
	NextHref  string // empty on the last page
	// AppendHref is NextHref for the "Show older" button, which appends the
	// rows to the list instead of replacing the page.
	AppendHref string
	NewestHref string // set when the page is not the first
	// Empty-state wording for the current filter.
	EmptyTitle, EmptyBody string
	Truncated             bool
	Unavailable           bool
}

// historyComponent is the Component function of the History page.
func historyComponent(v *View, data any) templ.Component {
	return HistoryPage(v, data.(historyPage))
}

// historyData is the Data function of the History page.
func historyData(s *Server, r *http.Request) (any, error) {
	if s.Downloads == nil {
		return historyPage{Unavailable: true}, nil
	}
	ctx := r.Context()
	var cats []core.Category
	if s.Categories != nil {
		var err error
		if cats, err = s.Categories.List(ctx); err != nil {
			return nil, err
		}
	}
	f := parseHistoryFilter(r.URL.Query(), cats)
	all, truncated, err := s.historyEntries(ctx, f)
	if err != nil {
		return nil, err
	}

	p := historyPage{Filter: f, Total: len(all), Truncated: truncated}
	p.StateOpts = []option{{"", "Ready and failed"}, {"ready", "Ready"}, {"failed", "Failed"}}
	p.KindOpts = []option{{"", "Usenet and torrents"}, {"nzb", "Usenet (NZB)"}, {"torrent", "Torrents"}}
	p.CatOpts = []option{{"", "All categories"}}
	names := map[core.ID]string{}
	for _, c := range cats {
		names[c.ID] = c.Name
		p.CatOpts = append(p.CatOpts, option{string(c.ID), histClean(c.Name, 100)})
	}

	start := 0
	if at, id, ok := parseCursor(f.Before); ok {
		start = len(all)
		for i, d := range all {
			if olderThan(d, at, id) {
				start = i
				break
			}
		}
		p.NewestHref = f.href("")
	}
	page := all[start:min(start+historyPageSize, len(all))]
	if start+len(page) < len(all) && len(page) > 0 {
		p.NextHref = f.href(cursorOf(page[len(page)-1]))
		p.AppendHref = p.NextHref + "&append=1"
	}
	now := time.Now()
	seeding := s.Settings.Current().Seeding
	for _, d := range page {
		p.Rows = append(p.Rows, historyRowOf(now, s.View.Of(ctx, d), names, seeding))
	}
	if len(page) > 0 {
		p.First, p.Last = start+1, start+len(page)
		if r.URL.Query().Get("append") == "1" {
			// "Show older" appended these rows to the ones above.
			p.First = 1
		}
	}
	p.EmptyTitle, p.EmptyBody = historyEmpty(f)
	return p, nil
}

// olderThan reports whether d sorts after the cursor in newest-first order.
func olderThan(d core.Download, at int64, id string) bool {
	t := historyTime(d).UnixNano()
	return t < at || (t == at && string(d.ID) < id)
}

// historyEntries reads every Ready and Failed Download matching the filter
// (except the paging cursor) and returns them newest first. The Download
// service lists oldest first with a forward-only cursor, so the whole result
// is read, which for a personal history is a few hundred rows at most.
func (s *Server) historyEntries(ctx context.Context, f historyFilter) (all []core.Download, truncated bool, err error) {
	states := []core.State{core.StateReady, core.StateFailed}
	switch f.State {
	case "ready":
		states = states[:1]
	case "failed":
		states = states[1:]
	}
	for _, st := range states {
		for after := ""; ; {
			page, err := s.Downloads.List(ctx, app.ListFilter{
				State: st, Kind: core.SourceKind(f.Kind), CategoryID: core.ID(f.Category),
				After: after, Limit: store.MaxListLimit,
			})
			if err != nil {
				return nil, false, fmt.Errorf("history: list %s downloads: %w", st, err)
			}
			all = append(all, page.Items...)
			if page.Next == "" {
				break
			}
			if len(all) >= historyScanCap {
				truncated = true
				break
			}
			after = page.Next
		}
	}
	slices.SortFunc(all, func(a, b core.Download) int {
		if c := historyTime(b).Compare(historyTime(a)); c != 0 {
			return c
		}
		return strings.Compare(string(b.ID), string(a.ID))
	})
	return all, truncated, nil
}

func historyRowOf(now time.Time, v view.View, categories map[core.ID]string, seeding core.SeedingSettings) historyRow {
	d := v.Download
	badge, badgeLabel := badgeOf(v)
	kind, icon := sourceLabel(d.SourceKind)
	row := historyRow{
		ID: string(d.ID), Name: histClean(d.Name, 255), ShortName: histClean(d.Name, 60), Kind: string(d.SourceKind), KindLabel: kind, KindIcon: icon,
		Torrent: d.SourceKind == core.SourceTorrent,
		State:   string(d.State), Badge: badge, BadgeLabel: badgeLabel,
		Category: histClean(categories[d.CategoryID], 100), Size: formatBytes(d.TotalBytes),
		When: histWhen(now, historyTime(d)), Ready: d.State == core.StateReady, Failed: d.State == core.StateFailed,
	}
	if row.Name == "" {
		row.Name, row.ShortName = "(unnamed)", "(unnamed)"
	}
	switch {
	case row.Ready:
		row.WhenLabel = "Completed"
		row.OutputPath = histClean(d.OutputPath, 1024)
		row.Seed = seedOf(v, seeding)
	case row.Failed:
		row.WhenLabel = "Failed"
		row.Reason = failureOf(d.FailureReason).Title
		row.Detail = failureDetail(v)
	}
	return row
}

func historyEmpty(f historyFilter) (title, body string) {
	switch {
	case !f.Active():
		return "Nothing in the history yet", "Finished downloads stay here until you remove them, with the reason for any failure."
	case f.State == "failed" && f.Kind == "" && f.Category == "":
		return "No failed downloads", "Nothing has failed. A download that fails shows up here with the reason."
	case f.State == "ready" && f.Kind == "" && f.Category == "":
		return "No ready downloads yet", "Downloads that finish show up here with the folder they were saved to."
	}
	return "No downloads match these filters", "Nothing in the history fits them. Try removing a filter."
}
