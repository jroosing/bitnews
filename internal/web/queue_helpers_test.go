package web_test

import (
	"bufio"
	"context"
	"io"
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

	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// fakeRuntime is a RuntimeSource whose answers the tests set.
type fakeRuntime struct {
	mu       sync.Mutex
	down, up int64
	queues   map[core.SourceKind]web.QueueState
	limits   map[core.SourceKind][2]int64
	calls    []string
	// gate, when set, makes Rates wait until it is closed or sent to.
	gate chan struct{}
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		queues: map[core.SourceKind]web.QueueState{
			core.SourceNZB:     {Available: true},
			core.SourceTorrent: {Available: true},
		},
		limits: map[core.SourceKind][2]int64{},
	}
}

func (f *fakeRuntime) Rates() (int64, int64) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.down, f.up
}

// blockRates makes Rates wait until the returned function is called.
func (f *fakeRuntime) blockRates() (release func()) {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	return func() { close(gate) }
}

func (f *fakeRuntime) setRates(down, up int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down, f.up = down, up
}

func (f *fakeRuntime) QueueState(k core.SourceKind) web.QueueState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queues[k]
}

func (f *fakeRuntime) setQueue(k core.SourceKind, st web.QueueState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[k] = st
}

func (f *fakeRuntime) PauseQueue(k core.SourceKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "pause "+string(k))
	st := f.queues[k]
	st.Paused, st.Reason = true, web.PauseReasonUser
	f.queues[k] = st
}

func (f *fakeRuntime) ResumeQueue(k core.SourceKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "resume "+string(k))
	st := f.queues[k]
	st.Paused, st.Reason = false, ""
	f.queues[k] = st
}

func (f *fakeRuntime) SpeedLimits(k core.SourceKind) (int64, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.limits[k]
	return l[0], l[1]
}

func (f *fakeRuntime) SetSpeedLimit(k core.SourceKind, down, up int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "limit "+string(k))
	f.limits[k] = [2]int64{down, up}
}

func (f *fakeRuntime) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeTicks hands out tickers the test fires by hand. A ticker is told apart
// by its period.
type fakeTicks struct {
	mu sync.Mutex
	ch map[time.Duration]chan time.Time
}

func newFakeTicks() *fakeTicks { return &fakeTicks{ch: map[time.Duration]chan time.Time{}} }

func (f *fakeTicks) newTicker(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.ch[d]
	if !ok {
		c = make(chan time.Time, 1)
		f.ch[d] = c
	}
	return c, func() {}
}

// fire makes the ticker with period d tick once.
func (f *fakeTicks) fire(d time.Duration) {
	f.mu.Lock()
	c := f.ch[d]
	f.mu.Unlock()
	if c == nil {
		return
	}
	select {
	case c <- time.Now():
	default:
	}
}

// Periods the tests configure.
const (
	testTick = 1000 * time.Millisecond
	testPing = 15000 * time.Millisecond
)

// qenv is the whole stack with a queue: real store, services, bus and view
// module, a fake RuntimeSource and a fake LiveSource (env.live).
type qenv struct {
	*env
	repo       *store.DownloadRepo
	svc        *app.DownloadService
	categories *app.CategoryService
	bus        *core.Bus
	rt         *fakeRuntime
	ticks      *fakeTicks
	server     *web.Server
	b          *browser
	tok        string // CSRF token of b
	n          int
}

type qopt func(*qcfg)

type qcfg struct {
	noRuntime bool
	live      web.LiveConfig
}

func withoutRuntime() qopt { return func(c *qcfg) { c.noRuntime = true } }

func withLive(f func(*web.LiveConfig)) qopt { return func(c *qcfg) { f(&c.live) } }

