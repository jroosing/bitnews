package qbit_test

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // an info hash, not a security hash
	"encoding/hex"
	"encoding/json"
	"fmt"
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

	"gitlab.com/kyura4k/gitnews/internal/adapter/qbit"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

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

// removed records the Downloads whose data the DataRemover was asked to
// delete.
type removed struct {
	mu  sync.Mutex
	ids []core.ID
}

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

// engine is the in-memory fake of the three ports the daemon implements.
type engine struct {
	t         testing.TB
	downloads core.DownloadRepo

	mu     sync.Mutex
	views  map[string]qbit.TorrentView // by hash
	files  map[string][]qbit.TorrentFile
	added  []addCall
	limits map[core.ID]limitCall
	// failAdd, when set, is returned by the next adds.
	failAdd error
	n       int
}

type addCall struct {
	File bool
	Data []byte
	URL  string
	Opts qbit.AddOptions
	Hash string
}

type limitCall struct {
	Ratio     float64
	Seconds   int64
	UseGlobal bool
}

func newEngine(t testing.TB, downloads core.DownloadRepo) *engine {
	return &engine{
		t: t, downloads: downloads, views: map[string]qbit.TorrentView{},
		files: map[string][]qbit.TorrentFile{}, limits: map[core.ID]limitCall{},
	}
}

func (e *engine) List(context.Context) ([]qbit.TorrentView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]qbit.TorrentView, 0, len(e.views))
	for _, v := range e.views {
		out = append(out, v)
	}
	return out, nil
}

func (e *engine) Get(_ context.Context, hash string) (qbit.TorrentView, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.views[hash]
	return v, ok, nil
}

func (e *engine) Files(_ context.Context, hash string) ([]qbit.TorrentFile, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.views[hash]; !ok {
		return nil, false, nil
	}
	return e.files[hash], true, nil
}

func (e *engine) SetShareLimit(_ context.Context, id core.ID, ratio float64, seconds int64, useGlobal bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.limits[id] = limitCall{Ratio: ratio, Seconds: seconds, UseGlobal: useGlobal}
	return nil
}

func (e *engine) limit(id core.ID) (limitCall, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.limits[id]
	return l, ok
}

func (e *engine) addCalls() []addCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]addCall(nil), e.added...)
}

func (e *engine) AddTorrentFile(ctx context.Context, data []byte, opts qbit.AddOptions) (string, error) {
	return e.add(ctx, addCall{File: true, Data: data, Opts: opts}, string(data))
}

func (e *engine) AddURL(ctx context.Context, u string, opts qbit.AddOptions) (string, error) {
	return e.add(ctx, addCall{URL: u, Opts: opts}, u)
}

// add mimics the daemon: it creates the Download and the torrent.
func (e *engine) add(ctx context.Context, c addCall, key string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failAdd != nil {
		return "", e.failAdd
	}
	sum := sha1.Sum([]byte(key)) //nolint:gosec
	hash := hex.EncodeToString(sum[:])
	if _, dup := e.views[hash]; dup {
		return hash, fmt.Errorf("torrent exists: %w", core.ErrConflict)
	}
	e.n++
	at := epoch.Add(time.Duration(e.n) * time.Minute)
	state := core.StateQueued
	if strings.HasPrefix(c.URL, "magnet:") {
		state = core.StateFetchingMetadata
	}
	d := core.Download{
		ID: core.NewID(), Name: "added " + hash[:6], SourceKind: core.SourceTorrent, State: state,
		CategoryID: c.Opts.CategoryID, Paused: c.Opts.Paused, Priority: core.DownloadPriorityNormal,
		QueuePosition: 1000 + e.n, AddedAt: at, UpdatedAt: at,
	}
	if err := e.downloads.Create(ctx, d); err != nil {
		return "", err
	}
	v := qbit.TorrentView{DownloadID: d.ID, InfoHash: hash, Name: d.Name, AddedAt: at, Paused: c.Opts.Paused}
	if c.Opts.ShareLimit != nil {
		v.ShareLimit = *c.Opts.ShareLimit
	} else {
		v.ShareLimit = qbit.ShareLimit{Global: true}
	}
	e.views[hash] = v
	c.Hash = hash
	e.added = append(e.added, c)
	return hash, nil
}

// put registers a torrent view for tests.
func (e *engine) put(v qbit.TorrentView) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.views[v.InfoHash] = v
}

// stack is the whole adapter on a temp-dir database: real store, real app
// services, a real app key and the handler under test.
type stack struct {
	t          testing.TB
	db         *store.DB
	clock      *clock
	log        *syncBuffer
	downloads  *store.DownloadRepo
	cats       *store.CategoryRepo
	settings   *app.SettingsService
	dls        *app.DownloadService
	categories *app.CategoryService
	appKey     *auth.AppKeyService
	limiter    *auth.LoginLimiter
	remover    *removed
	engine     *engine
	torrents   string // torrent complete root
	incomplete string
	user, pass string
	handler    http.Handler

	n int // download counter for AddedAt
}

