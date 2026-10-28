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

	"github.com/jroosing/bitnews/internal/adapter/qbit"
	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/view"
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

func (r *removed) Hold(context.Context, core.Download) (func(), error) { return func() {}, nil }

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

// torrent is what the fake engine knows of one torrent: the live facts the
// view module reads through the engine's view.LiveSource.
type torrent struct {
	DownloadID core.ID
	InfoHash   string
	Name       string
	// Size and DoneBytes are the engine's byte counts; zero Size is unknown.
	Size, DoneBytes  int64
	DownRate, UpRate int64
	Uploaded         int64
	SeedingTime      time.Duration
	Peers, Seeds     int
	Private          bool
	// Paused is a Share Limit holding the torrent.
	Paused bool
	// OwnRatio and OwnSeeding are the Download's own Share Limit halves; nil
	// leaves the global default.
	OwnRatio   *float64
	OwnSeeding *time.Duration
	// SavePath and ContentPath are the engine's paths: the Download's
	// directory is ContentPath, else SavePath/Name.
	SavePath, ContentPath string
}

// engine is the in-memory fake of the torrent side: the live source of the
// view module and the Share Limit setter of the app services. A torrent added
// through the AddService is known by its stored source until a test puts
// more facts.
type engine struct {
	t       testing.TB
	sources core.TorrentSourceRepo

	mu       sync.Mutex
	torrents map[string]torrent // by hash
	byID     map[core.ID]string // hash by Download
	files    map[string][]core.TorrentFile
	limits   map[core.ID]limitCall
}

var (
	_ view.LiveSource      = (*engine)(nil)
	_ app.ShareLimitSetter = (*engine)(nil)
)

// limitCall is the last Share Limit a Download got; Cleared means the
// global default.
type limitCall struct {
	Limit   core.ShareLimit
	Cleared bool
}

func newEngine(t testing.TB, sources core.TorrentSourceRepo) *engine {
	return &engine{
		t: t, sources: sources, torrents: map[string]torrent{}, byID: map[core.ID]string{},
		files: map[string][]core.TorrentFile{}, limits: map[core.ID]limitCall{},
	}
}

// of returns the torrent of a Download.
func (e *engine) of(ctx context.Context, d core.Download) (torrent, bool) {
	e.mu.Lock()
	h, ok := e.byID[d.ID]
	t := e.torrents[h]
	e.mu.Unlock()
	if ok {
		return t, true
	}
	src, err := e.sources.Get(ctx, d.ID)
	if err != nil {
		return torrent{}, false
	}
	return torrent{DownloadID: d.ID, InfoHash: src.InfoHash.String(), Name: d.Name}, true
}

// Live implements view.LiveSource.
func (e *engine) Live(ctx context.Context, d core.Download) view.Live {
	t, ok := e.of(ctx, d)
	if !ok {
		return view.Live{}
	}
	dir := t.ContentPath
	if dir == "" && t.SavePath != "" {
		dir = filepath.Join(t.SavePath, t.Name)
	}
	e.mu.Lock()
	files := e.files[t.InfoHash]
	e.mu.Unlock()
	return view.Live{
		DownRate: t.DownRate, UpRate: t.UpRate,
		Torrent: &view.TorrentLive{
			Hash: t.InfoHash, Registered: true, Size: t.Size, DoneBytes: t.DoneBytes,
			Peers: t.Peers, Seeds: t.Seeds, Private: t.Private,
			Uploaded: t.Uploaded, SeedingTime: t.SeedingTime,
			OwnRatio: t.OwnRatio, OwnSeedingTime: t.OwnSeeding, LimitPaused: t.Paused,
			Name: t.Name, Dir: dir, Files: files,
		},
	}
}

// SetShareLimit implements app.ShareLimitSetter.
func (e *engine) SetShareLimit(_ context.Context, id core.ID, l core.ShareLimit) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.limits[id] = limitCall{Limit: l}
	return nil
}

// ClearShareLimit implements app.ShareLimitSetter.
func (e *engine) ClearShareLimit(_ context.Context, id core.ID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.limits[id] = limitCall{Cleared: true}
	return nil
}

func (e *engine) limit(id core.ID) (limitCall, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.limits[id]
	return l, ok
}

// put registers a torrent for tests.
func (e *engine) put(t torrent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.torrents[t.InfoHash] = t
	e.byID[t.DownloadID] = t.InfoHash
}

// setFiles gives a torrent its files.
func (e *engine) setFiles(hash string, fs ...core.TorrentFile) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.files[hash] = fs
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
	sources    *store.TorrentSourceRepo
	adder      *app.AddService
	torrents   string // torrent complete root
	incomplete string
	user, pass string
	handler    http.Handler

	n int // download counter for AddedAt
}

