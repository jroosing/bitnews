package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/app"
)

// settingsSlugs are the six sections in navigation order.
var settingsSlugs = []string{"paths", "servers", "speed", "postprocessing", "torrent", "security"}

var settingsTitles = []string{"Paths & Categories", "Usenet servers", "Speed & queue", "Post-processing", "Torrent network", "Security"}

func settingsSignedIn(t *testing.T, opts ...envOption) (*env, *browser) {
	t.Helper()
	e := newEnv(t, opts...)
	b := e.browser()
	b.login()
	return e, b
}

// settingsPost sends an htmx form post carrying the CSRF header, like app.js does.
func (b *browser) settingsPost(target string, form url.Values, extra ...reqOpt) *httptest.ResponseRecorder {
	b.e.t.Helper()
	opts := append([]reqOpt{htmx(), header("X-CSRF-Token", b.csrf())}, extra...)
	return b.post(target, form, opts...)
}

// settingsDo sends a non-POST htmx request (GET, PATCH, DELETE) with the CSRF header.
func (b *browser) settingsDo(method, target string, form url.Values) *httptest.ResponseRecorder {
	b.e.t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://example.com")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-CSRF-Token", b.csrf())
	return b.send(r)
}

func settingsNav(t *testing.T, body string) *html.Node {
	t.Helper()
	nav := find(parseHTML(t, body), func(n *html.Node) bool {
		return n.Data == "nav" && attr(n, "aria-label") == "Settings sections"
	})
	require.NotNil(t, nav, "settings sub-navigation")
	return nav
}

func TestSettingsRedirectsToTheFirstSection(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.get("/settings")

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/settings/paths", location(rec))
}

func TestSettingsRequiresASession(t *testing.T) {
	e := newEnv(t)

	for _, p := range append([]string{"/settings"}, "/settings/paths", "/settings/servers", "/settings/speed", "/settings/torrent", "/settings/security") {
		rec := e.browser().get(p)

		assert.Equal(t, http.StatusSeeOther, rec.Code, p)
		assert.Equal(t, "/login?next="+url.QueryEscape(p), location(rec), p)
	}
}

func TestSettingsNavListsAllSectionsAndMarksTheCurrentOne(t *testing.T) {
	_, b := settingsSignedIn(t)

	for i, slug := range settingsSlugs {
		rec := b.get("/settings/" + slug)
		require.Equal(t, http.StatusOK, rec.Code, slug)

		nav := settingsNav(t, rec.Body.String())
		links := findAll(nav, func(n *html.Node) bool { return n.Data == "a" })
		require.Len(t, links, len(settingsSlugs), slug)
		var current []string
		for j, a := range links {
			assert.Equal(t, "/settings/"+settingsSlugs[j], attr(a, "href"))
			assert.Contains(t, text(a), settingsTitles[j])
			if attr(a, "aria-current") == "page" {
				current = append(current, settingsTitles[j])
			}
		}
		assert.Equal(t, []string{settingsTitles[i]}, current, slug)
	}
}

func TestMainNavKeepsSettingsActiveBelowSettings(t *testing.T) {
	_, b := settingsSignedIn(t)

	for _, slug := range settingsSlugs {
		doc := parseHTML(t, b.get("/settings/"+slug).Body.String())
		var current []string
		for _, nav := range findAll(doc, func(n *html.Node) bool { return n.Data == "nav" && strings.HasPrefix(attr(n, "aria-label"), "Main") }) {
			for _, a := range findAll(nav, func(n *html.Node) bool { return n.Data == "a" && attr(n, "aria-current") == "page" }) {
				current = append(current, text(a)+" "+attr(a, "href"))
			}
		}
		assert.Equal(t, []string{"Settings /settings", "Settings /settings"}, current, slug)
	}
}