// newQueueEnv builds the stack and signs in.
func newQueueEnv(t *testing.T, opts ...qopt) *qenv {
	t.Helper()
	var cfg qcfg
	q := &qenv{rt: newFakeRuntime(), ticks: newFakeTicks()}
	cfg.live = web.LiveConfig{Tick: testTick, Ping: testPing, NewTicker: q.ticks.newTicker}
	for _, o := range opts {
		o(&cfg)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	e := &env{t: t, clock: &clock{t: epoch.Add(24 * time.Hour)}, log: &syncBuffer{}, live: newFakeLive()}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", e.log.String())
		}
	})
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	q.bus = core.NewBus()
	users := store.NewUserRepo(db)
	q.repo = store.NewDownloadRepo(db)
	cats := store.NewCategoryRepo(db)
	key := store.GenerateKey()
	servers := store.NewServerRepo(db, key)
	require.NoError(t, users.Create(ctx, core.User{
		ID: core.NewID(), Username: "admin", PasswordHash: sharedAdminHash(t), CreatedAt: epoch,
	}))
	e.settings, err = app.NewSettingsService(ctx, store.NewSettingsRepo(db), q.bus)
	require.NoError(t, err)
	e.setup, err = auth.NewSetup(ctx, users, logger, auth.WithClock(e.clock.Now))
	require.NoError(t, err)
	e.sessions, err = auth.NewSessionService(store.NewSessionRepo(db), users,
		auth.WithSessionClock(e.clock.Now), auth.WithSessionLogger(logger))
	require.NoError(t, err)
	uiAuth, err := auth.NewUIAuth(e.sessions, e.settings.Current)
	require.NoError(t, err)

	appOpts := []app.Option{app.WithClock(e.clock.Now)}
	q.svc = app.NewDownloadService(q.repo, cats, q.bus, appOpts...)
	sources := store.NewDownloadSourceRepo(db, key)
	appKey, err := auth.NewAppKeyService(ctx, store.NewAppKeyRepo(db), auth.WithAppKeyLogger(logger))
	require.NoError(t, err)
	catSvc := app.NewCategoryService(cats, q.repo, q.bus, appOpts...)
	serverSvc := app.NewServerService(servers, q.bus, nil, appOpts...)
	q.categories = catSvc
	deps := web.Deps{
		Logger: logger, Version: "1.2.3",
		Sessions: e.sessions, UIAuth: uiAuth, Setup: e.setup, Settings: e.settings,
		Downloads: q.svc, Categories: catSvc, Servers: serverSvc, Bus: q.bus,
		View: newView(t, q.repo, e.live, e.settings),
		Live: cfg.live,
	}
	if !cfg.noRuntime {
		deps.Runtime = q.rt
	}
	q.server, err = web.New(deps)
	require.NoError(t, err)
	srv, err := api.New(api.Config{
		Logger:     logger,
		Downloads:  q.svc,
		Adder:      app.NewAddService(q.svc, sources, e.settings, append(appOpts, app.WithLogger(logger))...),
		Categories: catSvc,
		Servers:    serverSvc,
		Settings:   e.settings,
		Setup:      e.setup,
		Sessions:   e.sessions,
		UIAuth:     uiAuth,
		AppKey:     appKey,
		Bus:        q.bus,
		Web:        q.server,
	})
	require.NoError(t, err)
	e.handler = srv.Handler()
	q.env = e
	q.b = e.browser()
	q.b.login()
	q.tok = q.b.csrf()
	return q
}

// add stores a Download. Zero fields get defaults: kind nzb, state queued,
// priority normal, a name, and the next queue position.
func (q *qenv) add(d core.Download) core.Download {
	q.t.Helper()
	q.n++
	if d.ID == "" {
		d.ID = core.NewID()
	}
	if d.Name == "" {
		d.Name = "Download " + strings.Repeat("x", q.n)
	}
	if d.SourceKind == "" {
		d.SourceKind = core.SourceNZB
	}
	if d.State == "" {
		d.State = core.StateQueued
	}
	if d.Priority == "" {
		d.Priority = core.DownloadPriorityNormal
	}
	if d.QueuePosition == 0 {
		d.QueuePosition = q.n
	}
	d.AddedAt = epoch.Add(time.Duration(q.n) * time.Minute)
	d.UpdatedAt = d.AddedAt
	require.NoError(q.t, q.repo.Create(context.Background(), d))
	return d
}

