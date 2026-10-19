package sabnzbd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// DefaultVersion is the SABnzbd version the adapter reports. Sonarr and
// Radarr only need 3.0.0 or newer (Prowlarr 2.0.0); a current 4.x release
// also avoids their "upgrade SABnzbd" warnings.
const DefaultVersion = "4.5.0"

// Request size limits. Only addfile carries a large body.
const (
	maxFormBytes   = 1 << 20
	maxUploadBytes = app.DefaultMaxNZBBytes + 1<<20
	maxFieldBytes  = 16 << 10
	maxParts       = 32
	// pageSize is the page size used when reading Downloads from the
	// service; the repository caps pages at this size.
	pageSize = 200
)

// SettingsReader gives the adapter the Settings in effect.
// *app.SettingsService is one.
type SettingsReader interface {
	Current() core.Settings
}

// Deps is everything the adapter needs. Credentials, Downloads, Adder,
// Categories and Settings are required; the rest are optional.
type Deps struct {
	Credentials *auth.AdapterCredentialService
	Downloads   *app.DownloadService
	Adder       *app.AddService
	Categories  *app.CategoryService
	Settings    SettingsReader

	// StageSource reports the Post-processing stage a Download is in, which
	// decides whether its history status is Verifying, Repairing,
	// Extracting or Moving. false means unknown, which shows as Verifying.
	// The daemon feeds it from core.PostProcessStage events.
	StageSource func(core.ID) (core.Stage, bool)
	// SpeedSource reports the current download rate and whether downloading
	// is paused as a whole. Without it the speed is 0 and every time left is
	// "0:00:00".
	SpeedSource func() (bytesPerSec int64, paused bool)
	// DiskSpace reports the free and total bytes of the filesystem that
	// holds path. Without it the disk space fields are "0".
	DiskSpace func(path string) (free, total int64, ok bool)

	Logger *slog.Logger
	// Version is what mode=version reports. The default is DefaultVersion.
	Version string
	// Clock defaults to time.Now.
	Clock func() time.Time
}

// New returns the handler for /sabnzbd/api. It panics if a required
// dependency is missing, since that is a wiring bug found at startup.
func New(d Deps) http.Handler {
	if d.Credentials == nil || d.Downloads == nil || d.Adder == nil || d.Categories == nil || d.Settings == nil {
		panic("sabnzbd: Deps is missing a required service")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Version == "" {
		d.Version = DefaultVersion
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	return &handler{Deps: d, log: d.Logger}
}

type handler struct {
	Deps
	log *slog.Logger
}

// call is one request in flight.
type call struct {
	h *handler
	w http.ResponseWriter
	r *http.Request
	// params are the query parameters, then the form fields the query did
	// not set.
	params url.Values

	// staged is the uploaded NZB of an addfile call, with its file name.
	staged   *app.StagedNZB
	fileName string

	authDone, authOK bool
}

func (c *call) ctx() context.Context { return c.r.Context() }

func (c *call) get(name string) string { return strings.TrimSpace(c.params.Get(name)) }

// ServeHTTP dispatches on the mode parameter.
//
// A call is authenticated by its apikey parameter, which must be an
// adapter_sab credential: "API Key Required" without one and "API Key
// Incorrect" with a wrong, revoked or foreign one, both as HTTP 200 like
// SABnzbd. mode=version needs no key. The output parameter is ignored: the
// answer is always JSON.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	c := &call{h: h, w: w, r: r, params: r.URL.Query()}
	defer func() { c.staged.Discard() }() // after a successful Add this does nothing

	if !c.readBody() {
		return
	}
	mode := strings.ToLower(c.get("mode"))
	if mode == "version" {
		c.json(versionBody{Version: h.Version})
		return
	}
	if !c.authorize() {
		return
	}
	switch mode {
	case "get_config":
		c.getConfig()
	case "get_cats":
		c.getCats()
	case "fullstatus", "status":
		c.fullStatus()
	case "addfile":
		c.addFile()
	case "addurl":
		c.addURL()
	case "queue":
		c.queueMode()
	case "history":
		c.historyMode()
	case "retry":
		c.fail("retry is not supported")
	default:
		h.log.Debug("sabnzbd: mode not implemented", "mode", printable(mode, 32))
		c.fail("not implemented")
	}
}

// printable shortens s and drops everything but printable ASCII, for logs.
func printable(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return '?'
		}
		return r
	}, s)
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// readBody reads the form fields of a POST into c.params and, for a
// multipart upload, stages the NZB file part of an addfile call. It answers
// the request itself and returns false when the body is unusable.
func (c *call) readBody() bool {
	mt, _, _ := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	switch mt {
	case "multipart/form-data":
		c.r.Body = http.MaxBytesReader(c.w, c.r.Body, maxUploadBytes)
		return c.readMultipart()
	case "application/x-www-form-urlencoded":
		c.r.Body = http.MaxBytesReader(c.w, c.r.Body, maxFormBytes)
		if err := c.r.ParseForm(); err != nil {
			c.bodyFailed(err)
			return false
		}
		for k, vs := range c.r.PostForm {
			if !c.params.Has(k) {
				c.params[k] = vs
			}
		}
	}
	return true
}

