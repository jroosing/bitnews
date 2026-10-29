package web

import (
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/a-h/templ"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

// categoriesView is the "Categories" card: the table and the add form.
type categoriesView struct {
	Items []categoryRow
	// The add form: what was typed and what was wrong with it.
	Name      string
	NameError string
	Dirs      categoryDirs
	Error     string
}

// categoryRow is one row of the table, read-only or being edited.
type categoryRow struct {
	ID   string
	Name string
	Dirs categoryDirs
}

// categoryDirs are the folder overrides of a category as typed, one per
// protocol (empty means the default folder), with their problems.
type categoryDirs struct {
	Usenet, Torrent           string
	UsenetError, TorrentError string
}

func (d categoryDirs) invalid() bool { return d.UsenetError != "" || d.TorrentError != "" }

func categoryRowOf(c core.Category) categoryRow {
	return categoryRow{ID: string(c.ID), Name: c.Name, Dirs: categoryDirs{Usenet: c.UsenetDir, Torrent: c.TorrentDir}}
}

// readCategoryDirs reads the two folder fields of a form and checks their
// shape (never the disk), each next to its own field. Valid folders are
// cleaned: a trailing or doubled separator is dropped.
func readCategoryDirs(form url.Values) categoryDirs {
	d := categoryDirs{Usenet: strings.TrimSpace(form.Get("usenet_dir")), Torrent: strings.TrimSpace(form.Get("torrent_dir"))}
	if d.Usenet != "" {
		if d.UsenetError = pathShapeError(d.Usenet, categoryDirMsg); d.UsenetError == "" {
			d.Usenet = filepath.Clean(d.Usenet)
		}
	}
	if d.Torrent != "" {
		if d.TorrentError = pathShapeError(d.Torrent, categoryDirMsg); d.TorrentError == "" {
			d.Torrent = filepath.Clean(d.Torrent)
		}
	}
	return d
}

// Messages for category input, shared by the create and edit forms.
const (
	categoryNameMsg = "Use 1 to 64 lowercase letters, digits, dots, underscores or hyphens. A name of only dots is not allowed."
	categoryDirMsg  = "Use an absolute path: it has to start with / (or leave it empty for the default folder)."
)

// categoriesViewFor loads the table. base carries the add form state.
func (s *Server) categoriesViewFor(r *http.Request, base categoriesView) (categoriesView, error) {
	cats, err := s.Categories.List(r.Context())
	if err != nil {
		return categoriesView{}, err
	}
	base.Items = make([]categoryRow, 0, len(cats))
	for _, c := range cats {
		base.Items = append(base.Items, categoryRowOf(c))
	}
	return base, nil
}

func (s *Server) registerCategoryRoutes() {
	s.handleAuthed("POST /settings/categories", s.postCategory)
	s.handleAuthed("GET /settings/categories/{id}", s.getCategoryRow)
	s.handleAuthed("GET /settings/categories/{id}/edit", s.getCategoryEdit)
	s.handleAuthed("POST /settings/categories/{id}", s.postCategoryUpdate)
	s.handleAuthed("DELETE /settings/categories/{id}", s.deleteCategory)
	s.handleAuthed("POST /settings/categories/{id}/delete", s.deleteCategory)
}

// respondCategories answers with the card for htmx, or the whole page.
func (s *Server) respondCategories(w http.ResponseWriter, r *http.Request, status int, v categoriesView) {
	if isHTMX(r) {
		s.renderComponent(w, r, status, CategoriesCard(v))
		return
	}
	d := pathsPageData{Paths: pathsViewFrom(s.Settings.Current().Paths, nil), Categories: v}
	s.renderSettingsPage(w, r, status, "paths", d)
}

func (s *Server) postCategory(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Categories != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	form := categoriesView{Name: strings.TrimSpace(r.PostForm.Get("name")), Dirs: readCategoryDirs(r.PostForm)}
	if (core.Category{Name: form.Name}).Validate() != nil {
		form.NameError = categoryNameMsg
	}
	if form.NameError == "" && !form.Dirs.invalid() {
		_, err := s.Categories.Create(r.Context(), form.Name, form.Dirs.Usenet, form.Dirs.Torrent)
		if ae, ok := errors.AsType[*app.Error](err); ok && ae.Code == "name_taken" {
			form.NameError = "A category with this name already exists."
		} else if ok {
			form.Error = ae.Message
		} else if err != nil {
			s.settingsFail(w, r, "category", err)
			return
		}
		if err == nil {
			form = categoriesView{}
			trigger(w, "success", "Category added.", nil)
		}
	}
	status := http.StatusOK
	if form.NameError != "" || form.Dirs.invalid() || form.Error != "" {
		status = http.StatusUnprocessableEntity
	}
	v, err := s.categoriesViewFor(r, form)
	if err != nil {
		s.settingsFail(w, r, "category", err)
		return
	}
	s.respondCategories(w, r, status, v)
}

// findCategory returns the row for an ID from the list of categories.
func (s *Server) findCategory(r *http.Request) (categoryRow, bool, error) {
	cats, err := s.Categories.List(r.Context())
	if err != nil {
		return categoryRow{}, false, err
	}
	id := core.ID(r.PathValue("id"))
	for _, c := range cats {
		if c.ID == id {
			return categoryRowOf(c), true, nil
		}
	}
	return categoryRow{}, false, nil
}

func (s *Server) categoryRowFragment(w http.ResponseWriter, r *http.Request, draw func(categoryRow) templ.Component) {
	if !s.settingsServices(w, r, s.Categories != nil) {
		return
	}
	row, ok, err := s.findCategory(r)
	switch {
	case err != nil:
		s.settingsFail(w, r, "category", err)
	case !ok:
		s.settingsFail(w, r, "category", core.ErrNotFound)
	case !isHTMX(r):
		s.redirect(w, r, "/settings/paths")
	default:
		s.renderComponent(w, r, http.StatusOK, draw(row))
	}
}

func (s *Server) getCategoryRow(w http.ResponseWriter, r *http.Request) {
	s.categoryRowFragment(w, r, CategoryRow)
}

func (s *Server) getCategoryEdit(w http.ResponseWriter, r *http.Request) {
	s.categoryRowFragment(w, r, CategoryEditRow)
}

// postCategoryUpdate changes the folder overrides of a category.
func (s *Server) postCategoryUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Categories != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	row, ok, err := s.findCategory(r)
	if err != nil {
		s.settingsFail(w, r, "category", err)
		return
	}
	if !ok {
		s.settingsFail(w, r, "category", core.ErrNotFound)
		return
	}
	row.Dirs = readCategoryDirs(r.PostForm)
	if row.Dirs.invalid() {
		s.renderComponent(w, r, http.StatusUnprocessableEntity, CategoryEditRow(row))
		return
	}
	updated, err := s.Categories.Update(r.Context(), core.ID(row.ID), app.CategoryPatch{UsenetDir: &row.Dirs.Usenet, TorrentDir: &row.Dirs.Torrent})
	if ae, isApp := errors.AsType[*app.Error](err); isApp {
		row.Dirs.UsenetError = ae.Message
		s.renderComponent(w, r, http.StatusUnprocessableEntity, CategoryEditRow(row))
		return
	} else if err != nil {
		s.settingsFail(w, r, "category", err)
		return
	}
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/paths")
		return
	}
	row = categoryRowOf(updated)
	trigger(w, "success", "Category saved. It applies to new downloads.", nil)
	s.renderComponent(w, r, http.StatusOK, CategoryRow(row))
}

func (s *Server) deleteCategory(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Categories != nil) {
		return
	}
	if err := s.Categories.Delete(r.Context(), core.ID(r.PathValue("id"))); err != nil {
		s.settingsFail(w, r, "category", err)
		return
	}
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/paths")
		return
	}
	v, err := s.categoriesViewFor(r, categoriesView{})
	if err != nil {
		s.settingsFail(w, r, "category", err)
		return
	}
	trigger(w, "success", "Category deleted. Its downloads keep their files.", nil)
	s.renderComponent(w, r, http.StatusOK, CategoriesCard(v))
}