// get reads a Download back.
func (q *qenv) get(id core.ID) core.Download {
	q.t.Helper()
	d, err := q.repo.Get(context.Background(), id)
	require.NoError(q.t, err)
	return d
}

// act posts to a queue action like htmx does: the CSRF header, HX-Request
// and the form values.
func (q *qenv) act(path string, form url.Values) *httptest.ResponseRecorder {
	q.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	return q.b.post(path, form, htmx(), header("X-CSRF-Token", q.tok))
}

// ---- server-sent events -------------------------------------------------

// sseEvent is one parsed event of a stream.
type sseEvent struct {
	Name string
	Data string
}

// sseStream is a client of /ui/events over real HTTP.
type sseStream struct {
	t      *testing.T
	events chan sseEvent
	lines  chan string // comment lines such as ": ping"
	cancel context.CancelFunc
	resp   *http.Response
	done   chan struct{}
}

// openStream signs in on a real HTTP server in front of the stack and opens
// the stream. query is appended to /ui/events.
func (q *qenv) openStream(t *testing.T, query string) *sseStream {
	t.Helper()
	ts := httptest.NewServer(q.handler)
	t.Cleanup(ts.Close)
	return q.openStreamAt(t, ts, query)
}

func (q *qenv) openStreamAt(t *testing.T, ts *httptest.Server, query string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/ui/events"+query, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	for _, c := range q.b.cookies {
		req.AddCookie(c)
	}
	req.Host = "example.com"
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	require.NoError(t, err)
	s := &sseStream{t: t, events: make(chan sseEvent, 256), lines: make(chan string, 64), cancel: cancel, resp: resp, done: make(chan struct{})}
	t.Cleanup(s.close)
	go s.read()
	return s
}

func (s *sseStream) read() {
	defer close(s.done)
	defer close(s.events)
	sc := bufio.NewScanner(s.resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var cur sseEvent
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Name != "" || len(data) > 0 {
				cur.Data = strings.Join(data, "\n")
				s.events <- cur
			}
			cur, data = sseEvent{}, nil
		case strings.HasPrefix(line, ":"):
			select {
			case s.lines <- line:
			default:
			}
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		case line == "data:":
			data = append(data, "")
		}
	}
}

func (s *sseStream) close() {
	s.cancel()
	_ = s.resp.Body.Close()
	<-s.done
}

// next waits for the next event.
func (s *sseStream) next() sseEvent {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		require.True(s.t, ok, "stream ended")
		return ev
	case <-time.After(5 * time.Second):
		s.t.Fatal("no event within 5s")
	}
	return sseEvent{}
}

// until reads events until one is called name and returns it.
func (s *sseStream) until(name string) sseEvent {
	s.t.Helper()
	for {
		if ev := s.next(); ev.Name == name {
			return ev
		}
	}
}

// ended reports whether the stream has ended, waiting a little for it.
func (s *sseStream) ended() bool {
	select {
	case <-s.done:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

// quiet fails if an event arrives within a short while.
func (s *sseStream) quiet(d time.Duration) {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if ok {
			s.t.Fatalf("unexpected event %q: %s", ev.Name, ev.Data)
		}
	case <-time.After(d):
	}
}

var _ = io.Discard

// allIDs lists the IDs of every Download.
func (q *qenv) allIDs() []core.ID {
	q.t.Helper()
	var ids []core.ID
	for _, st := range []core.State{core.StateQueued, core.StateFetching} {
		page, _, err := q.repo.List(context.Background(), core.DownloadQuery{State: st, Limit: 200})
		require.NoError(q.t, err)
		for _, d := range page {
			ids = append(ids, d.ID)
		}
	}
	return ids
}

// flush lets the stream take in the events published so far, then fires its
// Tick. The stream reads the bus in its own goroutine and there is no signal
// that it caught up, so this waits a moment.
func (q *qenv) flush() {
	time.Sleep(60 * time.Millisecond)
	q.ticks.fire(testTick)
}
