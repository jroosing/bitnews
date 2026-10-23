package web_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
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
	"gitlab.com/kyura4k/gitnews/internal/view"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// fakeTorrents is a TorrentSource the tests fill. The other torrent facts
// come from the view module (env.live).
type fakeTorrents struct {
	mu    sync.Mutex
	files map[core.ID][]web.TorrentFile
	calls []web.ShareLimitChange
	err   error
}

func (f *fakeTorrents) Files(_ context.Context, id core.ID) ([]web.TorrentFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[id], nil
}

func (f *fakeTorrents) SetShareLimit(_ context.Context, _ core.ID, c web.ShareLimitChange) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, c)
	return nil
}

func (f *fakeTorrents) recorded() []web.ShareLimitChange {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]web.ShareLimitChange(nil), f.calls...)
}

// fakeDetail is a DetailSource the tests fill.
type fakeDetail struct {
	reports map[core.ID]web.FetchReport
}

func (f *fakeDetail) FetchReport(_ context.Context, id core.ID) (web.FetchReport, bool) {
	r, ok := f.reports[id]
	return r, ok
}

// denv is the stack for the history, detail and add tests: real store,
// services (adding included, with torrents on) and view module, fake daemon
// sources and a fake LiveSource (env.live).
type denv struct {
	*env
	repo     *store.DownloadRepo
	svc      *app.DownloadService
	catSvc   *app.CategoryService
	remover  *fakeRemover
	torrents *fakeTorrents
	detail   *fakeDetail
	b        *browser
	tok      string
	n        int
	root     string
}

// fakeRemover records the downloads whose data was deleted.
type fakeRemover struct {
	mu      sync.Mutex
	removed []core.ID
	err     error
}

func (f *fakeRemover) RemoveData(_ context.Context, d core.Download) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.removed = append(f.removed, d.ID)
	return nil
}

func (f *fakeRemover) ids() []core.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.ID(nil), f.removed...)
}

type dopt func(*dcfg)

type dcfg struct {
	noTorrents   bool
	noAdder      bool
	noTorrentSrc bool
	noDetail     bool
}

func withoutTorrentEngine() dopt { return func(c *dcfg) { c.noTorrents = true } }
func withoutAdder() dopt         { return func(c *dcfg) { c.noAdder = true } }
func withoutTorrentSource() dopt { return func(c *dcfg) { c.noTorrentSrc = true } }
func withoutDetailSource() dopt  { return func(c *dcfg) { c.noDetail = true } }

