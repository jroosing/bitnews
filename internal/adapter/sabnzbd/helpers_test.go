package sabnzbd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/sabnzbd"
	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/view"
)

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const tinyNZB = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><head><meta type="name">Meta.Name</meta></head>
<file poster="a@b" date="1" subject="&quot;a.rar&quot; yEnc (1/1)"><groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="10240" number="1">a@b</segment></segments></file></nzb>`

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

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

// removed records the Downloads whose data the DataRemover was asked to delete.
type removed struct {
	mu  sync.Mutex
	ids []core.ID
}

func (*removed) Hold(context.Context, core.Download) (func(), error) { return func() {}, nil }

func (r *removed) RemoveData(_ context.Context, d core.Download) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, d.ID)
	return nil
}

func (r *removed) list() []core.ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.ID(nil), r.ids...)
}

// stack is the whole adapter on a temp-dir database: real store, real app
// services, a real app key and the handler under test.
type stack struct {
	t          *testing.T
	db         *store.DB
	clock      *clock
	log        *syncBuffer
	dbPath     string
	sources    *store.DownloadSourceRepo
	downloads  *store.DownloadRepo
	cats       *store.CategoryRepo
	settings   *app.SettingsService
	dls        *app.DownloadService
	adder      *app.AddService
	categories *app.CategoryService
	appKey     *auth.AppKeyService
	remover    *removed
	complete   string
	incomplete string
	key        string
	handler    http.Handler

	mu     sync.Mutex
	stages map[core.ID]core.Stage
	rates  map[core.ID]int64
	speed  int64
	paused bool

	n int // download counter for AddedAt
}

type stackOption func(*stackConfig)

type stackConfig struct {
	deps    func(*sabnzbd.Deps)
	addOpts []app.Option
}

func withDeps(f func(*sabnzbd.Deps)) stackOption { return func(c *stackConfig) { c.deps = f } }

func withAddOptions(o ...app.Option) stackOption { return func(c *stackConfig) { c.addOpts = o } }

func newStack(t *testing.T, opts ...stackOption) *stack {
	t.Helper()
	var sc stackConfig
	for _, o := range opts {
		o(&sc)
	}
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bitnews.db")
	db, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	s := &stack{
		t: t, db: db, dbPath: dbPath, clock: &clock{t: epoch.Add(48 * time.Hour)}, log: &syncBuffer{},
		remover: &removed{}, stages: map[core.ID]core.Stage{}, rates: map[core.ID]int64{},
		complete: filepath.Join(t.TempDir(), "complete"), incomplete: filepath.Join(t.TempDir(), "incomplete"),
	}
	logger := slog.New(slog.NewTextHandler(s.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := core.NewBus()
	s.downloads = store.NewDownloadRepo(db)
	s.cats = store.NewCategoryRepo(db)
	s.settings, err = app.NewSettingsService(ctx, store.NewSettingsRepo(db), bus)
	require.NoError(t, err)
	cur := s.settings.Current()
	cur.Paths.Usenet = core.Roots{Incomplete: s.incomplete, Complete: s.complete}
	require.NoError(t, s.settings.Update(ctx, cur))

	appOpts := []app.Option{app.WithClock(s.clock.Now), app.WithLogger(logger), app.WithDataRemover(s.remover)}
	s.dls = app.NewDownloadService(s.downloads, s.cats, bus, appOpts...)
	s.categories = app.NewCategoryService(s.cats, s.downloads, bus, appOpts...)
	s.sources = store.NewDownloadSourceRepo(db, store.GenerateKey())
	s.adder = app.NewAddService(s.dls, s.sources, s.settings,
		append(appOpts, sc.addOpts...)...)
	s.appKey, err = auth.NewAppKeyService(ctx, store.NewAppKeyRepo(db), auth.WithAppKeyLogger(logger))
	require.NoError(t, err)
	s.key = s.appKey.Current().Reveal()

	views, err := view.New(view.Config{Downloads: s.downloads, Live: stackLive{s}, Settings: s.settings.Current})
	require.NoError(t, err)

	deps := sabnzbd.Deps{
		AppKey: s.appKey, Downloads: s.dls, Adder: s.adder, Categories: s.categories, Settings: s.settings,
		View: views,
		SpeedSource: func() (int64, bool) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.speed, s.paused
		},
		Logger: logger, Clock: s.clock.Now,
	}
	if sc.deps != nil {
		sc.deps(&deps)
	}
	s.handler = sabnzbd.New(deps)
	return s
}

// stackLive is the view's live source in the tests: the stage and the
// download rate set per Download with setStage and setRate.
type stackLive struct{ s *stack }

func (l stackLive) Live(_ context.Context, d core.Download) view.Live {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	st, ok := l.s.stages[d.ID]
	return view.Live{Stage: st, HaveStage: ok, DownRate: l.s.rates[d.ID]}
}

func (s *stack) setStage(id core.ID, st core.Stage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stages[id] = st
}

// setRate sets one Download's own download rate.
func (s *stack) setRate(id core.ID, bps int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rates[id] = bps
}

// setSpeed sets the global rate and pause flag of the SpeedSource.
func (s *stack) setSpeed(bps int64, paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speed, s.paused = bps, paused
}

// seed stores a Download. By default it is a Queued, normal priority NZB
// Download added one minute after the previous one.
func (s *stack) seed(name string, mutate ...func(*core.Download)) core.Download {
	s.t.Helper()
	s.n++
	at := epoch.Add(time.Duration(s.n) * time.Minute)
	d := core.Download{
		ID: core.NewID(), Name: name, SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, QueuePosition: s.n, AddedAt: at, UpdatedAt: at,
	}
	for _, m := range mutate {
		m(&d)
	}
	require.NoError(s.t, s.downloads.Create(context.Background(), d))
	return d
}

func inState(st core.State) func(*core.Download) { return func(d *core.Download) { d.State = st } }

func ready(path string) func(*core.Download) {
	return func(d *core.Download) {
		d.State, d.OutputPath = core.StateReady, path
		d.FetchedAt = d.AddedAt.Add(time.Minute)
		d.ReadyAt = d.AddedAt.Add(3 * time.Minute)
	}
}

func failed(reason core.FailureReason, detail string) func(*core.Download) {
	return func(d *core.Download) {
		d.State, d.FailureReason, d.FailureDetail = core.StateFailed, reason, detail
		d.FetchedAt = d.AddedAt.Add(time.Minute)
		d.UpdatedAt = d.AddedAt.Add(2 * time.Minute)
	}
}

func sized(total, done int64) func(*core.Download) {
	return func(d *core.Download) { d.TotalBytes, d.DoneBytes = total, done }
}

func inCategory(id core.ID) func(*core.Download) { return func(d *core.Download) { d.CategoryID = id } }

func (s *stack) category(name string) core.Category {
	s.t.Helper()
	c, err := s.categories.Create(context.Background(), name, "")
	require.NoError(s.t, err)
	return c
}

// call sends a GET with the SAB key and json output appended.
func (s *stack) call(query string) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.callWith(s.key, query)
}

func (s *stack) callWith(key, query string) *httptest.ResponseRecorder {
	s.t.Helper()
	target := "/sabnzbd/api?output=json&" + query
	if key != "" {
		target += "&apikey=" + url.QueryEscape(key)
	}
	return s.serve(httptest.NewRequest("GET", target, nil))
}

func (s *stack) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

// upload sends an addfile call: query parameters, then the multipart body
// with the NZB as part "name" and the extra text fields.
func (s *stack) upload(query, filename, nzb string, fields ...[2]string) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.serve(uploadRequest(s.t, "/sabnzbd/api?output=json&"+query+"&apikey="+url.QueryEscape(s.key), "name", filename, nzb, fields...))
}

func uploadRequest(t *testing.T, target, partName, filename, nzb string, fields ...[2]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		require.NoError(t, mw.WriteField(f[0], f[1]))
	}
	if filename != "" {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+partName+`"; filename="`+filename+`"`)
		h.Set("Content-Type", "application/x-nzb")
		w, err := mw.CreatePart(h)
		require.NoError(t, err)
		_, err = io.WriteString(w, nzb)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	r := httptest.NewRequest("POST", target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// body decodes the JSON answer into v.
func body(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), v), rec.Body.String())
}

