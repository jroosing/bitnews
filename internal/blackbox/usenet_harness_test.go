package blackbox_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// Test support of the TestUsenet* tests: a config dir that points the daemon
// at fake NNTP servers, an event log, and finding the decoded files.
//
// The tests assert on what both daemons do. The Go daemon goes on to
// post-processing and ready within moments; the Zig daemon of milestone Z3
// stops at fetched. So the tests look at the events up to "fetched", at the
// files wherever they are (the work dir <incomplete>/<id>/, or the complete
// dir once Go has moved them), and accept fetched, post_processing and ready
// as the state afterwards, never failed.

// pollEvery is the interval of every wait loop.
const pollEvery = 20 * time.Millisecond

// longWait is how long a wait loop gives the daemon: generous, because the
// loops return as soon as the condition holds.
const longWait = 60 * time.Second

// eventually polls cond until it holds or the timeout passes.
func eventually(t testing.TB, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", timeout, msg)
		}
		time.Sleep(pollEvery)
	}
}

// holds reports whether cond keeps holding for the whole window. It is the
// only way to check that something does not happen.
func holds(window time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if !cond() {
			return false
		}
		time.Sleep(pollEvery)
	}
	return cond()
}

// serverSpec describes a Server row for the daemon: where the fake is and
// how the daemon should use it.
type serverSpec struct {
	Name           string
	Host           string
	Port           int
	Username       string
	Password       string
	MaxConnections int
	RetentionDays  int
	Priority       int
	Disabled       bool
}

type specOption func(*serverSpec)

func specPriority(p int) specOption     { return func(s *serverSpec) { s.Priority = p } }
func specConns(n int) specOption        { return func(s *serverSpec) { s.MaxConnections = n } }
func specRetention(days int) specOption { return func(s *serverSpec) { s.RetentionDays = days } }
func specDisabled() specOption          { return func(s *serverSpec) { s.Disabled = true } }
func specPassword(p string) specOption  { return func(s *serverSpec) { s.Password = p } }
func (s serverSpec) with(o ...specOption) serverSpec {
	for _, f := range o {
		f(&s)
	}
	return s
}

var specCounter int

// specFor is the Server row for srv with the credentials the fake wants, two
// connections and priority 1.
func specFor(srv *nntpServer, opts ...specOption) serverSpec {
	specCounter++
	s := serverSpec{
		Name: fmt.Sprintf("Fake %d", specCounter), Host: srv.Host(), Port: srv.Port(),
		Username: serverUsername, Password: serverPassword, MaxConnections: 2, Priority: 1,
	}
	return s.with(opts...)
}

// usenetSetup is a config dir whose Servers are the given fakes, with data
// roots in temp dirs and a small disk reserve.
type usenetSetup struct {
	t     *testing.T
	cfg   configDir
	roots dataRoots
	ca    *testCA
}

// newUsenetSetup replaces the Server of the prepared config dir with specs
// (none means no Server at all, so Downloads stay queued) and stores Settings
// with temp-dir roots. tune may change the Settings.
func newUsenetSetup(t *testing.T, ca *testCA, specs []serverSpec, tune ...func(*core.Settings)) *usenetSetup {
	t.Helper()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	s := core.DefaultSettings()
	s.Paths.Usenet = core.Roots{Incomplete: roots.usenetIncomplete, Complete: roots.complete}
	s.Paths.Torrent = core.Roots{Incomplete: roots.torrentIncomplete, Complete: roots.torrentComplete}
	s.Queues.MinFree = 1 << 20
	for _, f := range tune {
		f(&s)
	}
	saveSettings(t, cfg, s)

	ctx := t.Context()
	db := openDB(t, cfg)
	key, err := store.LoadOrCreateKey(cfg.KeyFile, discard)
	require.NoError(t, err)
	repo := store.NewServerRepo(db, key)
	require.NoError(t, repo.Delete(ctx, cfg.ServerID))
	for _, sp := range specs {
		require.NoError(t, repo.Create(ctx, core.Server{
			ID: core.NewID(), Name: sp.Name, Host: sp.Host, Port: sp.Port,
			Username: sp.Username, Password: core.Secret(sp.Password),
			MaxConnections: sp.MaxConnections, RetentionDays: sp.RetentionDays,
			Priority: sp.Priority, Enabled: !sp.Disabled, CreatedAt: time.Now(),
		}))
	}
	require.NoError(t, db.Close())
	return &usenetSetup{t: t, cfg: cfg, roots: roots, ca: ca}
}