type stackOption func(*stackConfig)

type stackConfig struct {
	deps    func(*qbit.Deps)
	seeding *core.SeedingSettings
}

func withDeps(f func(*qbit.Deps)) stackOption { return func(c *stackConfig) { c.deps = f } }

// withSeeding sets the Share Limit defaults of the Settings.
func withSeeding(s core.SeedingSettings) stackOption {
	return func(c *stackConfig) { c.seeding = &s }
}

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
	if sc.seeding != nil {
		cur.Seeding = *sc.seeding
	}
	require.NoError(t, s.settings.Update(ctx, cur))

	key := store.GenerateKey()
	s.sources = store.NewTorrentSourceRepo(db, key)
	s.engine = newEngine(t, s.sources)
	appOpts := []app.Option{
		app.WithClock(s.clock.Now), app.WithLogger(logger), app.WithDataRemover(s.remover),
		app.WithTorrentSources(s.sources), app.WithShareLimits(s.engine),
	}
	s.dls = app.NewDownloadService(s.downloads, s.cats, bus, appOpts...)
	s.adder = app.NewAddService(s.dls, store.NewDownloadSourceRepo(db, key), s.settings, appOpts...)
	s.categories = app.NewCategoryService(s.cats, s.downloads, bus, appOpts...)
	s.appKey, err = auth.NewAppKeyService(ctx, store.NewAppKeyRepo(db), auth.WithAppKeyLogger(logger))
	require.NoError(t, err)
	s.user, s.pass = auth.QBitUsername, s.appKey.Current().Reveal()
	s.limiter = auth.NewLoginLimiter(auth.WithLimiterClock(s.clock.Now))
	views, err := view.New(view.Config{Downloads: s.downloads, Live: s.engine, Settings: s.settings.Current})
	require.NoError(t, err)

	deps := qbit.Deps{
		AppKey: s.appKey, Downloads: s.dls, Categories: s.categories, Settings: s.settings,
		Adder: s.adder, View: views,
		Limiter: s.limiter, Logger: logger, Clock: s.clock.Now,
	}
	if sc.deps != nil {
		sc.deps(&deps)
	}
	s.handler = qbit.New(deps)
	return s
}

// seed stores a Queued torrent Download and its torrent. mutate may change
// either.
func (s *stack) seed(name string, mutate ...func(*core.Download, *torrent)) (core.Download, torrent) {
	s.t.Helper()
	s.n++
	at := epoch.Add(time.Duration(s.n) * time.Minute)
	d := core.Download{
		ID: core.NewID(), Name: name, SourceKind: core.SourceTorrent, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, QueuePosition: s.n - 1, AddedAt: at, UpdatedAt: at,
	}
	sum := sha1.Sum([]byte(name)) //nolint:gosec
	v := torrent{InfoHash: hex.EncodeToString(sum[:]), Name: name}
	for _, m := range mutate {
		m(&d, &v)
	}
	v.DownloadID = d.ID
	require.NoError(s.t, s.downloads.Create(context.Background(), d))
	s.addSource(d.ID, v.InfoHash)
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

func inState(st core.State) func(*core.Download, *torrent) {
	return func(d *core.Download, _ *torrent) { d.State = st }
}

func ready(path string) func(*core.Download, *torrent) {
	return func(d *core.Download, v *torrent) {
		d.State, d.OutputPath = core.StateReady, path
		d.FetchedAt = d.AddedAt.Add(time.Minute)
		d.ReadyAt = d.AddedAt.Add(3 * time.Minute)
		v.ContentPath = path
	}
}

func sized(total, done int64) func(*core.Download, *torrent) {
	return func(d *core.Download, v *torrent) {
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
	s.addSource(d.ID, hex.EncodeToString(sum[:]))
	s.engine.put(torrent{
		DownloadID: d.ID, InfoHash: hex.EncodeToString(sum[:]), Name: d.Name, Size: d.TotalBytes,
		DoneBytes: d.DoneBytes, DownRate: 1 << 20,
	})
}

// addSource stores the torrent source of a Download, which the info hash
// lookup reads. A hash that is not one (a test of bad engine data) has none.
func (s *stack) addSource(id core.ID, hash string) {
	s.t.Helper()
	h, err := core.ParseInfoHash(hash)
	if err != nil {
		return
	}
	require.NoError(s.t, s.sources.Create(context.Background(), core.TorrentSource{DownloadID: id, InfoHash: h, TorrentPath: "/t/" + hash + ".torrent"}))
}
