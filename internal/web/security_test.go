package web_test

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/a-h/templ"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/web"
)

const wantCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func TestContentSecurityPolicyConstantIsTheDesignsPolicy(t *testing.T) {
	assert.Equal(t, wantCSP, web.ContentSecurityPolicy)
}

// htmlResponses renders every kind of HTML the UI produces: each page
// signed in and signed out, the setup page, error pages and the fragments
// htmx swaps.
func htmlResponses(t *testing.T) map[string]*httptest.ResponseRecorder {
	t.Helper()
	htmlOnce.Do(func() { htmlCache = renderEverything(t) })
	return htmlCache
}

var (
	htmlOnce  sync.Once
	htmlCache map[string]*httptest.ResponseRecorder
)

func renderEverything(t *testing.T) map[string]*httptest.ResponseRecorder {
	out := map[string]*httptest.ResponseRecorder{}

	e := newEnv(t)
	anon := e.browser()
	out["GET /login"] = anon.get("/login")
	out["POST /login wrong password"] = anon.post("/login", url.Values{"username": {"admin"}, "password": {"wrong wrong wrong"}})
	out["POST /login wrong password (htmx)"] = anon.post("/login", url.Values{"username": {"admin"}, "password": {"wrong wrong wrong"}}, htmx())
	out["404"] = anon.get("/nope")
	out["405"] = anon.post("/queue", url.Values{})
	out["403 csrf"] = anon.post("/login", url.Values{}, header("Origin", "https://evil.test"))
	b := e.browser()
	b.login()
	for _, page := range appPages {
		out["GET "+page] = b.get(page)
	}
	out["403 csrf (signed in)"] = b.post("/logout", url.Values{})
	addSettingsResponses(t, out)
	addQueueResponses(t, out)

	maps.Copy(out, downloadHTMLResponses(t))

	setup := newEnv(t, withoutAdmin())
	out["GET /setup"] = setup.browser().get("/setup")
	out["POST /setup invalid"] = setup.browser().post("/setup", setupForm("bad", "x", "short", "other"))
	out["POST /setup invalid (htmx)"] = setup.browser().post("/setup", setupForm("bad", "x", "short", "other"), htmx())
	return out
}

func TestEveryHTMLResponseCarriesTheStrictSecurityHeaders(t *testing.T) {
	for name, rec := range htmlResponses(t) {
		h := rec.Header()
		assert.Equal(t, wantCSP, h.Get("Content-Security-Policy"), name)
		assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"), name)
		assert.Equal(t, "same-origin", h.Get("Referrer-Policy"), name)
		assert.Equal(t, "no-store", h.Get("Cache-Control"), name)
		assert.Equal(t, "DENY", h.Get("X-Frame-Options"), name)
		assert.Contains(t, h.Get("Content-Type"), "text/html", name)
		assert.NotEmpty(t, h.Get("X-Request-ID"), name)
	}
}

func TestStaticAssetsKeepTheCSPToo(t *testing.T) {
	e := newEnv(t)
	rec := e.browser().get("/static/" + stylesheetName(t, e))

	assert.Equal(t, wantCSP, rec.Header().Get("Content-Security-Policy"))
}

