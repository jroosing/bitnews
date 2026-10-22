package web_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

// assetLinks lists the /static/ URLs a page references.
func assetLinks(t *testing.T, e *env) []string {
	t.Helper()
	doc := parseHTML(t, e.browser().get("/login").Body.String())
	var out []string
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		for _, k := range []string{"src", "href"} {
			if v := attr(n, k); strings.HasPrefix(v, "/static/") {
				out = append(out, v)
			}
		}
	})
	require.NotEmpty(t, out)
	return out
}

var hashedName = regexp.MustCompile(`^/static/.+\.[0-9a-f]{8}\.[a-z0-9]+$`)

func TestTemplatesLinkContentHashedAssets(t *testing.T) {
	e := newEnv(t)

	for _, link := range assetLinks(t, e) {
		assert.Regexp(t, hashedName, link)
	}
}

func TestHashedAssetsAreImmutableAndPlainNamesRevalidate(t *testing.T) {
	e := newEnv(t)
	b := e.browser()

	for _, link := range assetLinks(t, e) {
		rec := b.get(link)

		require.Equal(t, http.StatusOK, rec.Code, link)
		assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"), link)
		assert.NotEmpty(t, rec.Header().Get("ETag"), link)
		assert.NotEmpty(t, rec.Body.Bytes(), link)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	}

	rec := b.get("/static/js/app.js")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get("Content-Type"))
}

func TestAssetContentTypes(t *testing.T) {
	e := newEnv(t)
	b := e.browser()

	for _, tc := range []struct{ prefix, ctype string }{
		{"/static/app.", "text/css; charset=utf-8"},
		{"/static/js/app.", "text/javascript; charset=utf-8"},
		{"/static/favicon.", "image/svg+xml"},
	} {
		var link string
		for _, l := range assetLinks(t, e) {
			if strings.HasPrefix(l, tc.prefix) {
				link = l
			}
		}
		require.NotEmpty(t, link, tc.prefix)
		assert.Equal(t, tc.ctype, b.get(link).Header().Get("Content-Type"), link)
	}
	assert.Equal(t, "font/woff2", b.get("/static/fonts/inter-latin-wght-normal.woff2").Header().Get("Content-Type"))
}

func TestConditionalRequestsGet304(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	link := assetLinks(t, e)[0]
	etag := b.get(link).Header().Get("ETag")
	require.NotEmpty(t, etag)

	rec := b.get(link, header("If-None-Match", etag))

	assert.Equal(t, http.StatusNotModified, rec.Code)
}

func TestStylesheetFontsAreServedByHashedName(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	var css string
	for _, l := range assetLinks(t, e) {
		if strings.HasSuffix(l, ".css") {
			css = l
		}
	}
	body := b.get(css).Body.String()

	fonts := regexp.MustCompile(`url\("?(/static/fonts/[^")]+)"?\)`).FindAllStringSubmatch(body, -1)

	require.NotEmpty(t, fonts, "font URLs are rewritten to hashed names")
	for _, m := range fonts {
		assert.Regexp(t, hashedName, m[1])
		rec := b.get(m[1])
		assert.Equal(t, http.StatusOK, rec.Code, m[1])
		assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
	}
	assert.NotRegexp(t, `url\(["']?fonts/`, body, "no unhashed relative font URL is left")
}

func TestStaticRefusesTraversalDirectoriesAndUnknownNames(t *testing.T) {
	e := newEnv(t)
	b := e.browser()

	for _, target := range []string{
		"/static/", "/static/js/", "/static/js", "/static/fonts/", "/static/nope.css",
		"/static/../go.mod", "/static/%2e%2e/go.mod", "/static/js/../../go.mod", "/static/js/%2e%2e/%2e%2e/templates/head.html",
		"/static/..%2fassets/templates/head.html", "/static//app.css", "/static/templates/head.html", "/static/js/app.js/",
		"/static/app.00000000.css", "/static/js/app.zzzzzzzz.js",
	} {
		rec := b.get(target)

		assert.Contains(t, []int{http.StatusNotFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusBadRequest}, rec.Code, "%s -> %d", target, rec.Code)
		assert.NotContains(t, rec.Body.String(), "<!doctype", target)
		assert.NotContains(t, rec.Body.String(), "module ", target)
	}
}

func TestStaticOnlyAnswersGetAndHead(t *testing.T) {
	e := newEnv(t)
	link := assetLinks(t, e)[0]

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, httptest.NewRequest(method, link, nil))

		assert.NotEqual(t, http.StatusOK, rec.Code, method)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, link, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.Bytes())
}