func TestSectionPageHasOneH1WithTheSectionTitle(t *testing.T) {
	_, b := settingsSignedIn(t)

	for i, slug := range settingsSlugs {
		doc := parseHTML(t, b.get("/settings/"+slug).Body.String())
		h1 := findAll(doc, func(n *html.Node) bool { return n.Data == "h1" })
		require.Len(t, h1, 1, slug)
		assert.Equal(t, settingsTitles[i], text(h1[0]))
		title := find(doc, func(n *html.Node) bool { return n.Data == "title" })
		assert.Contains(t, text(title), settingsTitles[i])
	}
}

func TestSettingsSubNavigationIsAScrollableBarOnPhonesAndASidebarFromMd(t *testing.T) {
	_, b := settingsSignedIn(t)

	nav := settingsNav(t, b.get("/settings/paths").Body.String())

	classes := attr(nav, "class")
	assert.Contains(t, classes, "overflow-x-auto")
	assert.Contains(t, classes, "md:")
}

// addSettingsResponses renders every page and fragment of the settings
// section into out, so the HTML invariant tests (no inline script or style,
// accessible names, unique ids, security headers) cover them too.
func addSettingsResponses(t *testing.T, out map[string]*httptest.ResponseRecorder) {
	t.Helper()
	prober := &recordingProber{result: app.ProbeResult{Class: app.ProbeTLS, Detail: "certificate_untrusted", Message: "x"}}
	e, b := settingsSignedIn(t, withProber(prober))
	ctx := context.Background()
	cat, err := e.cats.Create(ctx, "tv", "/data/<media>/tv")
	require.NoError(t, err)
	_, err = e.cats.Create(ctx, "movies", "")
	require.NoError(t, err)
	srv := mustCreateServerAt(t, e, "alpha", 0)
	mustCreateServerAt(t, e, "bravo", 1)
	broken := mustCreateServerAt(t, e, "charlie", 2)
	_ = broken

	for _, slug := range settingsSlugs {
		out["GET /settings/"+slug+" (with data)"] = b.get("/settings/" + slug)
	}
	out["POST /settings/paths (saved)"] = b.settingsPost("/settings/paths", pathsForm(t))
	bad := pathsForm(t)
	bad.Set("usenet_complete", "relative")
	out["POST /settings/paths (invalid)"] = b.settingsPost("/settings/paths", bad)
	good := pathsForm(t)
	good.Set("_csrf", b.csrf())
	out["POST /settings/paths (plain form)"] = b.post("/settings/paths", good)
	out["POST /settings/categories (invalid)"] = b.settingsPost("/settings/categories", url.Values{"name": {"BAD"}})
	out["GET category edit row"] = b.settingsDo("GET", "/settings/categories/"+string(cat.ID)+"/edit", nil)
	out["POST category update (invalid)"] = b.settingsPost("/settings/categories/"+string(cat.ID), url.Values{"dir_override": {"rel"}})
	out["GET server form (new)"] = b.settingsDo("GET", "/settings/servers/new", nil)
	out["GET server form (edit)"] = b.settingsDo("GET", "/settings/servers/"+string(srv.ID)+"/edit", nil)
	invalid := serverForm("delta")
	invalid.Set("host", "bad host")
	out["POST server (invalid)"] = b.settingsPost("/settings/servers", invalid)
	out["POST server test (failed)"] = b.settingsPost("/settings/servers/test", serverForm("alpha"))
	out["POST server row test"] = b.settingsPost("/settings/servers/"+string(srv.ID)+"/test", nil)
	prober.mu.Lock()
	prober.result = okProbe()
	prober.mu.Unlock()
	out["POST server test (ok)"] = b.settingsPost("/settings/servers/test", serverForm("alpha"))
	e.breakServerKey()
	out["GET /settings/servers (password unavailable)"] = b.get("/settings/servers")
	out["GET server form (re-enter)"] = b.settingsDo("GET", "/settings/servers/"+string(srv.ID)+"/edit?reenter=1", nil)
	out["POST server row test (password unavailable)"] = b.settingsPost("/settings/servers/"+string(srv.ID)+"/test", nil)
	addSecurityResponses(t, e, b, out)
	addTransferResponses(t, out)
	addPostProcessingResponses(t, out)
}