// The CSP forbids inline script, inline style, event handlers and eval, so no
// page may contain any of them. This renders every page and checks.
func TestNoPageContainsInlineScriptStyleOrHandlers(t *testing.T) {
	for name, rec := range htmlResponses(t) {
		body := rec.Body.String()
		if body == "" || rec.Code == http.StatusNoContent {
			continue
		}
		doc := parseHTML(t, body)
		walk(doc, func(n *html.Node) {
			if n.Type != html.ElementNode {
				return
			}
			switch n.Data {
			case "script":
				assert.True(t, hasAttr(n, "src"), "%s: inline <script>", name)
				assert.True(t, strings.HasPrefix(attr(n, "src"), "/static/"), "%s: script from %q", name, attr(n, "src"))
				assert.Empty(t, strings.TrimSpace(text(n)), "%s: script with a body", name)
			case "style":
				t.Errorf("%s: <style> element", name)
			case "iframe", "object", "embed", "frame":
				t.Errorf("%s: <%s> element", name, n.Data)
			case "base":
				t.Errorf("%s: <base> element", name)
			}
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				assert.False(t, key == "style", "%s: style attribute on <%s>", name, n.Data)
				assert.False(t, strings.HasPrefix(key, "on"), "%s: handler %s on <%s>", name, key, n.Data)
				assert.False(t, strings.HasPrefix(key, "hx-on"), "%s: %s on <%s>", name, key, n.Data)
				assert.False(t, strings.HasPrefix(key, "x-on") && !strings.HasPrefix(key, "x-on:"), "%s: %s", name, key)
				switch key {
				case "href", "src", "action", "formaction", "hx-get", "hx-post", "hx-put", "hx-delete", "hx-patch", "poster":
					v := strings.ToLower(strings.TrimSpace(a.Val))
					assert.False(t, strings.HasPrefix(v, "javascript:") || strings.HasPrefix(v, "data:") || strings.HasPrefix(v, "vbscript:"), "%s: %s=%q", name, key, a.Val)
					assert.False(t, strings.Contains(v, "://") || strings.HasPrefix(v, "//"), "%s: %s=%q leaves the origin", name, key, a.Val)
				}
			}
		})
		assert.NotContains(t, body, "<no value>", name)
		assert.NotContains(t, body, "ZgotmplZ", name)
	}
}

func TestPagesLoadScriptsInTheOrderAppJSBeforeAlpine(t *testing.T) {
	e := newEnv(t)
	doc := parseHTML(t, e.browser().get("/login").Body.String())

	var order []string
	for _, s := range findAll(doc, func(n *html.Node) bool { return n.Data == "script" }) {
		src := attr(s, "src")
		switch {
		case strings.Contains(src, "/js/theme."):
			order = append(order, "theme")
			assert.False(t, hasAttr(s, "defer"), "theme.js runs before first paint")
		case strings.Contains(src, "/js/app."):
			order = append(order, "app")
		case strings.Contains(src, "htmx-ext-sse"):
			order = append(order, "sse")
		case strings.Contains(src, "/htmx-"):
			order = append(order, "htmx")
		case strings.Contains(src, "alpine-csp"):
			order = append(order, "alpine")
		}
		if !strings.Contains(src, "/js/theme.") {
			assert.True(t, hasAttr(s, "defer"), src)
		}
	}
	assert.Equal(t, []string{"theme", "app", "htmx", "sse", "alpine"}, order)
}

func TestAnonymousPagesLeakNoCSRFTokenOrAccountMenu(t *testing.T) {
	e := newEnv(t)

	for _, target := range []string{"/login", "/nope"} {
		body := e.browser().get(target).Body.String()

		assert.NotContains(t, body, `name="csrf-token"`, target)
		assert.NotContains(t, body, "/logout", target)
	}
}

// ---- escaping ---------------------------------------------------------

var hostile = []string{
	`<script>alert(1)</script>`,
	`"><img src=x onerror=alert(1)>`,
	`' onmouseover='alert(1)`,
	`javascript:alert(1)`,
	`</textarea></title><svg onload=alert(1)>`,
	"\u202Eevil\u202C",
	`{{.}}`,
	`&lt;b&gt;`,
	`background:url(javascript:alert(1))`,
}

// hostileLogins is the subset used where a test pays for argon2 per value.
var hostileLogins = []string{hostile[0], hostile[1], hostile[2], hostile[5]}

func TestHostileUsernameIsInertEverywhereItIsShown(t *testing.T) {
	for _, name := range hostileLogins {
		e := newEnv(t, withoutAdmin())
		b := e.browser()
		rec := b.post("/setup", setupForm(e.token, name, "a long enough password", "a long enough password"))
		if rec.Code != http.StatusSeeOther {
			continue // refused as a username (e.g. control characters)
		}
		for _, page := range appPages {
			rec := b.get(page)

			require.Equal(t, http.StatusOK, rec.Code, page)
			assertInert(t, rec.Body.String(), page+" as "+name)
			assert.Contains(t, text(parseHTML(t, rec.Body.String())), name, "shown as text: %s", page)
		}
	}
}

