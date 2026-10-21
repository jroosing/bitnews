package qbit

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// categoryJSON is one entry of torrents/categories.
type categoryJSON struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

// categorySavePath is where a Category's torrents land: its dir override, or
// its name below the torrent complete root.
func categorySavePath(root string, c core.Category) string {
	if c.DirOverride != "" {
		return c.DirOverride
	}
	return filepath.Join(root, c.Name)
}

// categories serves torrents/categories: an object keyed by name. The
// Categories are shared with Usenet.
func (c *call) categories() {
	cats, err := c.h.Categories.List(c.ctx())
	if err != nil {
		c.appFail(err)
		return
	}
	c.json(c.h.categoryMap(cats))
}

func (h *handler) categoryMap(cats []core.Category) map[string]categoryJSON {
	root := h.Settings.Current().Paths.Torrent.Complete
	out := make(map[string]categoryJSON, len(cats))
	for _, cat := range cats {
		out[cat.Name] = categoryJSON{Name: cat.Name, SavePath: categorySavePath(root, cat)}
	}
	return out
}

// createCategory serves torrents/createCategory. The save path is ignored: a
// Category is shared with Usenet and lands below the complete root of each
// protocol. A Category that exists (ignoring case) is not an error, so a
// media manager that spells the name differently still works.
func (c *call) createCategory() {
	name := c.get("category")
	if name == "" {
		c.text(http.StatusBadRequest, "Category name is empty")
		return
	}
	if _, err := c.h.ensureCategory(c, name); err != nil {
		if errors.Is(err, app.ErrInvalidInput) {
			c.text(http.StatusConflict, "Incorrect category name")
			return
		}
		c.appFail(err)
		return
	}
	c.ok()
}

// ensureCategory resolves a category name. A name that matches a Category
// (ignoring case) is used; an unknown name creates the Category with the
// default dir, as qBittorrent does on add. An empty name means none.
func (h *handler) ensureCategory(c *call, name string) (core.ID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	for attempt := range 2 {
		cats, err := h.Categories.List(c.ctx())
		if err != nil {
			return "", err
		}
		if cat, ok := findCategory(cats, name); ok {
			return cat.ID, nil
		}
		created, err := h.Categories.Create(c.ctx(), strings.ToLower(name), "")
		if err == nil {
			h.log.Info("qbit: category created", "category", created.Name)
			return created.ID, nil
		}
		// Someone created it between our List and Create: look again.
		if !errors.Is(err, core.ErrConflict) || attempt == 1 {
			return "", err
		}
	}
	return "", nil
}

func findCategory(cats []core.Category, name string) (core.Category, bool) {
	for _, cat := range cats {
		if strings.EqualFold(cat.Name, name) {
			return cat, true
		}
	}
	return core.Category{}, false
}
