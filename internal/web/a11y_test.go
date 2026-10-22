package web_test

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/net/html"
)

// Every control needs an accessible name: a <label for>, a wrapping label, or
// aria-label / aria-labelledby.
func TestEveryControlHasAnAccessibleName(t *testing.T) {
	for name, rec := range htmlResponses(t) {
		if rec.Body.Len() == 0 {
			continue
		}
		doc := parseHTML(t, rec.Body.String())
		labels := map[string]bool{}
		for _, l := range findAll(doc, func(n *html.Node) bool { return n.Data == "label" }) {
			if f := attr(l, "for"); f != "" {
				labels[f] = true
			}
		}
		controls := findAll(doc, func(n *html.Node) bool {
			switch n.Data {
			case "input":
				return attr(n, "type") != "hidden"
			case "select", "textarea", "progress":
				return true
			}
			return false
		})
		for _, c := range controls {
			named := labels[attr(c, "id")] || attr(c, "aria-label") != "" || attr(c, "aria-labelledby") != ""
			assert.True(t, named, "%s: <%s id=%q name=%q> has no label", name, c.Data, attr(c, "id"), attr(c, "name"))
		}
		for _, b := range findAll(doc, func(n *html.Node) bool { return n.Data == "button" }) {
			assert.NotEmpty(t, text(b)+attr(b, "aria-label")+attr(b, ":aria-label")+attr(b, "x-text"), "%s: button without a name", name)
		}
		seen := map[string]bool{}
		for _, n := range findAll(doc, func(n *html.Node) bool { return attr(n, "id") != "" }) {
			id := attr(n, "id")
			assert.False(t, seen[id], "%s: duplicate id %q", name, id)
			seen[id] = true
		}
		for _, img := range findAll(doc, func(n *html.Node) bool { return n.Data == "img" }) {
			assert.True(t, hasAttr(img, "alt"), "%s: img without alt", name)
		}
	}
}

func TestFormErrorsAreAnnouncedAndLinkedToTheirFields(t *testing.T) {
	e := newEnv(t, withoutAdmin())

	rec := e.browser().post("/setup", setupForm(e.token, "owner", "short", "short"))

	doc := parseHTML(t, rec.Body.String())
	pw := byID(doc, "password")
	assert.Equal(t, "true", attr(pw, "aria-invalid"))
	assert.Contains(t, attr(pw, "aria-describedby"), "password-error")
	assert.Contains(t, attr(pw, "aria-describedby"), "password-help")
}

func TestLoginErrorIsAnAlertRegion(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().post("/login", url.Values{"username": {"admin"}, "password": {"nope nope nope"}})

	assert.Equal(t, "alert", attr(byID(parseHTML(t, rec.Body.String()), "login-error"), "role"))
}

func TestToastRegionIsALiveRegionOnEveryPage(t *testing.T) {
	for name, rec := range htmlResponses(t) {
		if rec.Body.Len() == 0 {
			continue
		}
		assertToast(t, name, rec)
	}
}

func assertToast(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	doc := parseHTML(t, rec.Body.String())
	region := find(doc, func(n *html.Node) bool { return attr(n, "x-data") == "toastRegion" })
	if region == nil {
		// Fragments swapped into a page have none.
		assert.NotContains(t, rec.Body.String(), "<html", name)
		return
	}
	assert.Equal(t, "polite", attr(region, "aria-live"), name)
}
