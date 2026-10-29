package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/api"
	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
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

// syncBuffer is a bytes.Buffer safe for concurrent log writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
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
// services, a fake clock and a captured log.
type env struct {
	t         *testing.T
	db        *store.DB
	bus       *core.Bus
	clock     *clock
	log       *syncBuffer
	users     *store.UserRepo
	sessions  *auth.SessionService
	downloads *store.DownloadRepo
	cats      *store.CategoryRepo
	servers   *store.ServerRepo
	sources   *store.DownloadSourceRepo
	torrents  *store.TorrentSourceRepo
	key       store.Key
	prober    *fakeProber
	settings  *app.SettingsService
	setup     *auth.Setup
	appKey    *auth.AppKeyService
	server    *api.Server
	cfg       api.Config
}

type envOption func(*envConfig)

type envConfig struct {
	noAdmin bool
	mutate  func(*api.Config)
	// mutateEnv is like mutate, with the env (for its repositories).
	mutateEnv func(*env, *api.Config)
	remover   app.DataRemover
	addOpts   []app.Option
}

func withoutAdmin() envOption { return func(c *envConfig) { c.noAdmin = true } }

func withConfig(f func(*api.Config)) envOption { return func(c *envConfig) { c.mutate = f } }

// withConfigEnv is withConfig for a mutation that needs the env.
func withConfigEnv(f func(*env, *api.Config)) envOption {
	return func(c *envConfig) { c.mutateEnv = f }
}

// withDataRemover installs the hook DELETE ?delete_data=true calls.
func withDataRemover(r app.DataRemover) envOption { return func(c *envConfig) { c.remover = r } }

// withAddOptions configures the AddService (fetch client, timeouts, limits).
func withAddOptions(o ...app.Option) envOption { return func(c *envConfig) { c.addOpts = o } }

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

	e := &env{t: t, db: db, bus: core.NewBus(), clock: &clock{t: epoch.Add(24 * time.Hour)}, log: &syncBuffer{}}
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.users = store.NewUserRepo(db)
	e.downloads = store.NewDownloadRepo(db)
	e.cats = store.NewCategoryRepo(db)
	e.key = store.GenerateKey()
	e.servers = store.NewServerRepo(db, e.key)
	e.prober = &fakeProber{result: app.ProbeResult{OK: true, Class: app.ProbeOK}}
	if !ec.noAdmin {
		require.NoError(t, e.users.Create(ctx, core.User{
			ID: core.NewID(), Username: "admin", PasswordHash: sharedAdminHash(t), CreatedAt: epoch,
		}))
	}

	e.settings, err = app.NewSettingsService(ctx, store.NewSettingsRepo(db), e.bus)
	require.NoError(t, err)
	e.setup, err = auth.NewSetup(ctx, e.users, logger, auth.WithClock(e.clock.Now))
	require.NoError(t, err)
	e.sessions, err = auth.NewSessionService(store.NewSessionRepo(db), e.users,
		auth.WithSessionClock(e.clock.Now), auth.WithSessionLogger(logger))
	require.NoError(t, err)
	e.appKey, err = auth.NewAppKeyService(ctx, store.NewAppKeyRepo(db), auth.WithAppKeyLogger(logger))
	require.NoError(t, err)

	appOpts := []app.Option{app.WithClock(e.clock.Now)}
	if ec.remover != nil {
		appOpts = append(appOpts, app.WithDataRemover(ec.remover))
	}
	downloadSvc := app.NewDownloadService(e.downloads, e.cats, e.bus, appOpts...)
	e.sources = store.NewDownloadSourceRepo(db, e.key)
	e.torrents = store.NewTorrentSourceRepo(db, e.key)
	addOpts := append([]app.Option{app.WithLogger(logger), app.WithTorrentSources(e.torrents)}, appOpts...)
	e.cfg = api.Config{
		Logger:     logger,
		Version:    api.Version{Version: "1.2.3", Commit: "abc1234", GoVersion: "go9.99"},
		Downloads:  downloadSvc,
		Adder:      app.NewAddService(downloadSvc, e.sources, e.settings, append(addOpts, ec.addOpts...)...),
		Categories: app.NewCategoryService(e.cats, e.downloads, e.bus, appOpts...),
		Servers:    app.NewServerService(e.servers, e.bus, e.prober, appOpts...),
		Settings:   e.settings,
		Setup:      e.setup,
		AppKey:     e.appKey,
		Bus:        e.bus,
		InstanceID: "instance-1",
	}
	if ec.mutate != nil {
		ec.mutate(&e.cfg)
	}
	if ec.mutateEnv != nil {
		ec.mutateEnv(e, &e.cfg)
	}
	e.server, err = api.New(e.cfg)
	require.NoError(t, err)
	return e
}

// do runs the request through the full middleware stack.
func (e *env) do(method, path, body string, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, m := range mods {
		m(r)
	}
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, r)
	return rec
}

func withHeader(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

func withBearer(key string) func(*http.Request) { return withHeader("Authorization", "Bearer "+key) }

// session is a logged-in browser: its cookie and CSRF token.
type session struct {
	cookie *http.Cookie
}

// mods returns the request modifiers that send this session's cookie, as
// the browser does on a same-origin request.
func (s session) mods() []func(*http.Request) {
	return []func(*http.Request){withCookie(s.cookie), withHeader("Sec-Fetch-Site", "same-origin")}
}

// login opens a UI session for admin, as the web UI's sign-in does. The API
// accepts nothing but the app key, so its cookie and CSRF token must open
// nothing there.
func (e *env) login() session {
	e.t.Helper()
	token, err := e.sessions.Login(context.Background(), "admin", adminPassword, netip.MustParseAddr("127.0.0.1"))
	require.NoError(e.t, err)
	return session{cookie: &http.Cookie{Name: auth.SessionCookieName, Value: token}}
}

// apiKey returns the app key's plaintext.
func (e *env) apiKey() string { return e.appKey.Current().Reveal() }

// addDownload stores a Queued NZB Download added at epoch+offset.
func (e *env) addDownload(name string, offset time.Duration) core.Download {
	e.t.Helper()
	at := epoch.Add(offset)
	d := core.Download{
		ID: core.NewID(), Name: name, SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at,
	}
	require.NoError(e.t, e.downloads.Create(context.Background(), d))
	return d
}

// openAuth turns on disable auth and LAN bypass, as the UI's security page
// can: the adapters and the API must stay closed anyway.
func (e *env) openAuth() {
	e.t.Helper()
	s := e.settings.Current()
	s.Auth.DisableAuth = true
	s.Auth.LANBypassCIDRs = []string{"0.0.0.0/0", "::/0"}
	s.Auth.AllowedHosts = []string{"example.com"}
	require.NoError(e.t, e.settings.Update(context.Background(), s))
}