// readMultipart streams the parts. Text fields join the params (the query
// wins). The first file part of an addfile call is staged right away, so the
// NZB is never held in memory, but only after the caller is authenticated:
// an apikey that comes after the file in the body is too late. File parts of
// other modes, and further file parts, are skipped.
func (c *call) readMultipart() bool {
	mr, err := c.r.MultipartReader()
	if err != nil {
		c.fail("Invalid multipart request")
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
			c.fail("Too many form fields")
			return false
		}
		if part.FileName() != "" {
			if c.staged != nil || strings.ToLower(c.get("mode")) != "addfile" {
				continue
			}
			if !c.authorize() {
				return false
			}
			st, err := c.h.Adder.Stage(c.ctx(), part)
			if err != nil {
				c.appFail(err)
				return false
			}
			c.staged, c.fileName = st, part.FileName()
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
		if err != nil {
			c.bodyFailed(err)
			return false
		}
		if len(raw) > maxFieldBytes {
			c.fail("Form field too long")
			return false
		}
		if name := part.FormName(); name != "" && !c.params.Has(name) {
			c.params.Set(name, string(raw))
		}
	}
}

func (c *call) bodyFailed(err error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		c.fail("Request too large")
		return
	}
	c.fail("Invalid request body")
}

// authorize checks the apikey once per call. On failure it has answered.
func (c *call) authorize() bool {
	if c.authDone {
		return c.authOK
	}
	c.authDone = true
	key := c.get("apikey")
	if key == "" {
		c.fail("API Key Required")
		return false
	}
	_, err := c.h.Credentials.Authenticate(c.ctx(), auth.KindAdapterSAB, key)
	switch {
	case err == nil:
		c.authOK = true
		return true
	case errors.Is(err, auth.ErrInvalidAdapterCredential):
		c.fail("API Key Incorrect")
	default:
		c.h.log.Error("sabnzbd: authentication failed", "err", err)
		c.w.WriteHeader(http.StatusInternalServerError)
		c.encode(errorBody{Error: "Internal error"})
	}
	return false
}

// json answers 200 with v.
func (c *call) json(v any) { c.encode(v) }

// fail answers SAB's error shape: HTTP 200, status false.
func (c *call) fail(msg string) { c.encode(errorBody{Status: false, Error: msg}) }

func (c *call) encode(v any) { _ = json.NewEncoder(c.w).Encode(v) }

// appFail turns an error of the app layer into SAB's error shape. Errors a
// client can act on carry safe messages; anything else is logged and
// reported generically.
func (c *call) appFail(err error) {
	if appErr, ok := errors.AsType[*app.Error](err); ok {
		c.fail(appErr.Message)
		return
	}
	if errors.Is(err, context.Canceled) {
		return // the caller went away
	}
	c.h.log.Error("sabnzbd: request failed", "err", err)
	c.fail("Internal error")
}
