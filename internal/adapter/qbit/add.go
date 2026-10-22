package qbit

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// add serves torrents/add: the urls field (magnet links and http(s) URLs of
// .torrent files, one per line) and any number of uploaded .torrent files.
// The answer is "Ok." when at least one torrent was added and "Fails."
// otherwise, always with HTTP 200, as qBittorrent does. The session was
// checked before the body is read.
//
// Of the options, category (created when unknown), paused or stopped,
// savepath (only under a torrent root), ratioLimit and seedingTimeLimit are
// honoured. tags, sequentialDownload, firstLastPiecePrio, autoTMM, rename,
// the speed limits and the rest are ignored.
//
// URLs can hold passkeys: they are handed to the TorrentAdder and are never
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
		hash, err := c.h.Adder.AddTorrentFile(c.ctx(), data, opts)
		if c.addFailed(err, "file") {
			continue
		}
		added = true
		c.h.log.Info("qbit: torrent added", "hash", hash, "source", "file")
	}
	for _, u := range urls {
		hash, err := c.h.Adder.AddURL(c.ctx(), u, opts)
		if c.addFailed(err, "url") {
			continue
		}
		added = true
		c.h.log.Info("qbit: torrent added", "hash", hash, "source", "url")
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
		return printable(strings.ToLower(u[:i]), 16)
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

// addOptions reads the options of an add call. A Category that cannot be
// created fails the call ("Fails.").
func (c *call) addOptions() (AddOptions, bool) {
	var o AddOptions
	if name := c.get("category"); name != "" {
		id, err := c.h.ensureCategory(c, name)
		if err != nil {
			if errors.Is(err, app.ErrInvalidInput) {
				c.h.log.Info("qbit: add refused, bad category", "category", printable(name, 40))
				c.text(200, "Fails.")
				return o, false
			}
			c.appFail(err)
			return o, false
		}
		o.CategoryID = id
	}
	o.Paused = isTrue(c.get("paused")) || isTrue(c.get("stopped"))
	if p := c.get("savepath"); p != "" {
		if safe, ok := c.h.safeSavePath(p); ok {
			o.SavePath = safe
		} else {
			c.h.log.Info("qbit: savepath ignored, not under a torrent root", "savepath", printable(p, 120))
		}
	}
	ratio, okR := shareArg(c.params.Get("ratioLimit"), true)
	minutes, okM := shareArg(c.params.Get("seedingTimeLimit"), false)
	if !okR {
		ratio = -2
	}
	if !okM {
		minutes = -2
	}
	if r, secs, global := c.h.resolveShare(ratio, minutes); !global {
		o.ShareLimit = &ShareLimit{Ratio: r, SeedingSeconds: secs}
	}
	return o, true
}

// safeSavePath accepts an absolute path that lies in one of the two torrent
// roots, checked lexically and against symlinks (fsutil.SafeJoin). Anything
// else is not accepted.
func (h *handler) safeSavePath(p string) (string, bool) {
	if !filepath.IsAbs(p) || strings.IndexByte(p, 0) >= 0 {
		return "", false
	}
	p = filepath.Clean(p)
	roots := h.Settings.Current().Paths.Torrent
	for _, root := range []string{roots.Complete, roots.Incomplete} {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if _, err := fsutil.SafeJoin(root, rel); err != nil {
			continue
		}
		return p, true
	}
	return "", false
}