// newDetailEnv builds the stack and signs in.
func newDetailEnv(t *testing.T, opts ...dopt) *denv {
	t.Helper()
	var cfg dcfg
	for _, o := range opts {
		o(&cfg)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &denv{
		remover:  &fakeRemover{},
		torrents: &fakeTorrents{files: map[core.ID][]web.TorrentFile{}},
		detail:   &fakeDetail{reports: map[core.ID]web.FetchReport{}},
		root:     t.TempDir(),
	}
	e := &env{t: t, clock: &clock{t: epoch.Add(24 * time.Hour)}, log: &syncBuffer{}, live: newFakeLive()}
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := core.NewBus()
	users := store.NewUserRepo(db)
	d.repo = store.NewDownloadRepo(db)
	cats := store.NewCategoryRepo(db)
	key := store.GenerateKey()
	servers := store.NewServerRepo(db, key)
	require.NoError(t, users.Create(ctx, core.User{
		ID: core.NewID(), Username: "admin", PasswordHash: sharedAdminHash(t), CreatedAt: epoch,
	}))
	e.settings, err = app.NewSettingsService(ctx, store.NewSettingsRepo(db), bus)
	require.NoError(t, err)
	s := e.settings.Current()
	s.Paths.Usenet.Incomplete = filepath.Join(d.root, "incomplete")
	s.Paths.Usenet.Complete = filepath.Join(d.root, "complete")
	s.Paths.Torrent.Incomplete = filepath.Join(d.root, "torrents-incomplete")
	s.Paths.Torrent.Complete = filepath.Join(d.root, "torrents-complete")
	require.NoError(t, e.settings.Update(ctx, s))
	e.setup, err = auth.NewSetup(ctx, users, logger, auth.WithClock(e.clock.Now))
	require.NoError(t, err)
	e.sessions, err = auth.NewSessionService(store.NewSessionRepo(db), users,
		auth.WithSessionClock(e.clock.Now), auth.WithSessionLogger(logger))
	require.NoError(t, err)
	uiAuth, err := auth.NewUIAuth(e.sessions, e.settings.Current)
	require.NoError(t, err)

	appOpts := []app.Option{app.WithClock(e.clock.Now), app.WithDataRemover(d.remover)}
	d.svc = app.NewDownloadService(d.repo, cats, bus, appOpts...)
	sources := store.NewDownloadSourceRepo(db, key)
	appKey, err := auth.NewAppKeyService(ctx, store.NewAppKeyRepo(db), auth.WithAppKeyLogger(logger))
	require.NoError(t, err)
	d.catSvc = app.NewCategoryService(cats, d.repo, bus, appOpts...)
	addOpts := []app.Option{app.WithLogger(logger), app.WithClock(e.clock.Now)}
	if !cfg.noTorrents {
		addOpts = append(addOpts, app.WithTorrentSources(store.NewTorrentSourceRepo(db, key)))
	}
	adder := app.NewAddService(d.svc, sources, e.settings, addOpts...)
	deps := web.Deps{
		Logger: logger, Version: "1.2.3",
		Sessions: e.sessions, UIAuth: uiAuth, Setup: e.setup, Settings: e.settings,
		Downloads: d.svc, Categories: d.catSvc, Bus: bus,
		View:    newView(t, d.repo, e.live, e.settings),
		Servers: app.NewServerService(servers, bus, nil, appOpts...),
	}
	if !cfg.noAdder {
		deps.Adder = adder
	}
	if !cfg.noTorrentSrc {
		deps.Torrents = d.torrents
	}
	if !cfg.noDetail {
		deps.Detail = d.detail
	}
	webUI, err := web.New(deps)
	require.NoError(t, err)
	srv, err := api.New(api.Config{
		Logger: logger, Downloads: d.svc, Adder: adder, Categories: d.catSvc,
		Servers:  deps.Servers,
		Settings: e.settings, Setup: e.setup, Sessions: e.sessions, UIAuth: uiAuth,
		AppKey: appKey,
		Bus:    bus, Web: webUI,
	})
	require.NoError(t, err)
	e.handler = srv.Handler()
	d.env = e
	d.b = e.browser()
	d.b.login()
	d.tok = d.b.csrf()
	return d
}

// add stores a Download. Zero fields get defaults: kind nzb, state ready.
func (d *denv) add(dl core.Download) core.Download {
	d.t.Helper()
	d.n++
	if dl.ID == "" {
		dl.ID = core.NewID()
	}
	if dl.Name == "" {
		dl.Name = "Release " + strings.Repeat("x", d.n)
	}
	if dl.SourceKind == "" {
		dl.SourceKind = core.SourceNZB
	}
	if dl.State == "" {
		dl.State = core.StateReady
	}
	if dl.Priority == "" {
		dl.Priority = core.DownloadPriorityNormal
	}
	if dl.AddedAt.IsZero() {
		dl.AddedAt = epoch.Add(time.Duration(d.n) * time.Minute)
	}
	if dl.UpdatedAt.IsZero() {
		dl.UpdatedAt = dl.AddedAt.Add(30 * time.Second)
	}
	switch dl.State {
	case core.StateReady:
		if dl.ReadyAt.IsZero() {
			dl.ReadyAt = dl.UpdatedAt
		}
		if dl.FetchedAt.IsZero() {
			dl.FetchedAt = dl.AddedAt.Add(20 * time.Second)
		}
		if dl.OutputPath == "" {
			dl.OutputPath = "/complete/" + string(dl.ID)
		}
	case core.StateFailed:
		if dl.FailureReason == "" {
			dl.FailureReason = core.FailureRepairInsufficient
		}
	}
	dl.QueuePosition = d.n
	require.NoError(d.t, d.repo.Create(context.Background(), dl))
	return dl
}

func (d *denv) get(id core.ID) (core.Download, error) {
	return d.repo.Get(context.Background(), id)
}

// act posts like htmx does: the CSRF header, HX-Request and the form values.
func (d *denv) act(path string, form url.Values) *httptest.ResponseRecorder {
	d.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	return d.b.post(path, form, htmx(), header("X-CSRF-Token", d.tok))
}

// formPart is one part of a multipart upload.
type formPart struct{ name, filename, value string }

func multipartBody(t *testing.T, parts ...formPart) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.filename != "" {
			fw, err := w.CreateFormFile(p.name, p.filename)
			require.NoError(t, err)
			_, err = io.WriteString(fw, p.value)
			require.NoError(t, err)
			continue
		}
		require.NoError(t, w.WriteField(p.name, p.value))
	}
	require.NoError(t, w.Close())
	return buf.String(), w.FormDataContentType()
}

