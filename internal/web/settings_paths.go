package web

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"gitlab.com/kyura4k/gitnews/internal/core"
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
	d := pathsPageData{
		Paths: pathsViewFrom(settingsCSRF(r), s.Settings.Current().Paths, nil),
	}
	cv, err := s.categoriesViewFor(r, categoriesView{})
	if err != nil {
		return nil, err
	}
	d.Categories = cv
	return d, nil
}

func (s *Server) registerPathRoutes() {
	s.handleAuthed("POST /settings/paths", s.postPaths)
	s.registerCategoryRoutes()
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

// postPaths saves the Paths part of the Settings and leaves the rest alone.
func (s *Server) postPaths(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	paths, typed, errs := parsePaths(r.PostForm)
	pathProblemErrors(paths, errs)
	if len(errs) > 0 {
		v := pathsViewFrom(settingsCSRF(r), typed, errs)
		v.Dirty = true
		v.Error = "Some folders need fixing. Nothing was saved."
		s.respondPaths(w, r, http.StatusUnprocessableEntity, v)
		return
	}

	next := s.Settings.Current()
	next.Paths = paths
	if err := s.Settings.Update(r.Context(), next); err != nil {
		if ae, ok := settingsAppError(err); ok {
			v := pathsViewFrom(settingsCSRF(r), typed, nil)
			v.Dirty, v.Error = true, ae.Message
			s.respondPaths(w, r, http.StatusUnprocessableEntity, v)
			return
		}
		s.settingsFail(w, r, "settings", err)
		return
	}

	v := pathsViewFrom(settingsCSRF(r), paths, nil)
	if isHTMX(r) {
		settingsTrigger(w, "success", "Folders saved. New downloads use them.", nil)
	} else {
		v.Saved = true
	}
	s.respondPaths(w, r, http.StatusOK, v)
}

// respondPaths answers with the card for htmx, or the whole page.
func (s *Server) respondPaths(w http.ResponseWriter, r *http.Request, status int, v pathsView) {
	if isHTMX(r) {
		s.renderSettingsPart(w, status, settingsPathsPage, "paths_card", v)
		return
	}
	cv, err := s.categoriesViewFor(r, categoriesView{})
	if err != nil {
		s.settingsFail(w, r, "category", err)
		return
	}
	s.renderSettingsPage(w, r, status, "paths", pathsPageData{Paths: v, Categories: cv})
}
