package blackbox_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// Helpers of the TestAPI* tests (the own API, /api/v1).

// apiHeaders are the headers of an authenticated JSON request.
func (d *daemon) apiHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + d.cfg.AppKey, "Content-Type": "application/json"}
}

// api sends a request with the app key and, when there is a body, as JSON.
func (d *daemon) api(method, path, body string) response {
	d.t.Helper()
	h := d.apiHeaders()
	if body == "" {
		delete(h, "Content-Type")
	}
	return d.request(method, path, h, body)
}

// apiGet is GET with the app key.
func (d *daemon) apiGet(path string) response {
	d.t.Helper()
	return d.api(http.MethodGet, path, "")
}

// obj decodes a response body that is one JSON object.
func obj(t *testing.T, r response) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &m), r.body)
	return m
}

// items decodes {"items":[...]} of a 200 answer.
func itemsOf(t *testing.T, r response) []map[string]any {
	t.Helper()
	return list(t, r)
}

// requireOK asserts a 200 JSON answer with the API headers.
func requireOK(t *testing.T, r response) {
	t.Helper()
	require.Equal(t, http.StatusOK, r.status, r.body)
	requireAPIHeaders(t, r)
}

// requireNoContent asserts a 204 answer without a body.
func requireNoContent(t *testing.T, r response) {
	t.Helper()
	require.Equal(t, http.StatusNoContent, r.status, r.body)
	assert.Empty(t, r.body)
	requireSecurityHeaders(t, r)
}

// requireError asserts the error shape of the API: status, code and a
// non-empty message, and nothing else in the body. It returns the message.
func requireError(t *testing.T, r response, status int, code string) string {
	t.Helper()
	require.Equal(t, status, r.status, r.body)
	requireAPIHeaders(t, r)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(r.body), &top), r.body)
	require.Len(t, top, 1, r.body)
	var e map[string]any
	require.NoError(t, json.Unmarshal(top["error"], &e), r.body)
	assert.Equal(t, code, e["code"], r.body)
	msg, ok := e["message"].(string)
	assert.True(t, ok && msg != "", "message must be a non-empty string: %s", r.body)
	for k := range e {
		assert.Contains(t, []string{"code", "message"}, k, "unexpected error field in %s", r.body)
	}
	return msg
}

// requireErrorMessage is requireError for an error whose message the app
// layer wrote, so the exact text is part of the contract.
func requireErrorMessage(t *testing.T, r response, status int, code, message string) {
	t.Helper()
	assert.Equal(t, message, requireError(t, r, status, code))
}

// errBody is the exact body of an error answer.
func errBody(code, message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message}})
	return string(b) + "\n"
}

// timeAt is a fixed, whole-second time for fixtures.
var fixtureBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// newDownload returns a Download in state added with fixture defaults.
func newDownload(name string, mods ...func(*core.Download)) core.Download {
	d := core.Download{
		ID: core.NewID(), Name: name, SourceKind: core.SourceNZB, State: core.StateAdded,
		Priority: core.DownloadPriorityNormal, AddedAt: fixtureBase, UpdatedAt: fixtureBase,
	}
	for _, m := range mods {
		m(&d)
	}
	return d
}

// at sets AddedAt and UpdatedAt to the base time plus minutes.
func at(minutes int) func(*core.Download) {
	return func(d *core.Download) {
		d.AddedAt = fixtureBase.Add(time.Duration(minutes) * time.Minute)
		d.UpdatedAt = d.AddedAt
	}
}

func inState(s core.State) func(*core.Download) { return func(d *core.Download) { d.State = s } }
func torrent() func(*core.Download) {
	return func(d *core.Download) { d.SourceKind = core.SourceTorrent }
}
func position(p int) func(*core.Download) { return func(d *core.Download) { d.QueuePosition = p } }
func withPriority(p core.DownloadPriority) func(*core.Download) {
	return func(d *core.Download) { d.Priority = p }
}
func inCategory(id core.ID) func(*core.Download) { return func(d *core.Download) { d.CategoryID = id } }
func paused() func(*core.Download)               { return func(d *core.Download) { d.Paused = true } }

