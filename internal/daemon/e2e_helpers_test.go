package daemon_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
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
	"gitlab.com/kyura4k/gitnews/internal/daemon"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
)

// These helpers build the whole daemon on a temp-dir database and in-memory
// NNTP servers, and drive it through its HTTP API like a client would.

var (
	adminHashOnce sync.Once
	adminHash     []byte
)

func sharedAdminHash(t *testing.T) []byte {
	t.Helper()
	adminHashOnce.Do(func() {
		var err error
		adminHash, err = auth.HashPassword(core.Secret("correct horse battery staple"))
		require.NoError(t, err)
	})
	return adminHash
}

// fakeNNTP is an in-memory Server that only accepts one password.
type fakeNNTP struct {
	*nntptest.Server
	mu   sync.Mutex
	pass string
}

func (f *fakeNNTP) setPass(p string) {
	f.mu.Lock()
	f.pass = p
	f.mu.Unlock()
}

func (f *fakeNNTP) wantPass() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pass
}

type okProber struct{}

func (okProber) Probe(context.Context, core.Server) app.ProbeResult {
	return app.ProbeResult{OK: true, Class: app.ProbeOK}
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type e2e struct {
	t          *testing.T
	root       string
	incomplete string
	complete   string
	db         *store.DB
	key        store.Key
	apiKey     string
	logs       *syncBuf
	tune       daemon.Tuning
	// torrents turns the torrent side on (it is off in the Usenet tests).
	torrents bool
	// torrentOpts configures the engine Build makes when none is injected.
	torrentOpts daemon.TorrentOptions
	// engine is the torrent engine to inject when torrents is on.
	engine core.TorrentEngine
	// noAdmin leaves the database without a user, so first-run setup is open.
	noAdmin bool
	// prober replaces the connection tester of the Server tests (okProber when nil).
	prober app.Prober
	// tincomplete and tcomplete are the torrent roots.
	tincomplete, tcomplete string

	mu      sync.Mutex
	servers map[string]*fakeNNTP
	ids     map[string]core.ID // server name -> API id

	d       *daemon.Daemon
	cancel  context.CancelFunc
	runDone chan error
	ts      *httptest.Server
	client  *http.Client
	sse     *sseRecorder
}

type e2eOption func(*e2e)

// withoutAdmin starts the daemon in first-run setup mode.
func withoutAdmin() e2eOption { return func(e *e2e) { e.noAdmin = true } }

func newE2E(t *testing.T, opts ...e2eOption) *e2e {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	zero := uint64(0)
	e := &e2e{
		t: t, root: root,
		incomplete:  filepath.Join(root, "usenet", "incomplete"),
		complete:    filepath.Join(root, "usenet", "complete"),
		tincomplete: filepath.Join(root, "torrents", "incomplete"),
		tcomplete:   filepath.Join(root, "torrents", "complete"),
		logs:        &syncBuf{}, servers: map[string]*fakeNNTP{}, ids: map[string]core.ID{},
		key: store.GenerateKey(),
		tune: daemon.Tuning{
			PollInterval: 20 * time.Millisecond, SaveInterval: 25 * time.Millisecond,
			DoneBytesInterval: 20 * time.Millisecond, FailureBackoff: 150 * time.Millisecond,
			Backoff:   func(int) time.Duration { return 0 },
			FreeSpace: func(string) (uint64, error) { return 1 << 40, nil }, MinFree: &zero,
			PostProcPollInterval: 30 * time.Millisecond, SessionCleanup: time.Hour,
			TorrentSampleInterval: 20 * time.Millisecond, TorrentPersistInterval: 30 * time.Millisecond,
			TorrentLimitInterval: 50 * time.Millisecond,
		},
		client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(e)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", e.logs.String())
		}
	})

	var err error
	e.db, err = store.Open(ctx, filepath.Join(root, "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.db.Close() })

	settings := core.DefaultSettings()
	settings.Paths.Usenet = core.Roots{Incomplete: e.incomplete, Complete: e.complete}
	settings.Paths.Torrent = core.Roots{Incomplete: e.tincomplete, Complete: e.tcomplete}
	require.NoError(t, store.NewSettingsRepo(e.db).Save(ctx, settings))
	if !e.noAdmin {
		require.NoError(t, store.NewUserRepo(e.db).Create(ctx, core.User{
			ID: core.NewID(), Username: "admin", PasswordHash: sharedAdminHash(t), CreatedAt: time.Now(),
		}))
	}
	e.apiKey, _, err = auth.NewAPIKeyService(store.NewAPIKeyRepo(e.db)).Create(ctx, "e2e", core.APIKeyScopeWrite)
	require.NoError(t, err)
	return e
}

// factory is the runner's connection factory: it connects to the fake whose
// name the spec carries, and refuses a wrong password like a provider does.
func (e *e2e) factory(spec runner.ServerSpec) nntp.ConnFactory {
	e.mu.Lock()
	f := e.servers[spec.Name]
	e.mu.Unlock()
	return func(ctx context.Context) (nntp.ArticleConn, error) {
		if f == nil || spec.Pass != f.wantPass() {
			return nil, &nntp.Error{Kind: nntp.ErrAuth, Op: "auth", Code: 481, Text: "Authentication failed"}
		}
		return f.Factory()(ctx)
	}
}

// start builds a Daemon on the database and runs it with an HTTP test
// server in front.
func (e *e2e) start() {
	e.t.Helper()
	d := e.build()
	e.d = d
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.runDone = make(chan error, 1)
	go func() { e.runDone <- d.Run(ctx) }()
	e.ts = httptest.NewServer(d.Handler())
	if !e.noAdmin { // the event stream is gated while setup is open
		e.sse = e.connectSSE()
	}
	e.t.Cleanup(e.stop)
}

// build assembles a Daemon on the database without starting it.
func (e *e2e) build() *daemon.Daemon {
	e.t.Helper()
	logger := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := daemon.Build(context.Background(), daemon.Deps{
		DB: e.db, Key: e.key, Logger: logger, Version: api.Version{Version: "test", Commit: "test"},
		Factory: e.factory, Prober: e.proberOrOK(), Tuning: e.tune, DisableTorrent: !e.torrents, TorrentEngine: e.engine, Torrent: e.torrentOpts,
	})
	require.NoError(e.t, err)
	return d
}

func (e *e2e) proberOrOK() app.Prober {
	if e.prober != nil {
		return e.prober
	}
	return okProber{}
}

// stop shuts the daemon down in the order Serve uses: HTTP first, then the
// workers. It is safe to call twice.
func (e *e2e) stop() {
	if e.d == nil {
		return
	}
	if e.sse != nil {
		e.sse.close()
	}
	e.ts.Close()
	e.cancel()
	select {
	case err := <-e.runDone:
		require.NoError(e.t, err)
	case <-time.After(15 * time.Second):
		e.t.Error("Run did not return after cancel")
	}
	e.d, e.sse = nil, nil
}

// addServer registers an in-memory Server and creates it through the API.
func (e *e2e) addServer(name string, priority int) *fakeNNTP {
	e.t.Helper()
	f := &fakeNNTP{Server: nntptest.NewServer(name), pass: "secret"}
	e.mu.Lock()
	e.servers[name] = f
	e.mu.Unlock()
	var out struct {
		ID core.ID `json:"id"`
	}
	e.call("POST", "/api/v1/servers", map[string]any{
		"name": name, "host": name + ".example", "port": 563, "username": "u", "password": "secret",
		"max_connections": 4, "priority": priority,
	}, http.StatusCreated, &out)
	e.ids[name] = out.ID
	return f
}

// call sends a JSON request and decodes a JSON answer.
func (e *e2e) call(method, path string, body any, wantStatus int, out any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(e.t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	require.NoError(e.t, err)
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(e.t, err)
	require.Equal(e.t, wantStatus, resp.StatusCode, "%s %s: %s", method, path, raw)
	if out != nil {
		require.NoError(e.t, json.Unmarshal(raw, out), string(raw))
	}
}

type dl struct {
	ID            core.ID    `json:"id"`
	Name          string     `json:"name"`
	State         core.State `json:"state"`
	Paused        bool       `json:"paused"`
	FailureReason string     `json:"failure_reason"`
	FailureDetail string     `json:"failure_detail"`
	TotalBytes    int64      `json:"total_bytes"`
	DoneBytes     int64      `json:"done_bytes"`
	OutputPath    string     `json:"output_path"`
	CategoryID    *core.ID   `json:"category_id"`
}

// addNZB uploads the NZB. category may be empty.
func (e *e2e) addNZB(name string, nzb []byte, category core.ID) dl {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(e.t, mw.WriteField("name", name))
	if category != "" {
		require.NoError(e.t, mw.WriteField("category_id", string(category)))
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="nzb"; filename="%s.nzb"`, name))
	h.Set("Content-Type", "application/x-nzb")
	pw, err := mw.CreatePart(h)
	require.NoError(e.t, err)
	_, err = pw.Write(nzb)
	require.NoError(e.t, err)
	require.NoError(e.t, mw.Close())
	req, err := http.NewRequest("POST", e.ts.URL+"/api/v1/downloads", &buf)
	require.NoError(e.t, err)
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := e.client.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equal(e.t, http.StatusCreated, resp.StatusCode, string(raw))
	var out dl
	require.NoError(e.t, json.Unmarshal(raw, &out))
	return out
}

func (e *e2e) addCategory(name string) core.ID {
	e.t.Helper()
	var out struct {
		ID core.ID `json:"id"`
	}
	e.call("POST", "/api/v1/categories", map[string]any{"name": name}, http.StatusCreated, &out)
	return out.ID
}

func (e *e2e) get(id core.ID) dl {
	e.t.Helper()
	var out dl
	e.call("GET", "/api/v1/downloads/"+string(id), nil, http.StatusOK, &out)
	return out
}

// waitState polls the API until the Download is in state.
func (e *e2e) waitState(id core.ID, state core.State) dl {
	e.t.Helper()
	var d dl
	require.Eventually(e.t, func() bool {
		d = e.get(id)
		return d.State == state
	}, 20*time.Second, 10*time.Millisecond, "download %s never reached %s (last: %+v)", id, state, d)
	return d
}

// sseRecorder collects the event names and payloads of /api/v1/events.
type sseRecorder struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	events []sseEvent
}

type sseEvent struct {
	Name string
	Data string
}

func (e *e2e) connectSSE() *sseRecorder {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/api/v1/events", nil)
	require.NoError(e.t, err)
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	require.NoError(e.t, err)
	r := &sseRecorder{cancel: cancel, done: make(chan struct{})}
	hello := make(chan struct{})
	go func() {
		defer close(r.done)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Name != "" {
					r.mu.Lock()
					r.events = append(r.events, cur)
					r.mu.Unlock()
					if cur.Name == "hello" {
						close(hello)
					}
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				cur.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case <-hello:
	case <-time.After(5 * time.Second):
		e.t.Fatal("no hello on the event stream")
	}
	return r
}

func (r *sseRecorder) close() {
	r.cancel()
	<-r.done
}

func (r *sseRecorder) all() []sseEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sseEvent(nil), r.events...)
}

// names returns the event names for download id, in order: events with an
// "id" in their payload only count when it matches. Progress events are left
// out. A stage event is written as "postprocess_stage:<stage>".
func (r *sseRecorder) names(id core.ID) []string {
	var out []string
	for _, ev := range r.all() {
		if ev.Name == "hello" || ev.Name == "download_progress" {
			continue
		}
		var p struct {
			ID    core.ID `json:"id"`
			Stage string  `json:"stage"`
		}
		_ = json.Unmarshal([]byte(ev.Data), &p)
		if p.ID != id {
			continue
		}
		name := ev.Name
		if name == "postprocess_stage" {
			name += ":" + p.Stage
		}
		out = append(out, name)
	}
	return out
}

// subsequence reports whether want occurs in got in order (gaps allowed).
func subsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// readTree returns relative path -> content of the regular files under dir.
func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	require.NoError(t, filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = b
		return err
	}))
	return out
}

// payload returns n deterministic pseudo-random bytes.
func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// golden loads a real par2cmdline set: the payload files and the PAR2 files.
type goldenSet struct {
	Data     map[string][]byte
	Par2     map[string][]byte
	Manifest struct {
		Files []struct {
			Name  string `json:"name"`
			Size  int    `json:"size"`
			MD5   string `json:"md5"`
			InSet bool   `json:"in_set"`
		} `json:"files"`
		SliceSize int `json:"slice_size"`
	}
}

func loadGolden(t *testing.T, name string) goldenSet {
	t.Helper()
	base := filepath.Join("..", "postproc", "par2", "testdata", "golden", name)
	g := goldenSet{Data: map[string][]byte{}, Par2: map[string][]byte{}}
	read := func(dir string, into map[string][]byte) {
		ents, err := os.ReadDir(filepath.Join(base, dir))
		require.NoError(t, err)
		for _, ent := range ents {
			b, err := os.ReadFile(filepath.Join(base, dir, ent.Name()))
			require.NoError(t, err)
			into[ent.Name()] = b
		}
	}
	read("data", g.Data)
	read("par2", g.Par2)
	raw, err := os.ReadFile(filepath.Join(base, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &g.Manifest))
	return g
}

// files returns data and PAR2 files together, as a release posts them.
func (g goldenSet) files() map[string][]byte {
	out := map[string][]byte{}
	maps.Copy(out, g.Data)
	maps.Copy(out, g.Par2)
	return out
}

// fixtureIDs returns the message IDs of the named file.
func fixtureIDs(t *testing.T, fx nntptest.Fixture, name string) []string {
	t.Helper()
	for _, f := range fx.Files {
		if f.Name == name {
			return f.MessageIDs
		}
	}
	t.Fatalf("fixture has no file %s", name)
	return nil
}

// requestsOf sums the BODY requests srv got for ids.
func requestsOf(srv *fakeNNTP, ids []string) int {
	n := 0
	for _, id := range ids {
		n += srv.Requests(id)
	}
	return n
}
