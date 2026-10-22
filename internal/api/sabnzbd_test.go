package api_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/sabnzbd"
	"gitlab.com/kyura4k/gitnews/internal/api"
)

// withSABnzbd mounts the real SABnzbd adapter at /sabnzbd/api.
func withSABnzbd() envOption {
	return withConfig(func(c *api.Config) {
		c.SABnzbd = sabnzbd.New(sabnzbd.Deps{
			AppKey: c.AppKey, Downloads: c.Downloads, Adder: c.Adder,
			Categories: c.Categories, Settings: c.Settings, Logger: c.Logger,
		})
	})
}

func TestSABnzbdRouteServesTheAppKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())

	ok := e.do("GET", "/sabnzbd/api?mode=get_cats&output=json&apikey="+e.apiKey(), "")

	require.Equal(t, http.StatusOK, ok.Code)
	assert.JSONEq(t, `{"categories":["*"]}`, ok.Body.String())
}

func TestOnlyTheKeyAsAParameterOpensTheSABnzbdRoute(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	sess := e.login()
	key := e.apiKey()
	required := `{"status":false,"error":"API Key Required"}`

	asBearer := e.do("GET", "/sabnzbd/api?mode=queue", "", withBearer(key))
	asSession := e.do("GET", "/sabnzbd/api?mode=queue", "", withCookie(sess.cookie))
	asSessionPost := e.do("POST", "/sabnzbd/api?mode=queue", "", sess.mods()...)

	assert.JSONEq(t, required, asBearer.Body.String(), "headers are not read: SAB takes the apikey parameter")
	assert.JSONEq(t, required, asSession.Body.String(), "the session cookie means nothing to an adapter")
	assert.JSONEq(t, required, asSessionPost.Body.String())
}

func TestDisabledAuthAndLANBypassNeverReachTheSABnzbdRoute(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	sess := e.login()
	require.Equal(t, http.StatusOK, e.do("PUT", "/api/v1/settings", openAuthJSON, sess.mods()...).Code)
	require.Equal(t, http.StatusOK, e.do("GET", "/api/v1/downloads", "").Code, "the UI is open")

	rec := e.do("GET", "/sabnzbd/api?mode=queue&output=json", "")

	assert.JSONEq(t, `{"status":false,"error":"API Key Required"}`, rec.Body.String())
}

func TestTheKeyAsAQueryParameterOnTheOwnAPIDoesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())

	rec := e.do("GET", "/api/v1/downloads?apikey="+e.apiKey(), "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRefreshingTheKeyStopsTheSABnzbdRouteAtOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	old := e.apiKey()
	require.Contains(t, e.do("GET", "/sabnzbd/api?mode=get_cats&apikey="+old, "").Body.String(), `"categories"`)

	fresh, err := e.appKey.Refresh(context.Background())
	require.NoError(t, err)

	assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, e.do("GET", "/sabnzbd/api?mode=get_cats&apikey="+old, "").Body.String())
	assert.Contains(t, e.do("GET", "/sabnzbd/api?mode=get_cats&apikey="+fresh.Reveal(), "").Body.String(), `"categories"`)
}

func TestTheSABnzbdRouteIsBehindTheSetupGate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutAdmin(), withSABnzbd())

	rec := e.do("GET", "/sabnzbd/api?mode=version", "")

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "setup_required", errorOf(t, rec))
}

func TestTheSABnzbdRouteGetsRequestIDsAndSecurityHeadersButNoCSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	key := e.apiKey()

	// A POST with a key and no CSRF token or Origin: keys are not cookies.
	rec := e.do("POST", "/sabnzbd/api?mode=get_cats&apikey="+key, "", withHeader("Origin", "https://evil.example"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"categories"`)
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'self'")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestTheSABnzbdRouteAcceptsEveryMethodAndTheTrailingSlash(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())

	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS"} {
		rec := e.do(method, "/sabnzbd/api?mode=version", "")
		assert.Equal(t, http.StatusOK, rec.Code, method)
	}
	assert.Equal(t, http.StatusOK, e.do("GET", "/sabnzbd/api/?mode=version", "").Code)
	assert.Equal(t, http.StatusNotFound, e.do("GET", "/sabnzbd/", "").Code)
	assert.Equal(t, http.StatusNotFound, e.do("GET", "/api?mode=version", "").Code, "/api at the root is not used")
}

func TestWithoutAHandlerTheSABnzbdPathIsNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("GET", "/sabnzbd/api?mode=version", "")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorOf(t, rec))
}

func TestAddURLThroughTheFullStackLeaksNeitherTheIndexerKeyNorTheAdapterKey(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t, withSABnzbd())
	ix := newIndexer(t, serveNZB(nzbXML(metaName)))
	key := e.apiKey()
	indexerURL := ix.URL + "/getnzb?apikey=" + urlSecret

	rec := e.do("GET", "/sabnzbd/api?mode=addurl&output=json&cat=tv&name="+url.QueryEscape(indexerURL)+"&apikey="+key, "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"nzo_ids"`)
	logs := e.log.String()
	assert.NotContains(t, logs, urlSecret, "the access log and the fetch log redact the indexer key")
	assert.NotContains(t, logs, key, "the adapter key is redacted in the access log")
	assert.Contains(t, logs, "/sabnzbd/api", "the request is still logged")
	assert.True(t, strings.Contains(logs, "mode=addurl"), "the mode is logged, nothing else of the query")
}
