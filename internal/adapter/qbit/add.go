package qbit

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/redact"
)

// add serves torrents/add: the urls field (magnet links and http(s) URLs of
// .torrent files, one per line) and any number of uploaded .torrent files.
// The answer is "Ok." when at least one torrent was added and "Fails."
// otherwise, always with HTTP 200, as qBittorrent does. The session was
// checked before the body is read.
//
// Of the options, category (created when unknown), paused or stopped,
// ratioLimit and seedingTimeLimit are honoured. savepath is ignored (a
// Download goes to its Category's directory), and so are tags,
// sequentialDownload, firstLastPiecePrio, autoTMM, rename, the speed limits
// and the rest.
//
// URLs can hold passkeys: they are handed to the AddService and are never
// logged, stored or echoed here.
func (c *call) add() {
	if !c.readAddBody() {
		return
	}
	urls, ok := c.addURLs()
	if !ok {
		return
	}
	if len(urls)+len(c.uploads) == 0 {
		c.text(200, "Fails.")
		return
	}
	opts, ok := c.addOptions()
	if !ok {
		return
	}
	added := false
	for _, data := range c.uploads {
		up, err := c.h.Adder.Receive(c.ctx(), bytes.NewReader(data), "", core.SourceTorrent)
		if err == nil {
			req := opts
			req.Upload = up
			err = c.addOne(req, "file")
		}
		added = !c.addFailed(err, "file") || added
	}
	for _, u := range urls {
		req := opts
		req.URL = u
		added = !c.addFailed(c.addOne(req, "url"), "url") || added
	}
	if c.ctx().Err() != nil {
		return
	}
	if added {
		c.text(200, "Ok.")
		return
	}
	c.text(200, "Fails.")
}

// addOne adds one torrent and logs it.
func (c *call) addOne(req app.AddRequest, kind string) error {
	d, err := c.h.Adder.Add(c.ctx(), req)
	if err == nil {
		c.h.log.Info("qbit: torrent added", "id", d.ID, "source", kind)
	}
	return err // a duplicate matches core.ErrConflict
}

// addFailed logs a failed add (the error text is masked: it may quote the
// URL) and reports whether there was one.
func (c *call) addFailed(err error, kind string) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, core.ErrConflict):
		c.h.log.Info("qbit: torrent not added, it exists already", "source", kind)
	default:
		c.h.log.Warn("qbit: torrent not added", "source", kind, "err", maskURLs(err.Error()))
	}
	return true
}

// readAddBody reads the form fields and the .torrent parts. It answers the
// request itself and returns false when the body is unusable.
func (c *call) readAddBody() bool {
	mt, _, _ := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		return c.readForm()
	}
	c.r.Body = http.MaxBytesReader(c.w, c.r.Body, maxUploadBytes)
	mr, err := c.r.MultipartReader()
	if err != nil {
		c.text(http.StatusBadRequest, "Bad Request")
		return false
	}
	for n := 0; ; n++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			c.bodyFailed(err)
			return false
		}
		if n >= maxParts {
			c.text(http.StatusBadRequest, "Bad Request")
			return false
		}
		if part.FileName() != "" {
			if len(c.uploads) >= maxFiles {
				c.text(http.StatusBadRequest, "Bad Request")
				return false
			}
			data, err := io.ReadAll(io.LimitReader(part, maxTorrentBytes+1))
			if err != nil {
				c.bodyFailed(err)
				return false
			}
			if len(data) > maxTorrentBytes {
				c.text(http.StatusRequestEntityTooLarge, "Request Entity Too Large")
				return false
			}
			if len(data) > 0 {
				c.uploads = append(c.uploads, data)
			}
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
		if err != nil {
			c.bodyFailed(err)
			return false
		}
		if len(raw) > maxFieldBytes {
			c.text(http.StatusRequestEntityTooLarge, "Request Entity Too Large")
			return false
		}
		if name := part.FormName(); name != "" {
			c.params.Set(name, string(raw))
		}
	}
}

// addURLs splits and checks the urls field. Only magnet links and http(s)
// URLs are accepted; the others are dropped (file:// and the like must never
// reach a fetcher). More than maxURLs is a bad request.
func (c *call) addURLs() ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(c.params.Get("urls"), "\n") {
		u := strings.TrimSpace(line)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if len(out) >= maxURLs {
			c.text(http.StatusBadRequest, "Bad Request")
			return nil, false
		}
		if !acceptableURL(u) {
			c.h.log.Info("qbit: url refused", "scheme", urlScheme(u))
			continue
		}
		out = append(out, u)
	}
	return out, true
}

func urlScheme(u string) string {
	if i := strings.IndexAny(u, ":/?"); i > 0 && u[i] == ':' {
		return redact.Printable(strings.ToLower(u[:i]), 16)
	}
	return ""
}

func acceptableURL(u string) bool {
	if len(u) > maxURLBytes || strings.ContainsFunc(u, unicode.IsControl) {
		return false
	}
	switch urlScheme(u) {
	case "magnet", "http", "https":
		return true
	}
	return false
}

// addOptions reads the options of an add call into the request every torrent
// of the call is added with. A Category that cannot be created fails the call
// ("Fails.").
func (c *call) addOptions() (app.AddRequest, bool) {
	req := app.AddRequest{Only: core.SourceTorrent}
	if name := c.get("category"); name != "" {
		id, err := c.h.ensureCategory(c, name)
		if err != nil {
			if errors.Is(err, app.ErrInvalidInput) {
				c.h.log.Info("qbit: add refused, bad category", "category", redact.Printable(name, 40))
				c.text(200, "Fails.")
				return req, false
			}
			c.appFail(err)
			return req, false
		}
		req.CategoryID = id
	}
	req.Paused = isTrue(c.get("paused")) || isTrue(c.get("stopped"))
	if c.get("savepath") != "" {
		c.h.log.Debug("qbit: the save path of a torrent add is ignored; the Download goes to its Category's directory")
	}
	ratio, okR := shareArg(c.params.Get("ratioLimit"), true)
	minutes, okM := shareArg(c.params.Get("seedingTimeLimit"), false)
	if !okR {
		ratio = -2
	}
	if !okM {
		minutes = -2
	}
	req.ShareLimit = c.h.resolveShare(ratio, minutes)
	return req, true
}