func TestHostileFormValuesComeBackEscaped(t *testing.T) {
	e := newEnv(t)
	setup := newEnv(t, withoutAdmin())
	for i, v := range hostile {
		// A fresh client address per value: the login limiter keys on it.
		fromHost := func(r *http.Request) { r.RemoteAddr = fmt.Sprintf("198.51.100.%d:4000", i+1) }
		rec := e.browser().post("/login", url.Values{"username": {v + "x"}, "password": {"wrong wrong wrong"}, "next": {v}}, fromHost)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		doc := parseHTML(t, rec.Body.String())
		assert.Equal(t, v+"x", attr(byID(doc, "username"), "value"), "round-trips as an attribute value, not markup")
		assertInert(t, rec.Body.String(), "login "+v)

		rec = setup.browser().post("/setup", setupForm(v, v, "short", "short"))
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		assertInert(t, rec.Body.String(), "setup "+v)
	}
}

func TestHostileNextParameterNeverReachesTheMarkup(t *testing.T) {
	e := newEnv(t)
	for _, v := range hostile {
		rec := e.browser().get("/login?next=" + url.QueryEscape(v))

		assertInert(t, rec.Body.String(), "next="+v)
		assert.NotContains(t, rec.Body.String(), `name="next"`, "rejected values are dropped")
	}
}

// assertInert fails when the markup contains an element or attribute that an
// attacker's text could only create by breaking out of its context.
func assertInert(t *testing.T, body, ctx string) {
	t.Helper()
	doc := parseHTML(t, body)
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		assert.NotEqual(t, "img", n.Data, "%s: injected <img>", ctx)
		for _, a := range n.Attr {
			assert.False(t, strings.HasPrefix(a.Key, "on"), "%s: injected handler %s", ctx, a.Key)
		}
	})
	scripts := 0
	for _, s := range findAll(doc, func(n *html.Node) bool { return n.Data == "script" }) {
		if !hasAttr(s, "src") {
			scripts++
		}
	}
	assert.Zero(t, scripts, "%s: injected inline script", ctx)
}

// ---- components with hostile data ---------------------------------------

func renderComponent(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf strings.Builder
	require.NoError(t, c.Render(context.Background(), &buf))
	return buf.String()
}

func TestComponentsEscapeHostileData(t *testing.T) {
	for _, v := range hostile {
		for name, c := range map[string]templ.Component{
			"badge":       web.Badge(web.BadgeReady, v),
			"field":       web.Field(web.FieldProps{ID: "f", Label: v, Value: v, Help: v, Error: v, Placeholder: v}),
			"alert":       web.Alert(web.AlertProps{Variant: web.AlertDanger, Title: v, Body: v}),
			"empty_state": web.EmptyState(web.EmptyProps{Title: v, Body: v, ActionLabel: v, ActionHref: v}),
			"pagination":  web.Pagination(web.PaginationProps{Summary: v, NextHref: v, PrevHref: v}),
			"progress":    web.Progress(web.ProgressProps{Value: 5, Label: v}),
			"copy_button": web.CopyButton(v),
			"button":      web.Button(web.ButtonProps{Label: v}),
			"card":        web.Card(web.CardProps{Title: v, Subtitle: v, Class: v}),
			"table":       web.TableShell(v, v),
			"modal":       web.Modal(web.ModalProps{ID: "m", Title: v, Description: v}),
		} {
			out := renderComponent(t, c)

			assertInert(t, "<div>"+out+"</div></dialog></table></section>", name+" "+v)
			assert.NotContains(t, out, "<script", name)
			walk(parseHTML(t, out), func(n *html.Node) {
				for _, a := range n.Attr {
					if n.Type == html.ElementNode && (a.Key == "href" || a.Key == "src" || a.Key == "action") {
						assert.NotContains(t, strings.ToLower(a.Val), "javascript:", "%s: %s attribute must be neutralised", name, a.Key)
					}
				}
			})
		}
	}
}

func TestSelectOptionsAreEscaped(t *testing.T) {
	for _, v := range hostile {
		out := renderComponent(t, web.SelectField(web.SelectProps{
			ID: "s", Label: "Priority", Selected: v,
			Options: []web.SelectOption{{Value: v, Label: v}, {Value: "b", Label: "B"}},
		}))

		assertInert(t, "<div>"+out+"</div>", "select "+v)
	}
}

func TestUnknownIconFailsTheRender(t *testing.T) {
	var buf strings.Builder
	assert.Error(t, web.Icon(web.IconName("no-such-icon"), "size-5").Render(context.Background(), &buf))
}

var _ = context.Background
var _ = core.ID("")
