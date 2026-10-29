package web_test

import (
	"golang.org/x/net/html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
)

func TestLoginPageRendersAFormThatWorksWithoutJavaScript(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().get("/login")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	doc := parseHTML(t, rec.Body.String())
	form := byID(doc, "login-form")
	require.NotNil(t, form)
	assert.Equal(t, "post", attr(form, "method"))
	assert.Equal(t, "/login", attr(form, "action"))
	assert.Equal(t, "/login", attr(form, "hx-post"), "progressively enhanced with htmx")
	assert.NotNil(t, byID(doc, "username"))
	assert.Equal(t, "password", attr(byID(doc, "password"), "type"))
}

func TestLoginSetsAHardenedSessionCookieAndRedirectsToTheQueue(t *testing.T) {
	e := newEnv(t)
	b := e.browser()

	rec := b.post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}})

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/queue", location(rec))
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			session = c
		}
	}
	require.NotNil(t, session, "session cookie")
	assert.True(t, session.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, session.SameSite)
	assert.Equal(t, "/", session.Path)
	assert.NotEmpty(t, session.Value)
	assert.Equal(t, http.StatusOK, b.get("/queue").Code, "the cookie opens the queue")
}

func TestLoginSecureCookieOverHTTPS(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}},
		func(r *http.Request) { r.TLS = &tlsState }, header("Origin", "https://example.com"))

	var secure bool
	for _, c := range rec.Result().Cookies() {
		secure = secure || (c.Name == auth.SessionCookieName && c.Secure)
	}
	assert.True(t, secure)
}

func TestLoginForHTMXRedirectsWithAHeader(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}}, htmx())

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "/queue", rec.Header().Get("HX-Redirect"))
	assert.Empty(t, location(rec))
}

func TestWrongPasswordShowsAGenericMessageAndKeepsTheUsername(t *testing.T) {
	e := newEnv(t)

	for _, user := range []string{"admin", "nobody"} {
		rec := e.browser().post("/login", url.Values{"username": {user}, "password": {"wrong password!"}})

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		doc := parseHTML(t, rec.Body.String())
		assert.Equal(t, "Invalid username or password.", text(byID(doc, "login-error")), user)
		assert.Equal(t, user, attr(byID(doc, "username"), "value"))
		assert.NotContains(t, rec.Body.String(), "wrong password!", "the password is never echoed")
		assert.Empty(t, rec.Result().Cookies())
	}
}

func TestLoginErrorForHTMXIsJustTheForm(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().post("/login", url.Values{"username": {"admin"}, "password": {"nope nope nope"}}, htmx())

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	body := rec.Body.String()
	assert.True(t, strings.HasPrefix(strings.TrimSpace(body), "<form"), body)
	assert.NotContains(t, body, "<html")
	assert.Contains(t, body, "Invalid username or password.")
}

func TestLoginRateLimitSaysHowLongToWait(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	for range 5 {
		rec := b.post("/login", url.Values{"username": {"admin"}, "password": {"wrong password!"}})
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	}

	rec := b.post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}})

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	doc := parseHTML(t, rec.Body.String())
	assert.Regexp(t, `^Too many failed attempts\. Try again in \d+ s\.$`, text(byID(doc, "login-error")))
	assert.Empty(t, rec.Result().Cookies(), "even the right password is held back")
}

func TestLoginRedirectsToTheNextPage(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}, "next": {"/history?x=1"}})

	assert.Equal(t, "/history?x=1", location(rec))
}

func TestLoginPageCarriesTheNextPageInAHiddenField(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().get("/login?next=%2Fsettings")

	next := find(parseHTML(t, rec.Body.String()), func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "next" })
	require.NotNil(t, next)
	assert.Equal(t, "/settings", attr(next, "value"))
}

func TestSignedInUserOnTheLoginPageGoesToTheQueue(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	rec := b.get("/login")

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/queue", location(rec))
}

func TestLoginWithoutTheSameOriginIsRefused(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}},
		header("Origin", "https://evil.test"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "reload the page")
	assert.Empty(t, rec.Result().Cookies())
}

func TestLogoutEndsTheSessionAndClearsTheCookie(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()
	old := b.cookies[auth.SessionCookieName].Value

	rec := b.post("/logout", url.Values{})

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/login", location(rec))
	assert.Nil(t, b.cookies[auth.SessionCookieName], "cookie cleared")
	stale := e.browser()
	stale.cookies[auth.SessionCookieName] = &http.Cookie{Name: auth.SessionCookieName, Value: old}
	assert.Equal(t, http.StatusSeeOther, stale.get("/queue").Code, "the old session is dead server-side")
}

func TestLogoutFromAnotherSiteIsRefused(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	for _, h := range []reqOpt{header("Origin", "https://evil.test"), header("Sec-Fetch-Site", "cross-site")} {
		rec := b.post("/logout", url.Values{}, h)

		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.NotNil(t, b.cookies[auth.SessionCookieName], "still signed in")
	}
}

func TestLogoutWithHTMXRedirectsTheBrowser(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	rec := b.post("/logout", url.Values{}, htmx(), header("Sec-Fetch-Site", "same-origin"))

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "/login", rec.Header().Get("HX-Redirect"))
}

func TestCSRFFailureForHTMXIsAToastNotAPage(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login()

	rec := b.post("/logout", url.Values{}, htmx(), header("Origin", "https://evil.test"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Header().Get("HX-Trigger"), `"toast"`)
	assert.Empty(t, rec.Body.String())
}

var _ = time.Second