// upload posts the parts like the dialog does (htmx, multipart, CSRF header).
func (d *denv) upload(parts ...formPart) *httptest.ResponseRecorder {
	d.t.Helper()
	body, ct := multipartBody(d.t, parts...)
	return d.postRaw("/downloads/add", body, ct)
}

func (d *denv) postRaw(path, body, contentType string, opts ...reqOpt) *httptest.ResponseRecorder {
	d.t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", "http://example.com")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-CSRF-Token", d.tok)
	for _, o := range opts {
		o(r)
	}
	return d.b.send(r)
}

// nzbXML builds a small NZB.
func nzbXML(name string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<head><meta type="name">` + name + `</meta></head>
<file poster="p@example.com" date="1700000000" subject="[1/1] - &quot;show.mkv&quot; yEnc (1/2)">
<groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="100" number="1">one@example.com</segment><segment bytes="50" number="2">two@example.com</segment></segments>
</file>
</nzb>`
}

// rows returns the history rows of a page body, keyed by Download ID.
func historyRowNames(t *testing.T, body string) []string {
	t.Helper()
	doc := parseHTML(t, body)
	var names []string
	for _, tr := range findAll(doc, func(n *html.Node) bool { return n.Data == "tr" && strings.HasPrefix(attr(n, "id"), "download-") }) {
		a := find(tr, func(n *html.Node) bool { return n.Data == "a" && strings.HasPrefix(attr(n, "href"), "/downloads/") })
		require.NotNil(t, a)
		names = append(names, text(a))
	}
	return names
}

// downloadHTMLResponses renders the history, detail and add-dialog pages and
// fragments for the HTML invariants (CSP, accessibility) that run over
// htmlResponses.
func downloadHTMLResponses(t *testing.T) map[string]*httptest.ResponseRecorder {
	d := newDetailEnv(t)
	out := map[string]*httptest.ResponseRecorder{}
	cat, err := d.catSvc.Create(t.Context(), "tv", "")
	require.NoError(t, err)
	ready := d.add(core.Download{Name: `<b>Ready</b> "quoted" & name`, CategoryID: cat.ID, TotalBytes: 5 << 30, DoneBytes: 5 << 30})
	failed := d.add(core.Download{Name: "Failed.One", State: core.StateFailed, FailureReason: core.FailureRepairInsufficient, FailureDetail: "needs 4 blocks"})
	active := d.add(core.Download{Name: "Active.One", State: core.StateFetching, TotalBytes: 1000, DoneBytes: 400})
	tor := d.add(core.Download{Name: "Torrent.One", SourceKind: core.SourceTorrent, TotalBytes: 100})
	d.detail.reports[ready.ID] = web.FetchReport{MissingArticles: 1, DamagedArticles: 2, MissingFiles: 1}
	d.live.set(tor.ID, view.Live{Torrent: &view.TorrentLive{Hash: "0123456789abcdef0123456789abcdef01234567", Registered: true, Peers: 3}})
	d.torrents.files[tor.ID] = []web.TorrentFile{{Path: "a/b.mkv", Size: 100}}
	out["GET /history (rows)"] = d.b.get("/history")
	out["GET /history (empty filter)"] = d.b.get("/history?kind=torrent&state=failed")
	out["GET /history (htmx)"] = d.b.get("/history?state=failed", htmx())
	for name, id := range map[string]core.ID{"ready": ready.ID, "failed": failed.ID, "active": active.ID, "torrent": tor.ID} {
		out["GET /downloads/{id} ("+name+")"] = d.b.get("/downloads/" + string(id))
		out["GET /downloads/{id}/live ("+name+")"] = d.b.get("/downloads/"+string(id)+"/live", htmx())
	}
	out["GET /downloads/{id} (404)"] = d.b.get("/downloads/" + string(core.NewID()))
	out["GET /downloads/add"] = d.b.get("/downloads/add", htmx())
	out["POST /downloads/add (invalid)"] = d.upload(formPart{name: "file", filename: "a.nzb", value: "<nzb>"})
	out["POST /downloads/add (nothing)"] = d.upload(formPart{name: "name", value: "x"})
	out["POST /downloads/{id}/share-limit (invalid)"] = d.act("/downloads/"+string(tor.ID)+"/share-limit", url.Values{"mode": {"custom"}, "ratio": {"x"}})
	out["POST /downloads/{id}/share-limit"] = d.act("/downloads/"+string(tor.ID)+"/share-limit", url.Values{"mode": {"default"}})
	return out
}
