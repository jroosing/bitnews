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

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// appPages are the pages behind the sign-in.
var appPages = []string{"/queue", "/history", "/settings/paths", "/ui/styleguide"}

func TestPagesRequireASession(t *testing.T) {
	e := newEnv(t)

	for _, page := range appPages {
		rec := e.browser().get(page)

		assert.Equal(t, http.StatusSeeOther, rec.Code, page)
		assert.Equal(t, "/login?next="+url.QueryEscape(page), location(rec), page)
	}
}

func TestUnauthenticatedHTMXRequestsGetALoginRedirectHeader(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().get("/queue", htmx())

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "/login", rec.Header().Get("HX-Redirect"))
}

func TestRedirectToLoginKeepsOnlyKnownPagesAsNext(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().get("/queue?filter=x")
	assert.Equal(t, "/login?next="+url.QueryEscape("/queue?filter=x"), location(rec))

	for _, target := range []string{"/", "/unknown"} {
		rec := e.browser().get(target)
		assert.NotContains(t, location(rec), "next=", target)
	}
}

func TestSignedInUserSeesEveryPage(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	for _, page := range appPages {
		rec := b.get(page)

		assert.Equal(t, http.StatusOK, rec.Code, page)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/html", page)
		assert.NotContains(t, rec.Body.String(), "no value", page)
	}
}

func TestLayoutHasLandmarksSkipLinkNavAndTheMetaTags(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	rec := b.get("/history")

	doc := parseHTML(t, rec.Body.String())
	assert.Equal(t, "en", attr(find(doc, func(n *html.Node) bool { return n.Data == "html" }), "lang"))
	skip := find(doc, func(n *html.Node) bool { return n.Data == "a" && attr(n, "href") == "#main" })
	require.NotNil(t, skip, "skip link")
	assert.Contains(t, attr(skip, "class"), "skip-link")
	assert.NotNil(t, byID(doc, "main"))
	for _, tag := range []string{"main", "header", "nav", "aside"} {
		assert.NotNil(t, find(doc, func(n *html.Node) bool { return n.Data == tag }), "<%s> landmark", tag)
	}
	assert.Len(t, findAll(doc, func(n *html.Node) bool { return n.Data == "h1" }), 1, "one h1")
	assert.Equal(t, "History", text(find(doc, func(n *html.Node) bool { return n.Data == "h1" })))

	csrf := find(doc, func(n *html.Node) bool { return n.Data == "meta" && attr(n, "name") == "csrf-token" })
	require.NotNil(t, csrf)
	assert.Len(t, attr(csrf, "content"), 64)
	cfg := find(doc, func(n *html.Node) bool { return n.Data == "meta" && attr(n, "name") == "htmx-config" })
	require.NotNil(t, cfg)
	for _, want := range []string{`"allowEval":false`, `"allowScriptTags":false`, `"selfRequestsOnly":true`, `"includeIndicatorStyles":false`} {
		assert.Contains(t, attr(cfg, "content"), want)
	}
}

func TestNavMarksTheCurrentPageAndHidesTheStyleGuide(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	for _, tc := range []struct{ page, current string }{{"/queue", "Queue"}, {"/history", "History"}, {"/settings/paths", "Settings"}} {
		doc := parseHTML(t, b.get(tc.page).Body.String())
		var labels, current []string
		for _, nav := range findAll(doc, func(n *html.Node) bool { return n.Data == "nav" && strings.HasPrefix(attr(n, "aria-label"), "Main") }) {
			for _, a := range findAll(nav, func(n *html.Node) bool { return n.Data == "a" }) {
				labels = append(labels, text(a))
				if attr(a, "aria-current") == "page" {
					current = append(current, text(a))
				}
			}
		}
		assert.Equal(t, []string{"Queue", "History", "Settings", "Queue", "History", "Settings"}, labels, "desktop and mobile nav")
		assert.Equal(t, []string{tc.current, tc.current}, current, tc.page)
	}
}

func TestUserMenuShowsTheUserAndASignOutForm(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	doc := parseHTML(t, b.get("/queue").Body.String())

	form := find(doc, func(n *html.Node) bool { return n.Data == "form" && attr(n, "action") == "/logout" })
	require.NotNil(t, form)
	assert.Equal(t, "post", attr(form, "method"))
	hidden := find(form, func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "_csrf" })
	require.NotNil(t, hidden)
	assert.Equal(t, b.csrf(), attr(hidden, "value"))
	assert.Contains(t, text(doc), "admin")
}

