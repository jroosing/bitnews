package web_test

import (
	"context"
	"crypto/tls"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

const adminPassword = "correct horse battery staple"

var (
	adminHashOnce sync.Once
	adminHash     []byte
)

// sharedAdminHash hashes the admin password once for all tests: argon2id at
// production cost is slow.
func sharedAdminHash(t *testing.T) []byte {
	t.Helper()
	adminHashOnce.Do(func() {
		var err error
		adminHash, err = auth.HashPassword(core.Secret(adminPassword))
		require.NoError(t, err)
	})
	return adminHash
}

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is the whole stack on a temp-dir database: real store, real auth, real
// services, the real API server around the web UI (so the setup gate, the
// request chain and the CSP override are all in play).
type env struct {
	t        *testing.T
	clock    *clock
	log      *syncBuffer
	settings *app.SettingsService
	setup    *auth.Setup
	sessions *auth.SessionService
	handler  http.Handler
	token    string // the setup token, when setup is open
}

type envOption func(*envConfig)

type envConfig struct{ noAdmin bool }

func withoutAdmin() envOption { return func(c *envConfig) { c.noAdmin = true } }

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	var ec envConfig
	for _, o := range opts {
		o(&ec)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	e := &env{t: t, clock: &clock{t: epoch.Add(24 * time.Hour)}, log: &syncBuffer{}}
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := core.NewBus()
	users := store.NewUserRepo(db)
	downloads := store.NewDownloadRepo(db)
	cats := store.NewCategoryRepo(db)
	key := store.GenerateKey()
	servers := store.NewServerRepo(db, key)
	if !ec.noAdmin {
		require.NoError(t, users.Create(ctx, core.User{
			ID: core.NewID(), Username: "admin", PasswordHash: sharedAdminHash(t), CreatedAt: epoch,
		}))
	}
	e.settings, err = app.NewSettingsService(ctx, store.NewSettingsRepo(db), bus)
	require.NoError(t, err)
	e.setup, err = auth.NewSetup(ctx, users, logger, auth.WithClock(e.clock.Now))
	require.NoError(t, err)
	e.sessions, err = auth.NewSessionService(store.NewSessionRepo(db), users,
		auth.WithSessionClock(e.clock.Now), auth.WithSessionLogger(logger))
	require.NoError(t, err)
	uiAuth, err := auth.NewUIAuth(e.sessions, e.settings.Current)
	require.NoError(t, err)

	appOpts := []app.Option{app.WithClock(e.clock.Now)}
	downloadSvc := app.NewDownloadService(downloads, cats, bus, appOpts...)
	sources := store.NewDownloadSourceRepo(db, key)
	catSvc := app.NewCategoryService(cats, downloads, bus, appOpts...)
	serverSvc := app.NewServerService(servers, bus, nil, appOpts...)
	webUI, err := web.New(web.Deps{
		Logger: logger, Version: "1.2.3",
		Sessions: e.sessions, UIAuth: uiAuth, Setup: e.setup, Settings: e.settings,
		Downloads: downloadSvc, Categories: catSvc, Servers: serverSvc, Bus: bus,
	})
	require.NoError(t, err)
	srv, err := api.New(api.Config{
		Logger:     logger,
		Downloads:  downloadSvc,
		Adder:      app.NewAddService(downloadSvc, sources, e.settings, append(appOpts, app.WithLogger(logger))...),
		Categories: catSvc,
		Servers:    serverSvc,
		Settings:   e.settings,
		Setup:      e.setup,
		Sessions:   e.sessions,
		UIAuth:     uiAuth,
		APIKeys:    auth.NewAPIKeyService(store.NewAPIKeyRepo(db), auth.WithAPIKeyClock(e.clock.Now), auth.WithAPIKeyLogger(logger)),
		Bus:        bus,
		Web:        webUI,
	})
	require.NoError(t, err)
	e.handler = srv.Handler()
	if e.setup.Required() {
		e.token = setupTokenFromLog(t, e.log.String())
	}
	return e
}

// setupTokenFromLog finds the one-time token in the startup log line.
func setupTokenFromLog(t *testing.T, log string) string {
	t.Helper()
	_, rest, ok := strings.Cut(log, "setup_token=")
	require.True(t, ok, "no setup token in log: %s", log)
	tok, _, _ := strings.Cut(rest, "\n")
	return strings.TrimSpace(tok)
}

// browser is a minimal cookie-keeping client that does not follow redirects.
type browser struct {
	e       *env
	cookies map[string]*http.Cookie
}

func (e *env) browser() *browser { return &browser{e: e, cookies: map[string]*http.Cookie{}} }

// reqOpt modifies a request before it is sent.
type reqOpt func(*http.Request)

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// htmx marks the request as made by htmx.
func htmx() reqOpt { return header("HX-Request", "true") }

func (b *browser) send(r *http.Request) *httptest.ResponseRecorder {
	b.e.t.Helper()
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	b.e.handler.ServeHTTP(rec, r)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
			continue
		}
		b.cookies[c.Name] = &http.Cookie{Name: c.Name, Value: c.Value}
	}
	return rec
}

func (b *browser) get(target string, opts ...reqOpt) *httptest.ResponseRecorder {
	b.e.t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for _, o := range opts {
		o(r)
	}
	return b.send(r)
}

// post sends a form like a browser does, with the same-origin Origin header.
func (b *browser) post(target string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	b.e.t.Helper()
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://example.com")
	for _, o := range opts {
		o(r)
	}
	return b.send(r)
}

// login signs in as admin through the login form.
func (b *browser) login() {
	b.e.t.Helper()
	rec := b.post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}})
	require.Equal(b.e.t, http.StatusSeeOther, rec.Code, rec.Body.String())
	require.NotNil(b.e.t, b.cookies[auth.SessionCookieName])
}

// csrf reads the CSRF token from the meta tag of a signed-in page.
func (b *browser) csrf() string {
	b.e.t.Helper()
	rec := b.get("/queue")
	require.Equal(b.e.t, http.StatusOK, rec.Code)
	doc := parseHTML(b.e.t, rec.Body.String())
	meta := find(doc, func(n *html.Node) bool { return n.Data == "meta" && attr(n, "name") == "csrf-token" })
	require.NotNil(b.e.t, meta, "no csrf-token meta tag")
	return attr(meta, "content")
}

func location(rec *httptest.ResponseRecorder) string { return rec.Header().Get("Location") }

// ---- HTML helpers -----------------------------------------------------

func parseHTML(t *testing.T, s string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(s))
	require.NoError(t, err)
	return doc
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// walk calls f for every node below n, depth first.
func walk(n *html.Node, f func(*html.Node)) {
	f(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, f)
	}
}

func find(n *html.Node, match func(*html.Node) bool) *html.Node {
	var found *html.Node
	walk(n, func(x *html.Node) {
		if found == nil && x.Type == html.ElementNode && match(x) {
			found = x
		}
	})
	return found
}

func findAll(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	walk(n, func(x *html.Node) {
		if x.Type == html.ElementNode && match(x) {
			out = append(out, x)
		}
	})
	return out
}

func byID(n *html.Node, id string) *html.Node {
	return find(n, func(x *html.Node) bool { return attr(x, "id") == id })
}

func text(n *html.Node) string {
	var b strings.Builder
	walk(n, func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

var tlsState = tls.ConnectionState{}

type (
	htmlAttr = template.HTMLAttr
	htmlHTML = template.HTML
)
