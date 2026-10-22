package qbit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/redact"
)

// Versions the adapter reports. Sonarr and Radarr want qBittorrent 3.2.4 or
// newer; they switch from pause/resume to stop/start at web API 2.11, and the
// torrent states follow the same switch (pausedDL becomes stoppedDL), so the
// default stays below it. Both are overridable in Deps.
const (
	DefaultVersion       = "v4.6.7"
	DefaultWebAPIVersion = "2.9.3"
)

// Request limits.
const (
	// maxFormBytes caps urlencoded bodies: hashes lists and login forms.
	maxFormBytes = 1 << 20
	// maxLoginBytes caps the login form.
	maxLoginBytes = 8 << 10
	// maxTorrentBytes caps one uploaded .torrent file.
	maxTorrentBytes = 10 << 20
	// maxUploadBytes caps the whole multipart body of torrents/add.
	maxUploadBytes = 32 << 20
	// maxFieldBytes caps one text field of a multipart body.
	maxFieldBytes = 256 << 10
	// maxParts caps the parts of a multipart body.
	maxParts = 64
	// maxFiles and maxURLs cap what one torrents/add call may carry.
	maxFiles = 16
	maxURLs  = 32
	// maxURLBytes caps one URL or magnet link.
	maxURLBytes = 16 << 10
	// maxHashes caps the hashes of one call.
	maxHashes = 1000
	// pageSize is the page size used when reading Downloads.
	pageSize = 200
)

// SettingsReader gives the adapter the Settings in effect.
// *app.SettingsService is one.
type SettingsReader interface {
	Current() core.Settings
}

// Deps is everything the adapter needs. AppKey, Downloads, Categories,
// Settings, Torrents, Adder and ShareLimits are required; the rest are
// optional.
type Deps struct {
	AppKey      *auth.AppKeyService
	Downloads   *app.DownloadService
	Categories  *app.CategoryService
	Settings    SettingsReader
	Torrents    TorrentSource
	Adder       TorrentAdder
	ShareLimits ShareLimiter

	// Files answers torrents/files. Without it the list is empty.
	Files FileSource
	// GlobalShare reports the global Share Limit default for preferences.
	// Without it no global limit is reported.
	GlobalShare func() GlobalShare
	// Limiter throttles failed logins per client IP. The default is a
	// limiter of the adapter's own, so that failures here never lock the UI
	// or another adapter; pass the shared one to count them together.
	Limiter *auth.LoginLimiter
	// MaxSessions caps the SID sessions in memory. The default is 1000.
	MaxSessions int

	Logger *slog.Logger
	// Version and WebAPIVersion are what app/version and app/webapiVersion
	// report. The defaults are DefaultVersion and DefaultWebAPIVersion. A web
	// API version of 2.11 or newer also makes the states stoppedDL and
	// stoppedUP replace pausedDL and pausedUP.
	Version       string
	WebAPIVersion string
	// Clock defaults to time.Now.
	Clock func() time.Time
}

