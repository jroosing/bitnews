package sabnzbd

import (
	"slices"
	"strconv"
	"strings"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// catFilter is the Category names, and the Categories a listing is limited
// to.
type catFilter struct {
	names map[core.ID]string
	// want is the Category IDs to show ("" is the uncategorised ones); nil
	// shows every Download. none says the filter named nothing that exists.
	want map[core.ID]bool
	none bool
}

// catName is the name SAB shows for a Download's Category.
func (f catFilter) catName(d core.Download) string {
	if n, ok := f.names[d.CategoryID]; ok && d.CategoryID != "" {
		return n
	}
	return defaultCategoryName
}

func (f catFilter) matches(d core.Download) bool {
	return !f.none && (f.want == nil || f.want[d.CategoryID])
}

// single is the one Category to ask the repository for, or "" for all (and
// for the uncategorised ones, which matches filters afterwards).
func (f catFilter) single() core.ID {
	if len(f.want) != 1 {
		return ""
	}
	for id := range f.want {
		return id
	}
	return ""
}

// readCatFilter reads the Categories and parses a category filter: a
// comma-separated list of names, "*" for the uncategorised ones, empty for
// all.
func (c *call) readCatFilter(filter string) (catFilter, error) {
	cats, err := c.h.Categories.List(c.ctx())
	if err != nil {
		return catFilter{}, err
	}
	f := catFilter{names: make(map[core.ID]string, len(cats))}
	for _, cat := range cats {
		f.names[cat.ID] = cat.Name
	}
	if filter = strings.TrimSpace(filter); filter == "" {
		return f, nil
	}
	f.want = map[core.ID]bool{}
	for n := range strings.SplitSeq(filter, ",") {
		n = strings.TrimSpace(n)
		if n == defaultCategoryName {
			f.want[""] = true
		} else if cat, ok := app.FindCategory(cats, n); ok {
			f.want[cat.ID] = true
		}
	}
	f.none = len(f.want) == 0
	return f, nil
}

// queueViews is the NZB queue in queue order, from the view module.
func (c *call) queueViews() ([]view.View, error) {
	return c.h.View.Queue(c.ctx(), core.SourceNZB)
}

// historyDownloads reads the NZB Downloads that are out of the queue and
// match f, every page of them.
func (c *call) historyDownloads(f catFilter) ([]core.Download, error) {
	if f.none {
		return nil, nil
	}
	var out []core.Download
	for after := ""; ; {
		page, err := c.h.Downloads.List(c.ctx(), app.ListFilter{
			Kind: core.SourceNZB, CategoryID: f.single(), After: after, Limit: pageSize,
		})
		if err != nil {
			return nil, err
		}
		for _, d := range page.Items {
			if !d.State.InQueue() && f.matches(d) {
				out = append(out, d)
			}
		}
		if page.Next == "" {
			return out, nil
		}
		after = page.Next
	}
}

// paging reads start and limit. A missing, invalid or negative start is 0;
// limit 0 (the default) means everything.
func (c *call) paging() (start, limit int) {
	start, _ = strconv.Atoi(c.get("start"))
	limit, _ = strconv.Atoi(c.get("limit"))
	return max(start, 0), max(limit, 0)
}

// page returns the window [start, start+limit) of n items; limit 0 is all.
func page(n, start, limit int) (lo, hi int) {
	lo = min(start, n)
	hi = n
	if limit > 0 {
		hi = min(lo+limit, n)
	}
	return lo, hi
}

// ids splits the value parameter of a delete: comma-separated nzo_ids.
// Strings that are not ours are dropped.
func (c *call) ids() []core.ID {
	var out []core.ID
	for s := range strings.SplitSeq(c.params.Get("value"), ",") {
		if id, ok := parseNzoID(s); ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// deleteData reports whether the del_files parameter asks to delete the
// files too.
func (c *call) deleteData() bool {
	switch strings.ToLower(c.get("del_files")) {
	case "1", "true", "yes":
		return true
	}
	return false
}