func TestBannerAppearsOnlyWhenAuthIsBypassedOrDisabled(t *testing.T) {
	banner := func(rec *httptest.ResponseRecorder) string {
		n := find(parseHTML(t, rec.Body.String()), func(n *html.Node) bool { return hasAttr(n, "data-auth-banner") })
		if n == nil {
			return ""
		}
		return attr(n, "data-auth-banner") + ": " + text(n)
	}

	t.Run("session", func(t *testing.T) {
		e := newEnv(t)
		b := e.browser()
		b.login()
		for _, page := range appPages {
			assert.Empty(t, banner(b.get(page)), page)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		e := newEnv(t)
		s := e.settings.Current()
		s.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"example.com"}}
		require.NoError(t, e.settings.Update(context.Background(), s))
		for _, page := range appPages {
			rec := e.browser().get(page)
			require.Equal(t, http.StatusOK, rec.Code, page)
			got := banner(rec)
			assert.Contains(t, got, "disabled: Authentication is turned off", page)
			assert.Contains(t, got, "Anyone who can reach this address")
		}
	})
	t.Run("bypass", func(t *testing.T) {
		e := newEnv(t)
		s := e.settings.Current()
		s.Auth = core.Auth{LANBypassCIDRs: []string{"192.0.2.0/24"}, AllowedHosts: []string{"example.com"}}
		require.NoError(t, e.settings.Update(context.Background(), s))
		rec := e.browser().get("/queue", func(r *http.Request) { r.RemoteAddr = "192.0.2.7:4000" })
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, banner(rec), "bypass: LAN bypass is on")
	})
	t.Run("cannot be dismissed", func(t *testing.T) {
		e := newEnv(t)
		s := e.settings.Current()
		s.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"example.com"}}
		require.NoError(t, e.settings.Update(context.Background(), s))
		n := find(parseHTML(t, e.browser().get("/queue").Body.String()), func(n *html.Node) bool { return hasAttr(n, "data-auth-banner") })
		require.NotNil(t, n)
		assert.Empty(t, findAll(n, func(n *html.Node) bool { return n.Data == "button" }), "no close button")
	})
}

func TestBypassPrincipalsGetPagesButNoSignOut(t *testing.T) {
	e := newEnv(t)
	s := e.settings.Current()
	s.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"example.com"}}
	require.NoError(t, e.settings.Update(context.Background(), s))
	b := e.browser()

	doc := parseHTML(t, b.get("/queue").Body.String())

	assert.Nil(t, find(doc, func(n *html.Node) bool { return n.Data == "form" && attr(n, "action") == "/logout" }))
	csrf := find(doc, func(n *html.Node) bool { return n.Data == "meta" && attr(n, "name") == "csrf-token" })
	require.NotNil(t, csrf, "CSRF still applies")
	assert.Equal(t, "/queue", location(b.get("/login")), "no login page when nobody needs to log in")
}

func TestDisallowedHostGetsAnHTMLErrorPage(t *testing.T) {
	e := newEnv(t)
	s := e.settings.Current()
	s.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}
	require.NoError(t, e.settings.Update(context.Background(), s))

	rec := e.browser().get("/queue") // Host is example.com

	assert.Equal(t, http.StatusMisdirectedRequest, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, rec.Body.String(), "Address not allowed")
}

func TestUnknownPagesAre404AndWrongMethodsAre405(t *testing.T) {
	e := newEnv(t)
	b := e.browser()

	rec := b.get("/nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Page not found")
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")

	rec = b.post("/queue", url.Values{})
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "GET, HEAD", rec.Header().Get("Allow"))
}

func TestAPIPathsKeepTheirJSONErrors(t *testing.T) {
	e := newEnv(t)

	for _, target := range []string{"/api/v1/nope", "/api/nope", "/api", "/sabnzbd/nope", "/api/v2/app/version"} {
		rec := e.browser().get(target)

		assert.Equal(t, http.StatusNotFound, rec.Code, target)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"), target)
		assert.Equal(t, "default-src 'self'; frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"), "API CSP unchanged: %s", target)
	}
}

func TestStyleGuideRendersEveryComponent(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	body := b.get("/ui/styleguide").Body.String()

	doc := parseHTML(t, body)
	root := find(doc, func(n *html.Node) bool { return hasAttr(n, "data-styleguide") })
	require.NotNil(t, root)
	classes := map[string]bool{}
	walk(root, func(n *html.Node) {
		if n.Type == html.ElementNode {
			for c := range strings.FieldsSeq(attr(n, "class")) {
				classes[c] = true
			}
		}
	})
	for _, c := range []string{
		"btn-primary", "btn-secondary", "btn-danger", "btn-ghost", "btn-spinner", "btn-sm", "btn-lg", "btn-icon",
		"input", "select", "checkbox", "switch", "field-error", "label", "help",
		"badge-queued", "badge-downloading", "badge-processing", "badge-ready", "badge-failed", "badge-paused",
		"alert-info", "alert-success", "alert-warn", "alert-danger",
		"card", "card-header", "table-shell", "table-responsive", "progress", "progress-ok", "progress-warn", "progress-bad", "modal", "tab",
	} {
		assert.True(t, classes[c], "style guide shows .%s", c)
	}
	assert.NotNil(t, find(root, func(n *html.Node) bool { return n.Data == "dialog" }), "modal")
	assert.NotNil(t, find(root, func(n *html.Node) bool { return n.Data == "progress" }), "native progress")
	assert.NotNil(t, find(root, func(n *html.Node) bool { return n.Data == "nav" && attr(n, "aria-label") == "Pagination" }), "pagination")
	assert.NotNil(t, find(root, func(n *html.Node) bool { return attr(n, "role") == "switch" }), "switch")
	assert.NotNil(t, find(root, func(n *html.Node) bool { return attr(n, "x-data") == "tabs" }), "tabs")
	assert.Contains(t, body, "Empty states explain")
}
