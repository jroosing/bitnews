package sabnzbd

import (
	"errors"
	"strings"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

// addRequest reads the parameters addfile and addurl share. The category is
// resolved later, once the NZB is known to be valid. SAB's pp and script
// parameters are ignored: bitnews always verifies, repairs and unpacks, and
// runs no scripts.
func (c *call) addRequest() app.AddRequest {
	prio, paused := parsePriority(c.get("priority"))
	return app.AddRequest{
		Name:     c.get("nzbname"),
		Priority: prio,
		Paused:   paused,
		Password: core.Secret(c.params.Get("password")),
	}
}

// addFile answers mode=addfile: the NZB was staged from the multipart file
// part (field "name" or "nzbfile", in fact any file part) while the body was
// read.
func (c *call) addFile() {
	if c.staged == nil {
		c.fail("No file given")
		return
	}
	req := c.addRequest()
	req.FileName = c.fileName
	c.finishAdd(c.staged, req)
}

// addURL answers mode=addurl: name is the URL of the NZB. The URL is fetched
// now, never stored, and only logged redacted (app.AddService.FetchNZB).
func (c *call) addURL() {
	u := c.get("name")
	if u == "" {
		c.fail("expect one parameter")
		return
	}
	req := c.addRequest()
	// Refuse bad priorities and passwords before fetching anything.
	if err := c.h.Adder.Check(c.ctx(), req); err != nil {
		c.appFail(err)
		return
	}
	st, err := c.h.Adder.FetchNZB(c.ctx(), u)
	if err != nil {
		c.appFail(err)
		return
	}
	c.staged = st // discarded by ServeHTTP if finishAdd fails
	c.finishAdd(st, req)
}

func (c *call) finishAdd(st *app.StagedNZB, req app.AddRequest) {
	id, err := c.h.ensureCategory(c, c.params.Get("cat"))
	if err != nil {
		c.appFail(err)
		return
	}
	req.CategoryID = id
	d, err := c.h.Adder.Add(c.ctx(), st, req)
	if err != nil {
		c.appFail(err)
		return
	}
	c.json(idsBody{Status: true, NzoIDs: []string{nzoID(d.ID)}})
}

// ensureCategory resolves the cat parameter. "*" and empty mean none. A name
// that matches a Category (ignoring case) is used. An unknown name creates
// the Category with the default dir: SAB does the same and Sonarr relies on
// it. This is the only place a Category is ever auto-created.
func (h *handler) ensureCategory(c *call, name string) (core.ID, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == defaultCategoryName {
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
			h.log.Info("sabnzbd: category created", "category", created.Name)
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
