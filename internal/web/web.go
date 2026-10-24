package web

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strings"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// Deps is what the UI needs. Later pages add their services here; New
// refuses to start without the ones marked required.
type Deps struct {
	Logger *slog.Logger
	// Version is shown in the sidebar.
	Version string

	// Authentication (required): the same instances the API uses.
	Sessions *auth.SessionService
	UIAuth   *auth.UIAuth
	Setup    *auth.Setup
	// Settings (required) provides the allowed hosts for the CSRF Origin
	// check and the trusted proxies for the client IP.
	Settings *app.SettingsService

	// Application services for the pages (optional until a page needs them).
	Downloads *app.DownloadService
	// View gives each Download's phase, progress, rates, ETA, stage, torrent
	// facts and failure. Required with Downloads.
	View       *view.Module
	Categories *app.CategoryService
	Servers    *app.ServerService
	Bus        *core.Bus
	// Adder adds NZBs and torrents (the "Add download" dialog).
	Adder *app.AddService
	// AppKey is the one API key of the app, shown on Settings → Security.
	AppKey *auth.AppKeyService
	// Runtime reports what the Queues are doing right now (speeds, pause
	// state, limits). Torrents gives a torrent's files and sets its Share
	// Limit. Both are filled in by the daemon; see sources.go. They may be
	// nil in tests, and pages must cope with that.
	Runtime  RuntimeSource
	Torrents TorrentSource
	// Live tunes the /ui/events stream (queue_events.go); the zero value is
	// what production uses.
	Live LiveConfig
	// Detail reports Usenet fetch details for the Download detail page.
	Detail DetailSource
	// Config reports what the daemon runs with right now: the settings it
	// started with (for the "restart needed" notes) and the live status of
	// the network bindings. Optional; the settings pages cope without it.
	Config ConfigSource
}

// Server is the UI: it implements http.Handler.
type Server struct {
	Deps
	assets *assets
	tpl    *templates
	mux    *http.ServeMux
	csrf   func(http.Handler) http.Handler

	pages  []Page
	live   liveHub             // the /ui/events streams (queue_events.go)
	routes map[string][]string // path -> methods, for 405 answers
	// interfaceBinding is whether this host can bind sockets to a network
	// interface (Linux); the network binding settings refuse an interface
	// where it is false, because bitnews would not start with one.
	interfaceBinding bool
	root             http.Handler
}

// New builds the UI handler. It serves every path outside /api/ and
// /sabnzbd/: pages, fragments, /static/ and a 404 page for the rest.
func New(deps Deps) (*Server, error) {
	if deps.Sessions == nil || deps.UIAuth == nil || deps.Setup == nil || deps.Settings == nil {
		return nil, errors.New("web: Deps is missing a required service")
	}
	if deps.Downloads != nil && deps.View == nil {
		return nil, errors.New("web: Deps.View is required with Deps.Downloads")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	static, err := subFS("assets/static")
	if err != nil {
		return nil, err
	}
	a, err := loadAssets(static, ".")
	if err != nil {
		return nil, err
	}
	tfs, err := subFS("assets/templates")
	if err != nil {
		return nil, err
	}
	tpl, err := loadTemplates(tfs, templateFuncs(a))
	if err != nil {
		return nil, err
	}
	s := &Server{
		Deps: deps, assets: a, tpl: tpl, mux: http.NewServeMux(),
		csrf:   auth.CSRF(deps.Settings.Current),
		routes: map[string][]string{},
		// SO_BINDTODEVICE is Linux only (docs/network-binding.md).
		interfaceBinding: runtime.GOOS == "linux",
	}
	s.registerRoutes()
	s.root = secure(http.HandlerFunc(s.dispatch))
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.root.ServeHTTP(w, r) }

// registerRoutes lists every route. A later page adds its own
// routes_<name>.go with a method called from here.
func (s *Server) registerRoutes() {
	s.mux.Handle("GET "+staticPrefix, s.assets)

	s.handle("GET /{$}", http.HandlerFunc(s.getRoot))
	s.handleOpen("GET /login", s.getLogin)
	s.handleOpen("POST /login", s.postLogin)
	s.handleAuthed("POST /logout", s.postLogout)
	s.handleOpen("GET /setup", s.getSetup)
	s.handleOpen("POST /setup", s.postSetup)

	for _, p := range builtinPages() {
		s.addPage(p)
	}
	s.registerQueue()
	s.registerSettings()
	s.registerDownloadRoutes()

	s.mux.HandleFunc("/", s.notFound)
}

// handle registers a route and remembers its method for 405 answers.
func (s *Server) handle(pattern string, h http.Handler) {
	method, path, _ := strings.Cut(pattern, " ")
	s.routes[path] = append(s.routes[path], method)
	if method == http.MethodGet {
		s.routes[path] = append(s.routes[path], http.MethodHead)
	}
	s.mux.Handle(pattern, h)
}

// handleOpen registers a route without authentication (login, setup). Unsafe
// methods still get the CSRF Origin check, and the JSON errors it produces
// become UI responses.
func (s *Server) handleOpen(pattern string, h http.HandlerFunc) {
	s.handle(pattern, formCSRF(s.htmlErrors(s.csrf(h))))
}

// handleAuthed registers a route behind UIAuth and CSRF: a session, or the
// disable-auth / LAN bypass principal. Unauthenticated requests are
// redirected to the login page.
func (s *Server) handleAuthed(pattern string, h http.HandlerFunc) {
	s.handle(pattern, formCSRF(s.htmlErrors(s.UIAuth.Middleware(s.csrf(h)))))
}

// dispatch routes the request through the mux and answers 405 for a known
// path with the wrong method.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if methods, ok := s.routes[r.URL.Path]; ok {
		w.Header().Set("Allow", strings.Join(methods, ", "))
		s.renderError(w, r, http.StatusMethodNotAllowed, "Method not allowed", "This address does not accept that kind of request.")
		return
	}
	s.renderError(w, r, http.StatusNotFound, "Page not found", "There is nothing at this address.")
}

// knownPath reports whether path is a UI page (or below one), the only
// places a post-login redirect may lead.
func (s *Server) knownPath(path string) bool {
	return slices.ContainsFunc(s.pages, func(p Page) bool {
		return path == p.Path || strings.HasPrefix(path, p.Path+"/")
	})
}