// generic decodes the JSON answer into a map.
func generic(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	body(t, rec, &m)
	return m
}

func nzo(d core.Download) string { return "SABnzbd_nzo_" + string(d.ID) }

func joinNzo(ds ...core.Download) string {
	ids := make([]string, len(ds))
	for i, d := range ds {
		ids[i] = nzo(d)
	}
	return strings.Join(ids, ",")
}

// uploadRequestFieldsLast is uploadRequest with the text fields after the file.
func uploadRequestFieldsLast(t *testing.T, target, filename, nzb string, fields ...[2]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, err := mw.CreateFormFile("name", filename)
	require.NoError(t, err)
	_, err = io.WriteString(w, nzb)
	require.NoError(t, err)
	for _, f := range fields {
		require.NoError(t, mw.WriteField(f[0], f[1]))
	}
	require.NoError(t, mw.Close())
	r := httptest.NewRequest("POST", target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// plainNZB has no <meta> name, so the Download is named after the upload.
const plainNZB = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<file poster="a@b" date="1" subject="&quot;a.rar&quot; yEnc (1/1)"><groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="10240" number="1">a@b</segment></segments></file></nzb>`

// all returns every stored Download, oldest first.
func (s *stack) all() []core.Download {
	s.t.Helper()
	ds, _, err := s.downloads.List(context.Background(), core.DownloadQuery{Limit: 200})
	require.NoError(s.t, err)
	return ds
}

// only returns the one stored Download.
func (s *stack) only() core.Download {
	s.t.Helper()
	ds := s.all()
	require.Len(s.t, ds, 1)
	return ds[0]
}

// ids reads {"status":true,"nzo_ids":[...]}.
func nzoIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var out struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
	}
	body(t, rec, &out)
	require.True(t, out.Status, rec.Body.String())
	return out.NzoIDs
}

// errorOf reads {"status":false,"error":"..."}.
func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Status bool   `json:"status"`
		Error  string `json:"error"`
	}
	body(t, rec, &out)
	require.False(t, out.Status, rec.Body.String())
	return out.Error
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func countOf(s, sub string) int { return strings.Count(s, sub) }

func stringsReplace(s, old, repl string) string { return strings.Replace(s, old, repl, 1) }
