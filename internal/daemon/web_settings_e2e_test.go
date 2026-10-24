//go:build e2e

package daemon_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
	"github.com/jroosing/bitnews/internal/usenet/runner"
)

// fakeServerProber tests a Server by connecting to the in-memory NNTP server
// of the same name, the way the real prober connects to a provider.
type fakeServerProber struct{ e *e2e }

func (p fakeServerProber) Probe(ctx context.Context, s core.Server) app.ProbeResult {
	conn, err := p.e.factory(runner.ServerSpec{Name: s.Name, Pass: s.Password.Reveal()})(ctx)
	if err != nil {
		return app.ProbeResult{Class: app.ProbeAuth, Detail: "rejected", Message: "the server rejected the user name or password (481)"}
	}
	_ = conn.Close()
	return app.ProbeResult{OK: true, Class: app.ProbeOK}
}

// hx sends a form like htmx does: the CSRF header and HX-Request.
func (w *webClient) hx(method, path string, form url.Values) (*http.Response, string) {
	w.t.Helper()
	req, err := http.NewRequest(method, w.base+path, strings.NewReader(form.Encode()))
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", w.base)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", w.csrf())
	resp, err := w.c.Do(req)
	require.NoError(w.t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(w.t, err)
	_ = resp.Body.Close()
	return resp, string(b)
}

// The settings pages over real HTTP, wired to the real services: a category
// and a server created in the UI are used by the next Download, and new
// folders apply to new Downloads only.
func TestE2EWebSettingsCategoryServerAndPathsAffectDownloads(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.prober = fakeServerProber{e}
	e.start()
	w := newWebClient(t, e.ts.URL)
	resp, body := w.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"correct horse battery staple"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)

	// The pages render for a signed-in user.
	for _, slug := range []string{"paths", "servers", "speed", "torrent", "security"} {
		resp, body = w.do("GET", "/settings/"+slug, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, slug)
		assert.Contains(t, body, `aria-label="Settings sections"`)
	}
	resp, _ = w.do("GET", "/settings", nil)
	assert.Equal(t, "/settings/paths", resp.Header.Get("Location"))

	// A category created in the UI shows up in the API.
	resp, body = w.hx("POST", "/settings/categories", url.Values{"name": {"tv"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var cats struct {
		Items []struct {
			ID   core.ID `json:"id"`
			Name string  `json:"name"`
		} `json:"items"`
	}
	e.call("GET", "/api/v1/categories", nil, http.StatusOK, &cats)
	require.Len(t, cats.Items, 1)
	assert.Equal(t, "tv", cats.Items[0].Name)
	tv := cats.Items[0].ID

	// A server added in the UI, after its connection test.
	fake := &fakeNNTP{Server: nntptest.NewServer("main"), pass: "secret"}
	e.mu.Lock()
	e.servers["main"] = fake
	e.mu.Unlock()
	form := url.Values{
		"name": {"main"}, "host": {"main.example"}, "port": {"563"}, "username": {"u"}, "password": {"wrong"},
		"max_connections": {"4"}, "retention_days": {"0"}, "priority": {"0"}, "enabled": {"on"},
	}
	resp, body = w.hx("POST", "/settings/servers/test", form)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "Login rejected")
	form.Set("password", "secret")
	resp, body = w.hx("POST", "/settings/servers/test", form)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "Connection works")
	resp, body = w.hx("POST", "/settings/servers", form)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, resp.Header.Get("HX-Trigger"), "dialog-close")
	assert.NotContains(t, body, "secret", "the password never comes back")
	var servers struct {
		Items []map[string]any `json:"items"`
	}
	e.call("GET", "/api/v1/servers", nil, http.StatusOK, &servers)
	require.Len(t, servers.Items, 1)
	assert.Equal(t, "main", servers.Items[0]["name"])
	_, hasPassword := servers.Items[0]["password"]
	assert.False(t, hasPassword)

	// A Download uses the category and the server created in the UI.
	g := loadGolden(t, "explicit_uniform")
	files := g.files()
	fx := nntptest.BuildFixture(t, files, 8192)
	fx.Install(fake.Server)
	first := e.addNZB("First.Show.S01E01", fx.NZB, tv)
	got := e.waitState(first.ID, core.StateReady)
	assert.Equal(t, filepath.Join(e.complete, "tv", "First.Show.S01E01"), got.OutputPath)

	// New folders, saved in the UI, apply to the next Download only.
	newIncomplete := filepath.Join(e.root, "moved", "usenet", "incomplete")
	newComplete := filepath.Join(e.root, "moved", "usenet", "complete")
	resp, body = w.hx("POST", "/settings/paths", url.Values{
		"usenet_incomplete": {newIncomplete}, "usenet_complete": {newComplete},
		"torrent_incomplete": {e.tincomplete}, "torrent_complete": {e.tcomplete},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var settings core.Settings
	e.call("GET", "/api/v1/settings", nil, http.StatusOK, &settings)
	assert.Equal(t, newComplete, settings.Paths.Usenet.Complete)

	files2 := g.files()
	files2["second.nfo"] = payload(2000, 3)
	fx2 := nntptest.BuildFixture(t, files2, 8192)
	fx2.Install(fake.Server)
	second := e.addNZB("Second.Show.S01E02", fx2.NZB, tv)
	got2 := e.waitState(second.ID, core.StateReady)
	assert.Equal(t, filepath.Join(newComplete, "tv", "Second.Show.S01E02"), got2.OutputPath, "the new folder")
	assert.Equal(t, filepath.Join(e.complete, "tv", "First.Show.S01E01"), e.get(first.ID).OutputPath, "the old Download keeps its folder")

	// Deleting the category in the UI clears it from the Download but keeps the files.
	resp, body = w.hx("DELETE", "/settings/categories/"+string(tv), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.DirExists(t, filepath.Join(e.complete, "tv", "First.Show.S01E01"))
	assert.Nil(t, e.get(first.ID).CategoryID)

	// The rest of the settings is untouched by the UI.
	assert.False(t, settings.Auth.DisableAuth)
	assert.Empty(t, settings.Auth.AllowedHosts)
	assert.Equal(t, core.DefaultSettings().Queues, settings.Queues)
}
