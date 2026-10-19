package api_test

import (
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
			Credentials: c.AdapterCredentials, Downloads: c.Downloads, Adder: c.Adder,
			Categories: c.Categories, Settings: c.Settings, Logger: c.Logger,
		})
	})
}

// sabKey creates an adapter_sab credential through the API, as the UI does.
func (e *env) sabKey(sess session) (key, id string) {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"Sonarr"}`, sess.mods()...)
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	var c adapterCredJSON
	decode(e.t, rec, &c)
	return c.Key, c.ID
}

func TestSABnzbdRouteServesAdapterCredentialsOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	sess := e.login()
	key, _ := e.sabKey(sess)

	ok := e.do("GET", "/sabnzbd/api?mode=get_cats&output=json&apikey="+key, "")

	require.Equal(t, http.StatusOK, ok.Code)
	assert.JSONEq(t, `{"categories":["*"]}`, ok.Body.String())
}

func TestAnAPIKeyAndASessionAreRefusedOnTheSABnzbdRoute(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	sess := e.login()
	apiKey := e.newKey("write")
	incorrect := `{"status":false,"error":"API Key Incorrect"}`
	required := `{"status":false,"error":"API Key Required"}`

	asParam := e.do("GET", "/sabnzbd/api?mode=queue&apikey="+apiKey, "")
	asBearer := e.do("GET", "/sabnzbd/api?mode=queue", "", withBearer(apiKey))
	asSession := e.do("GET", "/sabnzbd/api?mode=queue", "", withCookie(sess.cookie))
	asSessionPost := e.do("POST", "/sabnzbd/api?mode=queue", "", sess.mods()...)

	assert.JSONEq(t, incorrect, asParam.Body.String(), "an own-API key is not an adapter credential")
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

func TestAnAdapterCredentialIsRefusedOnTheOwnAPI(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	sess := e.login()
	key, _ := e.sabKey(sess)
	qbit := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_qbit","name":"Radarr"}`, sess.mods()...)
	var q adapterCredJSON
	decode(t, qbit, &q)

	for _, k := range []string{key, q.Key} {
		for _, path := range []string{"/api/v1/downloads", "/api/v1/settings", "/api/v1/version", "/api/v1/categories", "/api/v1/events"} {
			asBearer := e.do("GET", path, "", withBearer(k))
			asHeader := e.do("GET", path, "", withHeader("X-Api-Key", k))

			assert.Equal(t, http.StatusUnauthorized, asBearer.Code, path)
			assert.Equal(t, http.StatusUnauthorized, asHeader.Code, path)
		}
		write := e.do("POST", "/api/v1/categories", `{"name":"x"}`, withBearer(k))
		assert.Equal(t, http.StatusUnauthorized, write.Code)
		assert.Equal(t, http.StatusUnauthorized, e.do("POST", "/api/v1/api-keys", `{"name":"x","scope":"write"}`, withBearer(k)).Code)
	}
}

func TestAnAdapterCredentialAsAQueryParameterOnTheOwnAPIDoesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	key, _ := e.sabKey(e.login())

	rec := e.do("GET", "/api/v1/downloads?apikey="+key, "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRevokingAnAdapterCredentialStopsTheSABnzbdRouteAtOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withSABnzbd())
	sess := e.login()
	key, id := e.sabKey(sess)
	path := "/sabnzbd/api?mode=get_cats&apikey=" + key
	require.Contains(t, e.do("GET", path, "").Body.String(), `"categories"`)

	require.Equal(t, http.StatusNoContent, e.do("DELETE", "/api/v1/adapter-credentials/"+id, "", sess.mods()...).Code)

	assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, e.do("GET", path, "").Body.String())
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
	key, _ := e.sabKey(e.login())

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
	key, _ := e.sabKey(e.login())
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
