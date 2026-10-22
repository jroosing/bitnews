package web

import (
	"net/http"
	"path/filepath"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// categoriesView is the "Categories" card: the table and the add form.
type categoriesView struct {
	CSRF  string
	Items []categoryRow
	// The add form: what was typed and what was wrong with it.
	Name, Dir           string
	NameError, DirError string
	Error               string
}

// categoryRow is one row of the table, read-only or being edited.
type categoryRow struct {
	CSRF  string
	ID    string
	Name  string
	Dir   string // the override; empty means the default folder
	Error string
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
	base.CSRF = settingsCSRF(r)
	base.Items = make([]categoryRow, 0, len(cats))
	for _, c := range cats {
		base.Items = append(base.Items, categoryRow{CSRF: base.CSRF, ID: string(c.ID), Name: c.Name, Dir: c.DirOverride})
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

// validCategoryInput checks a name and the shape of a folder (never the disk),
// but reports both problems at once, each next to its field.
func validCategoryInput(name, dir string, checkName bool) (nameErr, dirErr string) {
	if checkName && (core.Category{Name: name}).Validate() != nil {
		nameErr = categoryNameMsg
	}
	if dir != "" {
		dirErr = pathShapeError(dir, categoryDirMsg)
	}
	return nameErr, dirErr
}

// cleanCategoryDir drops a trailing or doubled separator from a folder that
// passed the shape check. Empty (the default folder) stays empty.
func cleanCategoryDir(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Clean(dir)
}

// respondCategories answers with the card for htmx, or the whole page.
func (s *Server) respondCategories(w http.ResponseWriter, r *http.Request, status int, v categoriesView) {
	if isHTMX(r) {
		s.renderSettingsPart(w, status, settingsPathsPage, "categories_card", v)
		return
	}
	d := pathsPageData{Paths: pathsViewFrom(settingsCSRF(r), s.Settings.Current().Paths, nil), Categories: v}
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
	form := categoriesView{Name: strings.TrimSpace(r.PostForm.Get("name")), Dir: strings.TrimSpace(r.PostForm.Get("dir_override"))}
	form.NameError, form.DirError = validCategoryInput(form.Name, form.Dir, true)
	if form.NameError == "" && form.DirError == "" {
		_, err := s.Categories.Create(r.Context(), form.Name, cleanCategoryDir(form.Dir))
		if ae, ok := settingsAppError(err); ok && ae.Code == "name_taken" {
			form.NameError = "A category with this name already exists."
		} else if ok {
			form.Error = ae.Message
		} else if err != nil {
			s.settingsFail(w, r, "category", err)
			return
		}
		if err == nil {
			form = categoriesView{}
			settingsTrigger(w, "success", "Category added.", nil)
		}
	}
	status := http.StatusOK
	if form.NameError != "" || form.DirError != "" || form.Error != "" {
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
			return categoryRow{CSRF: settingsCSRF(r), ID: string(c.ID), Name: c.Name, Dir: c.DirOverride}, true, nil
		}
	}
	return categoryRow{}, false, nil
}

func (s *Server) categoryRowFragment(w http.ResponseWriter, r *http.Request, name string) {
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
		s.renderSettingsPart(w, http.StatusOK, settingsPathsPage, name, row)
	}
}

func (s *Server) getCategoryRow(w http.ResponseWriter, r *http.Request) {
	s.categoryRowFragment(w, r, "category_row")
}

func (s *Server) getCategoryEdit(w http.ResponseWriter, r *http.Request) {
	s.categoryRowFragment(w, r, "category_edit_row")
}

// postCategoryUpdate changes the folder override of a category.
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
	dir := strings.TrimSpace(r.PostForm.Get("dir_override"))
	if _, dirErr := validCategoryInput(row.Name, dir, false); dirErr != "" {
		row.Dir, row.Error = dir, dirErr
		s.renderSettingsPart(w, http.StatusUnprocessableEntity, settingsPathsPage, "category_edit_row", row)
		return
	}
	dir = cleanCategoryDir(dir)
	updated, err := s.Categories.Update(r.Context(), core.ID(row.ID), app.CategoryPatch{DirOverride: &dir})
	if ae, isApp := settingsAppError(err); isApp {
		row.Dir, row.Error = dir, ae.Message
		s.renderSettingsPart(w, http.StatusUnprocessableEntity, settingsPathsPage, "category_edit_row", row)
		return
	} else if err != nil {
		s.settingsFail(w, r, "category", err)
		return
	}
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/paths")
		return
	}
	row.Dir, row.Error = updated.DirOverride, ""
	settingsTrigger(w, "success", "Category saved. It applies to new downloads.", nil)
	s.renderSettingsPart(w, http.StatusOK, settingsPathsPage, "category_row", row)
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
	settingsTrigger(w, "success", "Category deleted. Its downloads keep their files.", nil)
	s.renderSettingsPart(w, http.StatusOK, settingsPathsPage, "categories_card", v)
}
