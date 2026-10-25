package sabnzbd

import (
	"strings"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

// addRequest reads the parameters addfile and addurl share. SAB's pp and
// script parameters are ignored: bitnews always verifies, repairs and
// unpacks, and runs no scripts. An unknown cat is created, as SAB does and
// Sonarr relies on; "*" and empty mean none.
func (c *call) addRequest() app.AddRequest {
	prio, paused := parsePriority(c.get("priority"))
	cat := strings.TrimSpace(c.params.Get("cat"))
	if cat == defaultCategoryName {
		cat = ""
	}
	return app.AddRequest{
		Name:         c.get("nzbname"),
		Priority:     prio,
		Paused:       paused,
		Password:     core.Secret(c.params.Get("password")),
		CategoryName: cat,
		Only:         core.SourceNZB,
	}
}

// addFile answers mode=addfile: the NZB was received from the multipart
// file part (field "name" or "nzbfile", in fact any file part) while the
// body was read.
func (c *call) addFile() {
	if c.staged == nil {
		c.fail("No file given")
		return
	}
	req := c.addRequest()
	req.Upload, req.FileName = c.staged, c.fileName
	c.finishAdd(req)
}

// addURL answers mode=addurl: name is the URL of the NZB. The URL is fetched
// now, never stored, and only logged redacted.
func (c *call) addURL() {
	u := c.get("name")
	if u == "" {
		c.fail("expect one parameter")
		return
	}
	req := c.addRequest()
	req.URL = u
	c.finishAdd(req)
}

func (c *call) finishAdd(req app.AddRequest) {
	d, err := c.h.Adder.Add(c.ctx(), req)
	if err != nil {
		c.appFail(err)
		return
	}
	c.json(idsBody{Status: true, NzoIDs: []string{nzoID(d.ID)}})
}