type stackOption func(*stackConfig)

type stackConfig struct {
	deps func(*qbit.Deps)
}

func withDeps(f func(*qbit.Deps)) stackOption { return func(c *stackConfig) { c.deps = f } }

func newStack(t testing.TB, opts ...stackOption) *stack {
	t.Helper()
	var sc stackConfig
	for _, o := range opts {
		o(&sc)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	s := &stack{
		t: t, db: db, clock: &clock{t: epoch.Add(48 * time.Hour)}, log: &syncBuffer{}, remover: &removed{},
		torrents: filepath.Join(t.TempDir(), "torrents"), incomplete: filepath.Join(t.TempDir(), "incomplete"),
	}
	logger := slog.New(slog.NewTextHandler(s.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := core.NewBus()
	s.downloads = store.NewDownloadRepo(db)
	s.cats = store.NewCategoryRepo(db)
	s.settings, err = app.NewSettingsService(ctx, store.NewSettingsRepo(db), bus)
	require.NoError(t, err)
	cur := s.settings.Current()
	cur.Paths.Torrent = core.Roots{Incomplete: s.incomplete, Complete: s.torrents}
	require.NoError(t, s.settings.Update(ctx, cur))

	appOpts := []app.Option{app.WithClock(s.clock.Now), app.WithLogger(logger), app.WithDataRemover(s.remover)}
	s.dls = app.NewDownloadService(s.downloads, s.cats, bus, appOpts...)
	s.categories = app.NewCategoryService(s.cats, s.downloads, bus, appOpts...)
	s.appKey, err = auth.NewAppKeyService(ctx, store.NewAppKeyRepo(db), auth.WithAppKeyLogger(logger))
	require.NoError(t, err)
	s.user, s.pass = auth.QBitUsername, s.appKey.Current().Reveal()
	s.engine = newEngine(t, s.downloads)
	s.limiter = auth.NewLoginLimiter(auth.WithLimiterClock(s.clock.Now))

	deps := qbit.Deps{
		AppKey: s.appKey, Downloads: s.dls, Categories: s.categories, Settings: s.settings,
		Torrents: s.engine, Adder: s.engine, ShareLimits: s.engine, Files: s.engine,
		Limiter: s.limiter, Logger: logger, Clock: s.clock.Now,
	}
	if sc.deps != nil {
		sc.deps(&deps)
	}
	s.handler = qbit.New(deps)
	return s
}

// seed stores a Queued torrent Download and its view. mutate may change
// either.
func (s *stack) seed(name string, mutate ...func(*core.Download, *qbit.TorrentView)) (core.Download, qbit.TorrentView) {
	s.t.Helper()
	s.n++
	at := epoch.Add(time.Duration(s.n) * time.Minute)
	d := core.Download{
		ID: core.NewID(), Name: name, SourceKind: core.SourceTorrent, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, QueuePosition: s.n - 1, AddedAt: at, UpdatedAt: at,
	}
	sum := sha1.Sum([]byte(name)) //nolint:gosec
	v := qbit.TorrentView{
		InfoHash: hex.EncodeToString(sum[:]), Name: name, AddedAt: at,
		ShareLimit: qbit.ShareLimit{Global: true},
	}
	for _, m := range mutate {
		m(&d, &v)
	}
	v.DownloadID = d.ID
	require.NoError(s.t, s.downloads.Create(context.Background(), d))
	s.engine.put(v)
	return d, v
}

func (s *stack) category(name string) core.Category {
	s.t.Helper()
	c, err := s.categories.Create(context.Background(), name, "")
	require.NoError(s.t, err)
	return c
}

// download reads a stored Download.
func (s *stack) download(id core.ID) core.Download {
	s.t.Helper()
	d, err := s.dls.Get(context.Background(), id)
	require.NoError(s.t, err)
	return d
}

func (s *stack) exists(id core.ID) bool {
	_, err := s.dls.Get(context.Background(), id)
	return err == nil
}

// serve runs the request.
func (s *stack) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

// login logs in with the stack's credential and returns the SID cookie.
func (s *stack) login() *http.Cookie {
	s.t.Helper()
	rec := s.serve(formRequest("POST", "/api/v2/auth/login", url.Values{"username": {s.user}, "password": {s.pass}}))
	require.Equal(s.t, http.StatusOK, rec.Code)
	require.Equal(s.t, "Ok.", rec.Body.String())
	return sidOf(s.t, rec)
}

func sidOf(t testing.TB, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "SID" {
			return c
		}
	}
	t.Fatal("no SID cookie")
	return nil
}

// get sends an authenticated GET.
func (s *stack) get(sid *http.Cookie, target string) *httptest.ResponseRecorder {
	s.t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	if sid != nil {
		r.AddCookie(sid)
	}
	return s.serve(r)
}

// post sends an authenticated form POST.
func (s *stack) post(sid *http.Cookie, target string, form url.Values) *httptest.ResponseRecorder {
	s.t.Helper()
	r := formRequest("POST", target, form)
	if sid != nil {
		r.AddCookie(sid)
	}
	return s.serve(r)
}

func formRequest(method, target string, form url.Values) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// part is one multipart part.
type part struct {
	name, filename, content string
}

func multipartRequest(t testing.TB, target string, parts ...part) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.filename == "" {
			require.NoError(t, mw.WriteField(p.name, p.content))
			continue
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.name, p.filename))
		h.Set("Content-Type", "application/x-bittorrent")
		w, err := mw.CreatePart(h)
		require.NoError(t, err)
		_, err = io.WriteString(w, p.content)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	r := httptest.NewRequest("POST", target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// decode reads the JSON answer into v.
func decode(t testing.TB, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), v), rec.Body.String())
}