// New returns the handler for /api/v2/. It panics if a required dependency is
// missing, since that is a wiring bug found at startup.
func New(d Deps) http.Handler {
	if d.AppKey == nil || d.Downloads == nil || d.Categories == nil || d.Settings == nil ||
		d.Torrents == nil || d.Adder == nil || d.ShareLimits == nil {
		panic("qbit: Deps is missing a required service")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Version == "" {
		d.Version = DefaultVersion
	}
	if d.WebAPIVersion == "" {
		d.WebAPIVersion = DefaultWebAPIVersion
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Limiter == nil {
		d.Limiter = auth.NewLoginLimiter(auth.WithLimiterClock(d.Clock))
	}
	if d.MaxSessions <= 0 {
		d.MaxSessions = defaultMaxSessions
	}
	h := &handler{Deps: d, log: d.Logger, sessions: newSessions(d.Clock, d.MaxSessions)}
	h.stopped = webAPIAtLeast(d.WebAPIVersion, 2, 11)
	h.routes = routes()
	return h
}

type handler struct {
	Deps
	log      *slog.Logger
	sessions *sessions
	routes   map[string]endpoint
	// stopped makes the states say stopped where qBittorrent < 5 says paused.
	stopped bool
}

// endpoint is one API call.
type endpoint struct {
	// post means the call changes something and needs POST.
	post bool
	// public means no session is needed (login).
	public bool
	// upload means the body is multipart and read by the handler itself.
	upload bool
	fn     func(*call)
}

// call is one request in flight.
type call struct {
	h *handler
	w http.ResponseWriter
	r *http.Request
	// params are the query parameters, then the form fields the query did
	// not set.
	params url.Values
	// uploads are the .torrent parts of torrents/add.
	uploads [][]byte
	sid     string
}

func (c *call) ctx() context.Context { return c.r.Context() }

func (c *call) get(name string) string { return strings.TrimSpace(c.params.Get(name)) }

// ServeHTTP authenticates and dispatches. Without a valid SID every call
// except auth/login is 403 "Forbidden", whatever the path: the answer does
// not tell which endpoints exist. A POST (any method that is not safe) whose
// Origin or Referer names another host is 401 "Unauthorized".
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name, ok := strings.CutPrefix(r.URL.Path, "/api/v2/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	c := &call{h: h, w: w, r: r, params: r.URL.Query()}
	ep, known := h.routes[name]

	if known && ep.public {
		if !h.sameOrigin(r) {
			c.text(http.StatusUnauthorized, "Unauthorized")
			return
		}
		if r.Method != http.MethodPost {
			c.text(http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		ep.fn(c)
		return
	}
	if !c.authenticate() {
		c.text(http.StatusForbidden, "Forbidden")
		return
	}
	if !safeMethod(r.Method) && !h.sameOrigin(r) {
		c.text(http.StatusUnauthorized, "Unauthorized")
		return
	}
	if !known {
		c.text(http.StatusNotFound, "Not Found")
		return
	}
	if ep.post && r.Method != http.MethodPost || !ep.post && r.Method != http.MethodGet && r.Method != http.MethodPost {
		c.w.Header().Set("Allow", allowed(ep))
		c.text(http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	if !ep.upload && !c.readForm() {
		return
	}
	ep.fn(c)
}

func allowed(ep endpoint) string {
	if ep.post {
		return http.MethodPost
	}
	return http.MethodGet + ", " + http.MethodPost
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// readForm merges the fields of an urlencoded POST body into c.params. Other
// content types carry nothing the adapter reads.
func (c *call) readForm() bool {
	if c.r.Method != http.MethodPost || !isForm(c.r) {
		return true
	}
	c.r.Body = http.MaxBytesReader(c.w, c.r.Body, maxFormBytes)
	if err := c.r.ParseForm(); err != nil {
		c.bodyFailed(err)
		return false
	}
	// the body wins, as in qBittorrent
	maps.Copy(c.params, c.r.PostForm)
	return true
}

func (c *call) bodyFailed(err error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		c.text(http.StatusRequestEntityTooLarge, "Request Entity Too Large")
		return
	}
	c.text(http.StatusBadRequest, "Bad Request")
}

// text answers with a plain text body.
func (c *call) text(status int, body string) {
	c.w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	c.w.WriteHeader(status)
	_, _ = c.w.Write([]byte(body))
}

// json answers 200 with v.
func (c *call) json(v any) {
	c.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(c.w).Encode(v)
}

// ok answers a call that succeeded and has nothing to say.
func (c *call) ok() { c.text(http.StatusOK, "") }

// appFail answers an error of the app layer. Errors a client can act on
// carry safe messages; anything else is logged and reported generically.
func (c *call) appFail(err error) {
	switch {
	case errors.Is(err, context.Canceled):
		return // the caller went away
	case errors.Is(err, core.ErrNotFound):
		c.text(http.StatusNotFound, "Not Found")
	case errors.Is(err, core.ErrConflict):
		c.text(http.StatusConflict, "Conflict")
	case errors.Is(err, app.ErrInvalidInput):
		if appErr, ok := errors.AsType[*app.Error](err); ok {
			c.text(http.StatusBadRequest, appErr.Message)
			return
		}
		c.text(http.StatusBadRequest, "Bad Request")
	default:
		c.h.log.Error("qbit: request failed", "err", maskURLs(err.Error()))
		c.text(http.StatusInternalServerError, "Internal Server Error")
	}
}

// ignorable says whether an action on one torrent failed in a way qBittorrent
// would not report: it is gone, or cannot take the action now.
func ignorable(err error) bool {
	return errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrConflict)
}

var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*:(?://|\?)[^\s"']+`)

// maskURLs masks every URL in s, for log lines: a URL can hold a passkey.
func maskURLs(s string) string {
	s = urlInText.ReplaceAllStringFunc(s, func(u string) string {
		if len(u) >= 7 && strings.EqualFold(u[:7], "magnet:") {
			return "magnet:?[redacted]" // trackers carry passkeys
		}
		return redact.URL(u)
	})
	return printable(s, 300)
}

// printable shortens s and replaces everything but printable ASCII, for
// logs.
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

func isNotFound(err error) bool { return errors.Is(err, core.ErrNotFound) }
