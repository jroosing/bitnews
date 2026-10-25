package web

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/jroosing/bitnews/internal/core"
)

// settingsPaths is the template and data of the Paths & Categories section.
// The page holds two independent cards, each swapped on its own by htmx.
const settingsPathsPage = "settings_paths.html"

// pathsPageData is the .Page.Data of the section.
type pathsPageData struct {
	Paths      pathsView
	Categories categoriesView
}

// pathsView is the "Download folders" card.
type pathsView struct {
	CSRF   string
	Groups []pathsGroup
	// Error is a problem that belongs to no single field.
	Error string
	// Dirty starts the form in the unsaved state (after a failed save).
	Dirty bool
	// Saved is the plain-POST success note; htmx shows a toast instead.
	Saved bool
}

// pathsGroup is the two folders of one protocol.
type pathsGroup struct {
	Key, Title, Summary string
	Incomplete          pathsField
	Complete            pathsField
}

type pathsField struct {
	ID, Name, Label, Value, Help, Error string
	Placeholder                         string
}

var pathsGroupDefs = []struct{ key, title, summary, protocol string }{
	{"usenet", "Usenet", "NZB downloads are unpacked in the incomplete folder, then moved to the complete folder.", "Usenet"},
	{"torrent", "Torrents", "Torrent data downloads in the incomplete folder and is moved to the complete folder when it finishes.", "Torrent"},
}

// pathsViewFrom builds the card for the given folders. The Errors map is
// keyed by form field name.
func pathsViewFrom(csrf string, paths core.Paths, errs map[string]string) pathsView {
	roots := map[string]core.Roots{"usenet": paths.Usenet, "torrent": paths.Torrent}
	v := pathsView{CSRF: csrf}
	for _, d := range pathsGroupDefs {
		r := roots[d.key]
		v.Groups = append(v.Groups, pathsGroup{
			Key: d.key, Title: d.title, Summary: d.summary,
			Incomplete: pathsField{
				ID: d.key + "-incomplete", Name: d.key + "_incomplete", Label: "Incomplete folder",
				Value: r.Incomplete, Help: "Work in progress. Use fast local storage.",
				Placeholder: "/data/" + d.key + "/incomplete", Error: errs[d.key+"_incomplete"],
			},
			Complete: pathsField{
				ID: d.key + "-complete", Name: d.key + "_complete", Label: "Complete folder",
				Value: r.Complete, Help: "Finished downloads, in a sub-folder per category.",
				Placeholder: "/data/" + d.key + "/complete", Error: errs[d.key+"_complete"],
			},
		})
	}
	return v
}

func settingsPathsData(s *Server, r *http.Request) (any, error) {
	return pathsPage(s, r, s.cardPage(r, pathsCards, nil))
}

// pathsPage is the section page: the folders card and the categories card.
func pathsPage(s *Server, r *http.Request, views cardViews) (any, error) {
	d := pathsPageData{Paths: views["paths"].(pathsView)}
	cv, err := s.categoriesViewFor(r, categoriesView{})
	if err != nil {
		return nil, err
	}
	d.Categories = cv
	return d, nil
}

func (s *Server) registerPathRoutes() {
	s.registerCards(pathsCards)
	s.handleAuthed("GET /settings/folders", s.getFolders)
	s.registerCategoryRoutes()
}

var pathsCards = []*settingsCard{pathsCard}

// pathsFields are the form fields of the folders, in the order of core.Paths.
var pathsFields = []string{"usenet_incomplete", "usenet_complete", "torrent_incomplete", "torrent_complete"}

// pathsOf reads the four folders from form values, as they are.
func pathsOf(v map[string]string) core.Paths {
	return core.Paths{
		Usenet:  core.Roots{Incomplete: v["usenet_incomplete"], Complete: v["usenet_complete"]},
		Torrent: core.Roots{Incomplete: v["torrent_incomplete"], Complete: v["torrent_complete"]},
	}
}

// pathsCard edits Settings.Paths. New downloads use the folders at once.
var pathsCard = &settingsCard{
	key: "paths", route: "/settings/paths", slug: "paths", partial: "paths_card",
	saved:   "Folders saved. New downloads use them.",
	invalid: "Some folders need fixing. Nothing was saved.",
	values: func(s core.Settings) map[string]string {
		p := s.Paths
		return map[string]string{
			"usenet_incomplete": p.Usenet.Incomplete, "usenet_complete": p.Usenet.Complete,
			"torrent_incomplete": p.Torrent.Incomplete, "torrent_complete": p.Torrent.Complete,
		}
	},
	read: func(_ *Server, f map[string]string, next *core.Settings) map[string]string {
		form := map[string][]string{}
		for _, name := range pathsFields {
			form[name] = []string{f[name]}
		}
		paths, _, errs := parsePaths(form)
		pathProblemErrors(paths, errs)
		if len(errs) > 0 {
			return errs
		}
		next.Paths = paths
		return nil
	},
	view: func(_ *Server, v cardView, _ core.Settings) any {
		typed := map[string]string{}
		for _, name := range pathsFields {
			typed[name] = strings.TrimSpace(v.V[name])
		}
		pv := pathsViewFrom(v.CSRF, pathsOf(typed), v.E)
		pv.Error, pv.Dirty, pv.Saved = v.Error, v.Dirty, v.Saved
		return pv
	},
	page: pathsPage,
}

// parsePaths reads the four folders of the form. It returns the folders
// (cleaned), the values as typed, and one message per bad field.
func parsePaths(form map[string][]string) (paths core.Paths, typed core.Paths, errs map[string]string) {
	errs = map[string]string{}
	get := func(name string) string {
		if v := form[name]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	clean := func(name string) (typedValue, cleaned string) {
		typedValue = get(name)
		if typedValue == "" {
			errs[name] = "Enter the full path of a folder, such as /data/usenet/incomplete."
		} else if msg := pathShapeError(typedValue, "Use an absolute path: it has to start with / (for example /data/usenet/incomplete)."); msg != "" {
			errs[name] = msg
		} else {
			cleaned = filepath.Clean(typedValue)
		}
		return typedValue, cleaned
	}
	typed.Usenet.Incomplete, paths.Usenet.Incomplete = clean("usenet_incomplete")
	typed.Usenet.Complete, paths.Usenet.Complete = clean("usenet_complete")
	typed.Torrent.Incomplete, paths.Torrent.Incomplete = clean("torrent_incomplete")
	typed.Torrent.Complete, paths.Torrent.Complete = clean("torrent_complete")
	return paths, typed, errs
}

// pathShapeError is the shape check of a typed folder: no control
// characters, absolute (relativeMsg is said otherwise) and no ".." element.
// It looks at the text only, never at the disk. A trailing slash or a doubled
// separator is not an error: the caller cleans it.
func pathShapeError(p, relativeMsg string) string {
	switch {
	case strings.ContainsFunc(p, unicode.IsControl):
		return "The path must not contain control characters."
	case !filepath.IsAbs(p):
		return relativeMsg
	case slices.Contains(strings.Split(filepath.ToSlash(p), "/"), ".."):
		return `The path must not contain ".." (write the full path instead).`
	}
	return ""
}

// pathProblemErrors maps the shape problems core finds in the four folders
// (a root nested in another) to the form field of the folder at fault.
func pathProblemErrors(p core.Paths, errs map[string]string) {
	for _, pp := range p.Problems() {
		name := pp.Protocol + "_" + pp.Root
		if _, seen := errs[name]; !seen {
			errs[name] = "This folder " + pp.Reason + ". Keep the incomplete and complete folders apart, not one inside the other."
		}
	}
}
