package web

import (
	"net/http"
	"slices"
	"strings"
)

// Page is one full-page route behind the sign-in. To add a page, append a
// Page to builtinPages (or call Server.addPage from a routes_<name>.go
// file), create assets/templates/pages/<Template> and run `make css`.
type Page struct {
	// Path is the GET route, e.g. "/queue".
	Path string
	// Template is the file below assets/templates/pages/.
	Template string
	// Title is the page heading and the document title.
	Title string
	// NavLabel and NavIcon put the page into the sidebar; an empty NavLabel
	// keeps it out of the navigation (the style guide).
	NavLabel, NavIcon string
	// Data returns the value templates see as .Page. Optional.
	Data func(s *Server, r *http.Request) (any, error)
}

// builtinPages are the pages of the app shell, in navigation order.
func builtinPages() []Page {
	return []Page{
		{Path: "/queue", Template: "queue.html", Title: "Queue", NavLabel: "Queue", NavIcon: "queue", Data: queueData},
		{Path: "/history", Template: "history.html", Title: "History", NavLabel: "History", NavIcon: "history", Data: historyData},
		{Path: "/ui/styleguide", Template: "styleguide.html", Title: "Style guide", Data: styleguideData},
	}
}

// addPage registers a page: a GET route that authenticates, renders the
// layout around the page template and, when NavLabel is set, shows up in the
// sidebar.
func (s *Server) addPage(p Page) {
	s.pages = append(s.pages, p)
	s.handleAuthed("GET "+p.Path, func(w http.ResponseWriter, r *http.Request) {
		v := s.newView(r, p.Title)
		if p.Data != nil {
			data, err := p.Data(s, r)
			if err != nil {
				s.Logger.Error("page data failed", "page", p.Path, "err", err)
				s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not load this page. Details are in the server log.")
				return
			}
			v.Page = data
		}
		s.render(w, r, http.StatusOK, p.Template, "layout_app", v)
	})
}

// navItems lists the sidebar entries, marking the one for path active.
func (s *Server) navItems(path string) []NavItem {
	var items []NavItem
	for _, p := range s.pages {
		if p.NavLabel == "" {
			continue
		}
		items = append(items, NavItem{
			Path: p.Path, Label: p.NavLabel, Icon: p.NavIcon,
			Active: path == p.Path || strings.HasPrefix(path, p.Path+"/"),
		})
	}
	return items
}

type styleSwatch struct{ Name, Class string }

type styleRow struct {
	Name, State, Size, Speed string
	Percent                  int
}

type styleguide struct {
	Swatches []styleSwatch
	States   []string
	Rows     []styleRow
	Options  []option
	Icons    []string
}

// option is one entry of a select_field.
type option struct{ Value, Label string }

func styleguideData(*Server, *http.Request) (any, error) {
	g := styleguide{
		Swatches: []styleSwatch{
			{"bg", "bg-bg"}, {"surface", "bg-surface"}, {"surface-2", "bg-surface-2"}, {"surface-3", "bg-surface-3"},
			{"line", "bg-line"}, {"line-strong", "bg-line-strong"},
			{"fg", "bg-fg"}, {"fg-muted", "bg-fg-muted"}, {"fg-subtle", "bg-fg-subtle"},
			{"accent", "bg-accent"}, {"accent-hover", "bg-accent-hover"}, {"accent-soft", "bg-accent-soft"},
			{"accent-text", "bg-accent-text"}, {"danger", "bg-danger"},
			{"info-soft", "bg-info-soft"}, {"ok-soft", "bg-ok-soft"}, {"warn-soft", "bg-warn-soft"}, {"bad-soft", "bg-bad-soft"},
		},
		States: []string{"queued", "downloading", "processing", "ready", "failed", "paused"},
		Rows: []styleRow{
			{"Ubuntu.24.04.Desktop.amd64", "downloading", "5.7 GB", "48.2 MB/s", 64},
			{"Some.Show.S01E01.1080p.WEB-DL", "processing", "2.1 GB", "-", 100},
			{"Another.Release.2160p.BluRay.REMUX", "queued", "58.4 GB", "-", 0},
			{"Broken.Upload.720p", "failed", "1.3 GB", "-", 71},
		},
		Options: []option{{"force", "Force"}, {"high", "High"}, {"normal", "Normal"}, {"low", "Low"}},
	}
	for name := range iconPaths {
		g.Icons = append(g.Icons, string(name))
	}
	slices.Sort(g.Icons)
	return g, nil
}
