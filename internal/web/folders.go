package web

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/jroosing/bitnews/internal/fsutil"
)

// maxPickerFolders caps one listing of the folder picker.
const maxPickerFolders = 500

// defaultPickerStart is where the picker opens when the field is empty: the
// shared /data mount of the Docker layout, or "/" without it.
const defaultPickerStart = "/data"

// folderListView is one folder of the folder picker (the "folder_list"
// fragment).
type folderListView struct {
	Path     string
	Writable bool
	// Up opens the parent folder; empty at the top.
	Up     string
	Crumbs []folderCrumb
	Dirs   []folderEntry
	// Truncated says only the first Limit folders are listed.
	Truncated bool
	Limit     int
	// Error says the folder could not be read.
	Error string
}

type folderCrumb struct{ Name, Href string }

type folderEntry struct {
	Name, Path, Href string
	Writable         bool
}

func folderHref(p string) string {
	return "/settings/folders?" + url.Values{"path": {p}}.Encode()
}

// getFolders serves the folder picker of the settings pages: the folders
// directly inside ?path= (or its nearest existing parent), on the machine
// bitnews runs on. It needs a session, like every settings page; the API key
// cannot reach it. It lists folders only (no files, no symlinks, no hidden
// entries) and never writes anything.
func (s *Server) getFolders(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if strings.TrimSpace(p) == "" {
		p = defaultPickerStart
	}
	dir := fsutil.NearestDir(p)
	v := folderListView{Path: dir, Writable: fsutil.Writable(dir), Limit: maxPickerFolders}
	if parent := filepath.Dir(dir); parent != dir {
		v.Up = folderHref(parent)
	}
	v.Crumbs = append(v.Crumbs, folderCrumb{Name: "/", Href: folderHref("/")})
	acc := string(filepath.Separator)
	for part := range strings.SplitSeq(strings.Trim(filepath.ToSlash(dir), "/"), "/") {
		if part == "" {
			continue
		}
		acc = filepath.Join(acc, part)
		v.Crumbs = append(v.Crumbs, folderCrumb{Name: part, Href: folderHref(acc)})
	}
	dirs, truncated, err := fsutil.ListDirs(dir, maxPickerFolders)
	if err != nil {
		v.Error = "bitnews cannot read this folder. Choose another one, or type the path."
	}
	v.Truncated = truncated
	for _, d := range dirs {
		v.Dirs = append(v.Dirs, folderEntry{Name: d.Name, Path: d.Path, Href: folderHref(d.Path), Writable: d.Writable})
	}
	view := s.newView(r, "")
	view.Page = v
	s.renderFragment(w, r, http.StatusOK, settingsPathsPage, "folder_list", view)
}
