package sabnzbd

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// view holds what queue and history need to build their answers: the
// NZB Downloads of some states, the Category names, and the filter.
type view struct {
	downloads []core.Download
	catNames  map[core.ID]string
}

// catName is the name SAB shows for a Download's Category.
func (v view) catName(d core.Download) string {
	if n, ok := v.catNames[d.CategoryID]; ok && d.CategoryID != "" {
		return n
	}
	return defaultCategoryName
}

// load reads the NZB Downloads in the given states that match the category
// filter (a comma-separated list of names; "*" is the uncategorised ones;
// empty is all).
func (c *call) load(states []core.State, categoryFilter string) (view, error) {
	cats, err := c.h.Categories.List(c.ctx())
	if err != nil {
		return view{}, err
	}
	v := view{catNames: make(map[core.ID]string, len(cats))}
	for _, cat := range cats {
		v.catNames[cat.ID] = cat.Name
	}

	var want map[core.ID]bool // nil: no filter
	var single core.ID        // the one Category to ask the repository for
	if categoryFilter = strings.TrimSpace(categoryFilter); categoryFilter != "" {
		want = map[core.ID]bool{}
		names := strings.SplitSeq(categoryFilter, ",")
		for n := range names {
			n = strings.TrimSpace(n)
			if n == defaultCategoryName {
				want[""] = true
			} else if cat, ok := findCategory(cats, n); ok {
				want[cat.ID] = true
			}
		}
		if len(want) == 0 {
			return v, nil // nothing can match
		}
		if len(want) == 1 {
			for id := range want {
				single = id // "" (uncategorised) is filtered below
			}
		}
	}

	for _, st := range states {
		for after := ""; ; {
			page, err := c.h.Downloads.List(c.ctx(), app.ListFilter{
				State: st, Kind: core.SourceNZB, CategoryID: single, After: after, Limit: pageSize,
			})
			if err != nil {
				return view{}, err
			}
			for _, d := range page.Items {
				if want == nil || want[d.CategoryID] {
					v.downloads = append(v.downloads, d)
				}
			}
			if page.Next == "" {
				break
			}
			after = page.Next
		}
	}
	return v, nil
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

// byQueueOrder sorts Downloads the way their Queue runs them.
func byQueueOrder(a, b core.Download) int {
	return cmp.Or(
		cmp.Compare(a.QueuePosition, b.QueuePosition),
		a.AddedAt.Compare(b.AddedAt),
		strings.Compare(string(a.ID), string(b.ID)),
	)
}
