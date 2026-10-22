package web_test

import (
	"golang.org/x/net/html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
)

func setupForm(token, user, password, confirm string) url.Values {
	return url.Values{"token": {token}, "username": {user}, "password": {password}, "confirm": {confirm}}
}

func TestWhileSetupIsOpenTheFrontDoorsGoToSetup(t *testing.T) {
	e := newEnv(t, withoutAdmin())
	b := e.browser()

	for _, target := range []string{"/", "/login"} {
		rec := b.get(target)

		assert.Equal(t, http.StatusSeeOther, rec.Code, target)
		assert.Equal(t, "/setup", location(rec), target)
	}
}

func TestWhileSetupIsOpenOtherPagesAreGated(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	for _, target := range []string{"/queue", "/history", "/settings", "/ui/styleguide"} {
		assert.Equal(t, http.StatusServiceUnavailable, e.browser().get(target).Code, target)
	}
	assert.Equal(t, http.StatusOK, e.browser().get("/static/"+stylesheetName(t, e)).Code, "assets stay available")
}

func TestSetupPageExplainsTheTokenAndTheRequirements(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	rec := e.browser().get("/setup")

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	for _, id := range []string{"token", "username", "password", "confirm"} {
		assert.NotNil(t, byID(doc, id), id)
	}
	assert.Equal(t, "12", attr(byID(doc, "password"), "minlength"))
	assert.Contains(t, text(byID(doc, "password-help")), "At least 12 characters")
	assert.Equal(t, "/setup", attr(byID(doc, "setup-form"), "hx-post"))
}

func TestSetupCreatesTheAdminSignsInAndClosesSetup(t *testing.T) {
	e := newEnv(t, withoutAdmin())
	b := e.browser()

	rec := b.post("/setup", setupForm(e.token, "owner", "a long enough password", "a long enough password"))

	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	assert.Equal(t, "/queue", location(rec))
	assert.NotNil(t, b.cookies[auth.SessionCookieName], "signed in right away")
	assert.Equal(t, http.StatusOK, b.get("/queue").Code)
	assert.False(t, e.setup.Required())
	assert.Equal(t, "/login", location(e.browser().get("/setup")), "setup is over")
	again := e.browser().post("/setup", setupForm(e.token, "other", "another long password", "another long password"))
	assert.Equal(t, "/login", location(again))
}

func TestSetupForHTMXRedirectsWithAHeader(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	rec := e.browser().post("/setup", setupForm(e.token, "owner", "a long enough password", "a long enough password"), htmx())

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "/queue", rec.Header().Get("HX-Redirect"))
}

func TestSetupRejectsAWrongTokenOnTheTokenField(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	rec := e.browser().post("/setup", setupForm("NOTTHETOKEN", "owner", "a long enough password", "a long enough password"))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.Contains(t, text(byID(doc, "token-error")), "setup token is not right")
	assert.Equal(t, "owner", attr(byID(doc, "username"), "value"))
	assert.True(t, e.setup.Required())
	assert.NotContains(t, rec.Body.String(), "a long enough password")
}

func TestSetupEnforcesThePasswordRequirements(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	cases := []struct {
		name, password, confirm, field, message string
	}{
		{"short", "short pw", "short pw", "password-error", "too short"},
		{"eleven", "elevenchars", "elevenchars", "password-error", "at least 12"},
		{"mismatch", "a long enough password", "a long enough passwore", "confirm-error", "do not match"},
		{"too long", strings.Repeat("x", 1025), strings.Repeat("x", 1025), "password-error", "too long"},
	}
	for _, tc := range cases {
		rec := e.browser().post("/setup", setupForm(e.token, "owner", tc.password, tc.confirm))

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, tc.name)
		doc := parseHTML(t, rec.Body.String())
		n := byID(doc, tc.field)
		require.NotNil(t, n, tc.name)
		assert.Contains(t, text(n), tc.message, tc.name)
		assert.True(t, e.setup.Required(), tc.name)
	}
}

func TestSetupRejectsBadUsernamesAndMissingToken(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	rec := e.browser().post("/setup", setupForm(e.token, " padded ", "a long enough password", "a long enough password"))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "username-error"))

	rec = e.browser().post("/setup", setupForm("", "owner", "a long enough password", "a long enough password"))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "token-error"))
	assert.True(t, e.setup.Required())
}

func TestSetupErrorForHTMXIsJustTheForm(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	rec := e.browser().post("/setup", setupForm(e.token, "owner", "short", "short"), htmx())

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.True(t, strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "<form"))
}

func TestSetupNeedsTheSameOrigin(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	rec := e.browser().post("/setup", setupForm(e.token, "owner", "a long enough password", "a long enough password"),
		header("Origin", "https://evil.test"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.True(t, e.setup.Required())
}

func TestFrontDoorsOnceSetupIsDone(t *testing.T) {
	e := newEnv(t)
	b := e.browser()

	assert.Equal(t, "/login", location(b.get("/")))
	b.login()
	assert.Equal(t, "/queue", location(b.get("/")))
	assert.Equal(t, "/queue", location(b.get("/setup")), "signed in: setup page leads to the app")
}

// stylesheetName is the hashed name of the stylesheet that the login (or setup) page links.
func stylesheetName(t *testing.T, e *env) string {
	t.Helper()
	page := "/login"
	if e.setup.Required() {
		page = "/setup"
	}
	doc := parseHTML(t, e.browser().get(page).Body.String())
	link := find(doc, func(n *html.Node) bool { return n.Data == "link" && attr(n, "rel") == "stylesheet" })
	require.NotNil(t, link)
	return strings.TrimPrefix(attr(link, "href"), "/static/")
}
