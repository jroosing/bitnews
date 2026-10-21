package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Version is what GET /api/v1/version reports.
type Version struct {
	Version string
	Commit  string
	// GoVersion defaults to the Go version the binary was built with.
	GoVersion string
}

// Config holds everything a Server needs. All fields are required except
// AdapterCredentials, SABnzbd, QBit, Web and the last four.
type Config struct {
	Logger     *slog.Logger
	Version    Version
	Downloads  *app.DownloadService
	Adder      *app.AddService
	Categories *app.CategoryService
	Servers    *app.ServerService
	Settings   *app.SettingsService
	Setup      *auth.Setup
	Sessions   *auth.SessionService
	UIAuth     *auth.UIAuth
	APIKeys    *auth.APIKeyService
	Bus        *core.Bus

	// AdapterCredentials manages the media-manager credentials behind
	// /api/v1/adapter-credentials. Without it those routes do not exist.
	AdapterCredentials *auth.AdapterCredentialService
	// SABnzbd serves the SABnzbd API emulation at /sabnzbd/api for every
	// method. It authenticates its own requests with adapter credentials,
	// so it sits outside the UI authentication and CSRF, and inside the
	// setup gate and the request ID, panic recovery and security header
	// middleware. Without it the path is a 404.
	SABnzbd http.Handler
	// QBit serves the qBittorrent WebUI API v2 emulation at /api/v2/ for
	// every method. It has its own SID sessions and adapter credentials, so
	// like SABnzbd it sits outside the UI authentication and CSRF, and
	// inside the setup gate and the request ID, panic recovery and security
	// header middleware. Without it the path is a 404.
	QBit http.Handler
	// Web serves the browser UI (internal/web) for every path outside
	// /api/ and /sabnzbd/: pages, /static/ and its own 404 page. It does its
	// own authentication, CSRF checks and security headers (a stricter CSP
	// than the API's, which stays unchanged). Unknown /api/ and /sabnzbd/
	// paths keep the JSON 404. Without it those paths are JSON 404s too.
	Web http.Handler

	// InstanceID identifies this process in the SSE hello event, so a
	// client can tell that the daemon restarted. It defaults to a random ID.
	InstanceID string
	// PingInterval is how often an idle event stream gets a ": ping"
	// comment. It defaults to 15 seconds.
	PingInterval time.Duration
	// NewTicker makes the ping ticker. Tests replace it with a fake; the
	// default wraps time.NewTicker.
	NewTicker func(d time.Duration) (c <-chan time.Time, stop func())
}

// Server is the own API v1: the handlers behind one middleware chain. Build
// it with New and run it with HTTPServer and Serve.
type Server struct {
	Config
	log     *slog.Logger
	mux     *http.ServeMux
	handler http.Handler

	// streams is cancelled when the HTTP server shuts down, which ends every
	// event stream so that Shutdown does not wait for clients that never
	// hang up.
	streams       context.Context
	cancelStreams context.CancelFunc
}

// New builds the Server and its routes.
func New(cfg Config) (*Server, error) {
	if cfg.Downloads == nil || cfg.Adder == nil || cfg.Categories == nil || cfg.Servers == nil || cfg.Settings == nil || cfg.Setup == nil ||
		cfg.Sessions == nil || cfg.UIAuth == nil || cfg.APIKeys == nil || cfg.Bus == nil {
		return nil, errors.New("api: Config is missing a service")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Version.GoVersion == "" {
		cfg.Version.GoVersion = runtime.Version()
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = string(core.NewID())
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 15 * time.Second
	}
	if cfg.NewTicker == nil {
		cfg.NewTicker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	s := &Server{Config: cfg, log: cfg.Logger, mux: http.NewServeMux()}
	s.streams, s.cancelStreams = context.WithCancel(context.Background())
	s.routes()
	// Order: request ID, panic recovery, access log, security headers, the
	// setup gate, then the routes (each with its own authentication).
	s.handler = s.wrapRequest(s.recoverPanics(s.accessLog(securityHeaders(auth.Gate(http.HandlerFunc(s.dispatch), cfg.Setup)))))
	return s, nil
}

// Handler returns the full middleware chain and routes.
func (s *Server) Handler() http.Handler { return s.handler }

// dispatch routes the request, answering unknown paths and wrong methods in
// the API's JSON error shape instead of the mux's plain text.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	h, pattern := s.mux.Handler(r)
	if pattern != "" {
		s.mux.ServeHTTP(w, r)
		return
	}
	if s.Web != nil && !apiPath(r.URL.Path) {
		s.Web.ServeHTTP(w, r)
		return
	}
	// No route matched. Let the mux say why, but keep its body.
	probe := &probeWriter{header: http.Header{}}
	h.ServeHTTP(probe, r)
	switch {
	case probe.status == http.StatusMethodNotAllowed:
		w.Header().Set("Allow", probe.header.Get("Allow"))
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this method is not allowed for this path")
	case probe.status >= 300 && probe.status < 400:
		w.Header().Set("Location", probe.header.Get("Location"))
		w.WriteHeader(probe.status)
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	}
}

// apiPath reports whether p belongs to the JSON API or an adapter, which keep
// the JSON error shape instead of the web UI's pages.
func apiPath(p string) bool {
	for _, prefix := range []string{"/api", "/sabnzbd"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// probeWriter captures the status and headers of the mux's own error
// handlers and drops their body.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header  { return p.header }
func (p *probeWriter) WriteHeader(code int) { p.status = code }
func (p *probeWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}

// HTTPServer returns the hardened http.Server for addr: header, read and
// idle timeouts, a 64 KiB header limit, and a write timeout that the event
// stream lifts for its own connection only. Shutting the server down ends
// every event stream.
func (s *Server) HTTPServer(addr string) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return s.streams },
	}
	srv.RegisterOnShutdown(s.cancelStreams)
	return srv
}

// Serve runs srv on ln until ctx ends, then shuts it down gracefully: it
// stops accepting connections and waits up to grace for requests in flight.
// A clean shutdown returns nil.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