// torrentJSON is the part of a torrents/info entry the tests read, with the
// JSON types Sonarr and Radarr use.
type torrentJSON struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	DLSpeed      int64   `json:"dlspeed"`
	UPSpeed      int64   `json:"upspeed"`
	ETA          int64   `json:"eta"`
	State        string  `json:"state"`
	Category     string  `json:"category"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	Ratio        float64 `json:"ratio"`
	RatioLimit   float64 `json:"ratio_limit"`
	SeedingTime  int64   `json:"seeding_time"`
	SeedingLimit int64   `json:"seeding_time_limit"`
	AddedOn      int64   `json:"added_on"`
	CompletionOn int64   `json:"completion_on"`
	AmountLeft   int64   `json:"amount_left"`
	Completed    int64   `json:"completed"`
	Downloaded   int64   `json:"downloaded"`
	Uploaded     int64   `json:"uploaded"`
	Priority     int     `json:"priority"`
	NumSeeds     int     `json:"num_seeds"`
	NumLeechs    int     `json:"num_leechs"`
	ForceStart   bool    `json:"force_start"`
}

// info lists torrents/info with the query.
func (s *stack) info(sid *http.Cookie, query string) []torrentJSON {
	s.t.Helper()
	target := "/api/v2/torrents/info"
	if query != "" {
		target += "?" + query
	}
	var out []torrentJSON
	decode(s.t, s.get(sid, target), &out)
	return out
}

func names(ts []torrentJSON) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func inState(st core.State) func(*core.Download, *qbit.TorrentView) {
	return func(d *core.Download, _ *qbit.TorrentView) { d.State = st }
}

func ready(path string) func(*core.Download, *qbit.TorrentView) {
	return func(d *core.Download, v *qbit.TorrentView) {
		d.State, d.OutputPath = core.StateReady, path
		d.FetchedAt = d.AddedAt.Add(time.Minute)
		d.ReadyAt = d.AddedAt.Add(3 * time.Minute)
		v.ContentPath = path
	}
}

func sized(total, done int64) func(*core.Download, *qbit.TorrentView) {
	return func(d *core.Download, v *qbit.TorrentView) {
		d.TotalBytes, d.DoneBytes = total, done
		v.Size, v.DoneBytes = total, done
	}
}

// seedBulk stores the i-th of many torrent Downloads, cheaply.
func (s *stack) seedBulk(i int) {
	at := epoch.Add(time.Duration(i) * time.Second)
	d := core.Download{
		ID: core.NewID(), Name: fmt.Sprintf("Bulk.Release.%05d", i), SourceKind: core.SourceTorrent,
		State: core.StateFetching, Priority: core.DownloadPriorityNormal, QueuePosition: i + 1,
		TotalBytes: 1 << 30, DoneBytes: int64(i) << 10, AddedAt: at, UpdatedAt: at,
	}
	if i%2 == 0 {
		d.State, d.OutputPath, d.FetchedAt, d.ReadyAt = core.StateReady, fmt.Sprintf("/data/torrents/bulk/%05d", i), at, at
	}
	require.NoError(s.t, s.downloads.Create(context.Background(), d))
	sum := sha1.Sum([]byte(d.Name)) //nolint:gosec
	s.engine.put(qbit.TorrentView{
		DownloadID: d.ID, InfoHash: hex.EncodeToString(sum[:]), Name: d.Name, Size: d.TotalBytes,
		DoneBytes: d.DoneBytes, DownRate: 1 << 20, AddedAt: at, ShareLimit: qbit.ShareLimit{Global: true},
	})
}