// start starts the daemon on the config dir; it trusts the test CA.
func (u *usenetSetup) start(opts ...startOption) *daemon {
	u.t.Helper()
	return startDaemon(u.t, u.cfg, append([]startOption{withEnv("SSL_CERT_FILE", u.ca.File)}, opts...)...)
}

// workDir is where the Download's files are while it is fetched.
func (u *usenetSetup) workDir(id string) string { return filepath.Join(u.roots.usenetIncomplete, id) }

// ---- the event log ----

// eventLog collects every event of the stream from the moment it is opened.
type eventLog struct {
	t      *testing.T
	mu     sync.Mutex
	frames []frame
}

// watch opens the event stream and collects it in the background.
func (d *daemon) watch() *eventLog {
	d.t.Helper()
	s := d.openStreamAfterHello()
	l := &eventLog{t: d.t}
	go func() {
		for f := range s.frames {
			l.mu.Lock()
			l.frames = append(l.frames, f)
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *eventLog) all() []frame {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.frames)
}

// idOf is the "id" of the event's data, or "".
func idOf(t testing.TB, f frame) string {
	t.Helper()
	id, _ := fjson(t, f)["id"].(string)
	return id
}

// of returns the events about one Download in order.
func (l *eventLog) of(id string) []frame {
	var out []frame
	for _, f := range l.all() {
		if f.name != "hello" && idOf(l.t, f) == id {
			out = append(out, f)
		}
	}
	return out
}

// count counts the events with this name about the Download.
func (l *eventLog) count(name, id string) int {
	n := 0
	for _, f := range l.of(id) {
		if f.name == name {
			n++
		}
	}
	return n
}

// states lists the states announced by download_added and download_updated
// events of the Download, in order, without repeats.
func (l *eventLog) states(id string) []string {
	var out []string
	for _, f := range l.of(id) {
		if f.name != "download_added" && f.name != "download_updated" {
			continue
		}
		st, _ := fjson(l.t, f)["state"].(string)
		if len(out) == 0 || out[len(out)-1] != st {
			out = append(out, st)
		}
	}
	return out
}

// firstIndex is the index in of(id) of the first event for which match is
// true, or -1.
func (l *eventLog) firstIndex(id string, match func(frame) bool) int {
	return slices.IndexFunc(l.of(id), match)
}

// stateIs matches a download_updated or download_added event of the state.
func stateIs(t testing.TB, state string) func(frame) bool {
	return func(f frame) bool {
		return (f.name == "download_updated" || f.name == "download_added") && fjson(t, f)["state"] == state
	}
}

// ---- adding and reading Downloads ----

// multipartBody builds a form with the file part "nzb" and the text fields
// (name, value pairs). It returns the body and its Content-Type.
func multipartBody(fileName string, content []byte, fields ...string) (string, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="nzb"; filename=%q`, fileName))
	h.Set("Content-Type", "application/x-nzb")
	p, _ := w.CreatePart(h)
	_, _ = p.Write(content)
	for i := 0; i+1 < len(fields); i += 2 {
		_ = w.WriteField(fields[i], fields[i+1])
	}
	_ = w.Close()
	return buf.String(), w.FormDataContentType()
}

// upload posts an NZB as a multipart upload.
func (d *daemon) upload(fileName string, nzb []byte, fields ...string) response {
	d.t.Helper()
	body, ct := multipartBody(fileName, nzb, fields...)
	return d.request(http.MethodPost, "/api/v1/downloads", map[string]string{
		"Authorization": "Bearer " + d.cfg.AppKey, "Content-Type": ct,
	}, body)
}

// addRelease uploads the NZB of rel and returns the new Download.
func (d *daemon) addRelease(rel release, fields ...string) map[string]any {
	d.t.Helper()
	r := d.upload("release.nzb", rel.NZB, fields...)
	require.Equal(d.t, http.StatusCreated, r.status, r.body)
	return obj(d.t, r)
}

// download reads one Download.
func (d *daemon) download(id string) map[string]any {
	d.t.Helper()
	r := d.apiGet("/api/v1/downloads/" + id)
	requireOK(d.t, r)
	return obj(d.t, r)
}

func (d *daemon) stateOf(id string) string {
	d.t.Helper()
	s, _ := d.download(id)["state"].(string)
	return s
}

// listed reads the Download from the list route.
func (d *daemon) listed(id string) map[string]any {
	d.t.Helper()
	for _, it := range itemsOf(d.t, d.apiGet("/api/v1/downloads?limit=200")) {
		if it["id"] == id {
			return it
		}
	}
	return nil
}

func num(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

// lifecycleRank orders the states of a Download that is neither paused nor
// pre-empted: the list route must never show one earlier than the get route
// read just before it.
func lifecycleRank(state string) int {
	switch state {
	case "added", "queued":
		return 0
	case "fetching":
		return 1
	case "fetched":
		return 2
	case "post_processing":
		return 3
	}
	return 4 // ready, failed
}

// waitFetched waits for the "fetched" event of the Download. A failure
// before it fails the test at once.
func waitFetched(t testing.TB, ev *eventLog, id string) {
	t.Helper()
	eventually(t, longWait, "the fetched event of "+id, func() bool {
		sawFetched := false
		for _, f := range ev.of(id) {
			switch f.name {
			case "fetched":
				sawFetched = true
			case "failed":
				if !sawFetched {
					t.Fatalf("download %s failed before it was fetched: %s", id, f.data)
				}
			}
		}
		return sawFetched
	})
}

// settle waits briefly for the Download to leave fetched/post_processing
// (the Go daemon does within moments; the Zig daemon of Z3 never does) and
// returns what it looks like then.
func settle(d *daemon, id string) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(2500 * time.Millisecond)
	for {
		m := d.download(id)
		st := m["state"]
		if (st != "fetched" && st != "post_processing") || time.Now().After(deadline) {
			return m
		}
		time.Sleep(pollEvery)
	}
}

// requireNotFailed asserts the Download is fetched, post_processing or ready.
func requireNotFailed(t testing.TB, m map[string]any) {
	t.Helper()
	assert.Contains(t, []string{"fetched", "post_processing", "ready"}, m["state"], "download %v: %v %v", m["id"], m["failure_reason"], m["failure_detail"])
}

// ---- finding the decoded files ----

// fileHit is where a file with some content was found.
type fileHit struct {
	Path string
	// InWork is true when it is in <incomplete>/<id>/ (not moved yet).
	InWork bool
}

// findByContent looks for a regular file with exactly these bytes in the
// Download's work dir, then anywhere under the complete root: the Go daemon
// moves the work dir there when it is done. The work dir is searched first so
// that a move between the two searches cannot hide the file.
func (u *usenetSetup) findByContent(id string, data []byte) (fileHit, bool) {
	var hit fileHit
	found := false
	walk := func(root string, inWork bool) {
		_ = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || found || !e.Type().IsRegular() {
				return nil
			}
			if fi, err := e.Info(); err != nil || fi.Size() != int64(len(data)) {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Equal(b, data) {
				hit, found = fileHit{Path: p, InWork: inWork}, true
			}
			return nil
		})
	}
	walk(u.workDir(id), true)
	if !found {
		walk(u.roots.complete, false)
	}
	return hit, found
}

// requireFiles waits until every file of rel exists, byte for byte, in the
// work dir or the complete dir, and checks the names the daemon gave them
// (the yEnc names), whichever dir they are in.
func (u *usenetSetup) requireFiles(t testing.TB, id string, rel release) {
	t.Helper()
	for _, f := range rel.Files {
		var hit fileHit
		eventually(t, 15*time.Second, fmt.Sprintf("file %q of %s with the right bytes", f.Name, id), func() bool {
			var ok bool
			hit, ok = u.findByContent(id, f.Data)
			return ok
		})
		assert.Equal(t, f.Name, filepath.Base(hit.Path), "the file is named by the yEnc header")
	}
}

// workFiles lists the names of the regular files directly in the work dir.
func (u *usenetSetup) workFiles(id string) []string {
	ents, err := os.ReadDir(u.workDir(id))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// requireFetchEvents checks the events of a Download that was fetched whole:
// added, fetching, progress that only grows, the final progress event, the
// fetched state and then the fetched event.
func requireFetchEvents(t testing.TB, ev *eventLog, id string, rel release) {
	t.Helper()
	evs := ev.of(id)
	require.NotEmpty(t, evs)
	assert.Equal(t, "download_added", evs[0].name, "the add is announced first")
	assert.Equal(t, "queued", fjson(t, evs[0])["state"])

	iFetching := ev.firstIndex(id, stateIs(t, "fetching"))
	iFetchedState := ev.firstIndex(id, stateIs(t, "fetched"))
	iFetchedEvent := ev.firstIndex(id, func(f frame) bool { return f.name == "fetched" })
	require.GreaterOrEqual(t, iFetching, 1, "fetching is announced")
	require.Greater(t, iFetchedState, iFetching)
	require.Greater(t, iFetchedEvent, iFetchedState, "the fetched event follows the fetched state change")

	var lastDone int64 = -1
	nProgress := 0
	for i, f := range evs[:iFetchedEvent] {
		if f.name != "download_progress" {
			continue
		}
		nProgress++
		m := fjson(t, f)
		assert.Greater(t, i, iFetching, "progress only while fetching")
		done, total := num(m, "done_bytes"), num(m, "total_bytes")
		assert.GreaterOrEqual(t, done, lastDone, "done_bytes never goes down")
		assert.LessOrEqual(t, done, total)
		assert.Equal(t, rel.Total, total, "total_bytes is the sum of the NZB segment sizes")
		assert.GreaterOrEqual(t, num(m, "bytes_per_second"), int64(0))
		lastDone = done
	}
	assert.GreaterOrEqual(t, nProgress, 1, "at least one progress event")
	assert.Equal(t, rel.Decoded(), lastDone, "the last progress event before fetched reports every decoded byte")

	fm := fjson(t, evs[iFetchedState])
	assert.Equal(t, rel.Decoded(), num(fm, "done_bytes"), "done_bytes of the fetched state is the decoded size")
	assert.Equal(t, rel.Total, num(fm, "total_bytes"))
	assert.NotNil(t, fm["fetched_at"])
	assert.Nil(t, fm["ready_at"])
	assert.Equal(t, map[string]any{"id": id}, fjson(t, evs[iFetchedEvent]))
	assert.Empty(t, fm["failure_reason"])
}

// fjson decodes the data of an event as an object.
func fjson(t testing.TB, f frame) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.data), &m), "event %s: %q", f.name, f.data)
	return m
}

// requireFetchedOrMissingArticles asserts the state after a fetch with lost
// or damaged articles and no PAR2 set. The Go daemon post-processes the
// Download and fails it with missing_articles; the Zig daemon of Z3 stops at
// fetched. Nothing else is acceptable.
func requireFetchedOrMissingArticles(t testing.TB, m map[string]any) {
	t.Helper()
	switch m["state"] {
	case "fetched", "post_processing":
	case "failed":
		assert.Equal(t, "missing_articles", m["failure_reason"], "%v", m["failure_detail"])
	default:
		t.Errorf("a download with lost articles and no PAR2 set cannot be %v", m["state"])
	}
}

// firstIndexAll is the index in the whole log of the first event matching.
func (l *eventLog) firstIndexAll(match func(frame) bool) int {
	return slices.IndexFunc(l.all(), match)
}

// stateIsID matches a download_added or download_updated event of one
// Download in the state.
func stateIsID(t testing.TB, id, state string) func(frame) bool {
	return func(f frame) bool {
		if f.name != "download_updated" && f.name != "download_added" {
			return false
		}
		m := fjson(t, f)
		return m["id"] == id && m["state"] == state
	}
}

// addServerTo stores one more Server row in the stopped config dir of u.
func addServerTo(t *testing.T, u *usenetSetup, sp serverSpec) {
	t.Helper()
	db := openDB(t, u.cfg)
	key, err := store.LoadOrCreateKey(u.cfg.KeyFile, discard)
	require.NoError(t, err)
	require.NoError(t, store.NewServerRepo(db, key).Create(t.Context(), core.Server{
		ID: core.NewID(), Name: sp.Name, Host: sp.Host, Port: sp.Port, Username: sp.Username,
		Password: core.Secret(sp.Password), MaxConnections: sp.MaxConnections, Priority: sp.Priority,
		Enabled: !sp.Disabled, CreatedAt: time.Now(),
	}))
	require.NoError(t, db.Close())
}

// dialTLSAndHold opens a TLS connection to srv that trusts the test CA and
// keeps it open until the test ends, so it holds one of the server's slots.
func dialTLSAndHold(t *testing.T, ca *testCA, srv *nntpServer) net.Conn {
	t.Helper()
	pemBytes, err := os.ReadFile(ca.File)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(pemBytes))
	c, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", srv.Port()), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	br := make([]byte, 64)
	_, err = c.Read(br)
	require.NoError(t, err)
	return c
}

// waitFetchedState waits until the Download has been fetched, by its state,
// for tests that cannot have the event stream open early enough (a daemon
// that was just started may be done before the stream is open).
func waitFetchedState(t testing.TB, d *daemon, id string) {
	t.Helper()
	eventually(t, longWait, "the Download to be fetched", func() bool {
		m := d.download(id)
		switch m["state"] {
		case "fetched", "post_processing", "ready":
			return true
		case "failed":
			t.Fatalf("download %s failed: %v %v", id, m["failure_reason"], m["failure_detail"])
		}
		return false
	})
}