// seedDownloads inserts Downloads into the database of c with the Go store,
// before the daemon starts or while it runs.
func seedDownloads(t *testing.T, c configDir, ds ...core.Download) {
	t.Helper()
	db := openDB(t, c)
	repo := store.NewDownloadRepo(db)
	for _, d := range ds {
		require.NoError(t, repo.Create(t.Context(), d))
	}
	require.NoError(t, db.Close())
}

// storedDownload reads one Download from the database of c with the Go store.
func storedDownload(t *testing.T, c configDir, id core.ID) (core.Download, error) {
	t.Helper()
	db := openDB(t, c)
	defer func() { require.NoError(t, db.Close()) }()
	return store.NewDownloadRepo(db).Get(t.Context(), id)
}

// storedDownloads lists every Download of the database of c.
func storedDownloads(t *testing.T, c configDir) []core.Download {
	t.Helper()
	db := openDB(t, c)
	defer func() { require.NoError(t, db.Close()) }()
	ds, err := core.ListAll(t.Context(), store.NewDownloadRepo(db), core.DownloadQuery{})
	require.NoError(t, err)
	return ds
}

// saveSettings stores Settings in the database of c the way a previous run
// would have left them.
func saveSettings(t *testing.T, c configDir, s core.Settings) {
	t.Helper()
	db := openDB(t, c)
	require.NoError(t, store.NewSettingsRepo(db).Save(t.Context(), s))
	require.NoError(t, db.Close())
}

// storedSettings reads the Settings from the database of c.
func storedSettings(t *testing.T, c configDir) core.Settings {
	t.Helper()
	db := openDB(t, c)
	defer func() { require.NoError(t, db.Close()) }()
	s, err := store.NewSettingsRepo(db).Load(t.Context())
	require.NoError(t, err)
	return s
}

// storedCategories lists the Categories of the database of c by name.
func storedCategories(t *testing.T, c configDir) map[string]core.Category {
	t.Helper()
	db := openDB(t, c)
	defer func() { require.NoError(t, db.Close()) }()
	cats, err := store.NewCategoryRepo(db).List(t.Context())
	require.NoError(t, err)
	out := map[string]core.Category{}
	for _, c := range cats {
		out[c.Name] = c
	}
	return out
}

// addCategory stores a Category in the database of c and returns it.
func addCategory(t *testing.T, c configDir, name string) core.Category {
	t.Helper()
	db := openDB(t, c)
	cat := core.Category{ID: core.NewID(), Name: name}
	require.NoError(t, store.NewCategoryRepo(db).Create(t.Context(), cat))
	require.NoError(t, db.Close())
	return cat
}

// startAPI prepares a config dir, lets seed add state to it and starts the
// daemon. seed may be nil.
func startAPI(t *testing.T, seed func(c configDir)) *daemon {
	t.Helper()
	cfg := prepareConfigDir(t)
	if seed != nil {
		seed(cfg)
	}
	return startDaemon(t, cfg)
}

// wantDownload is the exact JSON of a Download that has no failure data.
func wantDownloadJSON(id, name, kind, state, prio string, pos int, paused bool, added string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"source_kind":%q,"category_id":null,"state":%q,"priority":%q,"queue_position":%d,"paused":%t,`+
		`"total_bytes":0,"done_bytes":0,"added_at":%q,"fetched_at":null,"ready_at":null,"updated_at":%q}`,
		id, name, kind, state, prio, pos, paused, added, added)
}

// ids returns the "id" of each object.
func idsOfItems(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it["id"].(string))
	}
	return out
}

// namesOfItems returns the "name" of each object.
func namesOfItems(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it["name"].(string))
	}
	return out
}

// parseTime parses an RFC 3339 timestamp of an answer.
func parseTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	require.True(t, ok, "timestamp must be a string: %v", v)
	ts, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err, s)
	return ts
}

// jsonOf encodes a decoded object again, for JSONEq.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func newCategoryRepo(db *store.DB) *store.CategoryRepo { return store.NewCategoryRepo(db) }

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
