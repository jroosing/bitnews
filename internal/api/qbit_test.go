package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/qbit"
	"gitlab.com/kyura4k/gitnews/internal/adapter/sabnzbd"
	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// nullEngine is a torrent engine with no torrents that accepts every add.
type nullEngine struct{ urls []string }

func (*nullEngine) List(context.Context) ([]qbit.TorrentView, error) { return nil, nil }
func (*nullEngine) Get(context.Context, string) (qbit.TorrentView, bool, error) {
	return qbit.TorrentView{}, false, nil
}
func (*nullEngine) AddTorrentFile(context.Context, []byte, qbit.AddOptions) (string, error) {
	return strings.Repeat("a", 40), nil
}
func (n *nullEngine) AddURL(_ context.Context, u string, _ qbit.AddOptions) (string, error) {
	n.urls = append(n.urls, u)
	return strings.Repeat("b", 40), nil
}
func (*nullEngine) SetShareLimit(context.Context, core.ID, float64, int64, bool) error { return nil }

// withQBit mounts the real qBittorrent adapter at /api/v2/.
func withQBit(eng *nullEngine) envOption {
	return withConfig(func(c *api.Config) { mountQBit(c, eng) })
}

func mountQBit(c *api.Config, eng *nullEngine) {
	c.QBit = qbit.New(qbit.Deps{
		Credentials: c.AdapterCredentials, Downloads: c.Downloads, Categories: c.Categories, Settings: c.Settings,
		Torrents: eng, Adder: eng, ShareLimits: eng, Logger: c.Logger,
	})
}

// withQBitAndSABnzbd mounts both adapters (withConfig keeps only one mutation).
func withQBitAndSABnzbd(eng *nullEngine) envOption {
	return withConfig(func(c *api.Config) {
		mountQBit(c, eng)
		c.SABnzbd = sabnzbd.New(sabnzbd.Deps{
			Credentials: c.AdapterCredentials, Downloads: c.Downloads, Adder: c.Adder,
			Categories: c.Categories, Settings: c.Settings, Logger: c.Logger,
		})
	})
}

// qbitLogin creates an adapter_qbit credential through the API, as the UI
// does, and logs in with it. It returns the SID cookie and the credential.
func (e *env) qbitLogin(sess session) (sid *http.Cookie, user, pass string) {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_qbit","name":"Radarr","username":"radarr"}`, sess.mods()...)
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	var c adapterCredJSON
	decode(e.t, rec, &c)
	r := httptest.NewRequest("POST", "/api/v2/auth/login", strings.NewReader(url.Values{"username": {c.Username}, "password": {c.Key}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	out := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(out, r)
	require.Equal(e.t, "Ok.", out.Body.String())
	for _, ck := range out.Result().Cookies() {
		if ck.Name == "SID" {
			return &http.Cookie{Name: ck.Name, Value: ck.Value}, c.Username, c.Key
		}
	}
	e.t.Fatal("no SID cookie")
	return nil, "", ""
}

func TestQBitRouteServesTheSIDSessionOfAnAdapterCredential(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withQBit(&nullEngine{}))
	sid, _, _ := e.qbitLogin(e.login())

	ok := e.do("GET", "/api/v2/app/webapiVersion", "", withCookie(sid))
	none := e.do("GET", "/api/v2/app/webapiVersion", "")

	assert.Equal(t, http.StatusOK, ok.Code)
	assert.Equal(t, qbit.DefaultWebAPIVersion, ok.Body.String())
	assert.Equal(t, http.StatusForbidden, none.Code)
	assert.Equal(t, "Forbidden", none.Body.String())
}

func TestTheQBitRouteAcceptsEveryMethodAndNeedsASession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withQBit(&nullEngine{}))
	sid, _, _ := e.qbitLogin(e.login())

	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS"} {
		assert.Equal(t, http.StatusForbidden, e.do(method, "/api/v2/app/version", "").Code, method+" without a session reaches the adapter")
		assert.NotEqual(t, http.StatusNotFound, e.do(method, "/api/v2/app/version", "", withCookie(sid)).Code, method)
	}
	assert.Equal(t, http.StatusNotFound, e.do("GET", "/api/v3/app/version", "").Code)
}

func TestWithoutAHandlerTheQBitPathIsNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("GET", "/api/v2/app/version", "")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorOf(t, rec))
}

func TestTheQBitRouteIsBehindTheSetupGate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutAdmin(), withQBit(&nullEngine{}))

	rec := e.do("GET", "/api/v2/app/version", "")

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "setup_required", errorOf(t, rec))
}

func TestTheQBitRouteGetsRequestIDsAndSecurityHeadersButNoUICSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withQBit(&nullEngine{}))
	sid, _, _ := e.qbitLogin(e.login())

	// A POST with the SID and no UI CSRF token: the adapter has its own rule.
	rec := e.do("POST", "/api/v2/torrents/createCategory", "category=tv", withCookie(sid),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'self'")
}

func TestTheQBitCSRFRuleStillAppliesBehindTheStack(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withQBit(&nullEngine{}))
	sid, _, _ := e.qbitLogin(e.login())

	rec := e.do("POST", "/api/v2/torrents/createCategory", "category=tv", withCookie(sid),
		withHeader("Content-Type", "application/x-www-form-urlencoded"), withHeader("Origin", "https://evil.example"))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAQBitCredentialAndItsSIDAreRefusedOnTheOwnAPIAndSABnzbd(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withQBitAndSABnzbd(&nullEngine{}))
	sid, _, pass := e.qbitLogin(e.login())

	for _, path := range []string{"/api/v1/downloads", "/api/v1/settings", "/api/v1/categories", "/api/v1/version", "/api/v1/events"} {
		assert.Equal(t, http.StatusUnauthorized, e.do("GET", path, "", withBearer(pass)).Code, path+" with the password")
		assert.Equal(t, http.StatusUnauthorized, e.do("GET", path, "", withHeader("X-Api-Key", pass)).Code, path)
		assert.Equal(t, http.StatusUnauthorized, e.do("GET", path, "", withCookie(sid)).Code, path+" with the SID")
		assert.Equal(t, http.StatusUnauthorized, e.do("GET", path, "", withCookie(&http.Cookie{Name: "bitnews_session", Value: sid.Value})).Code, path)
	}
	incorrect := `{"status":false,"error":"API Key Incorrect"}`
	required := `{"status":false,"error":"API Key Required"}`
	assert.JSONEq(t, incorrect, e.do("GET", "/sabnzbd/api?mode=queue&apikey="+pass, "").Body.String(), "the qBit password is no SAB key")
	assert.JSONEq(t, incorrect, e.do("GET", "/sabnzbd/api?mode=queue&apikey="+sid.Value, "").Body.String(), "the SID is no SAB key")
	assert.JSONEq(t, required, e.do("GET", "/sabnzbd/api?mode=queue", "", withCookie(sid)).Body.String(), "the SID cookie means nothing to SAB")
}

func TestTheOtherDirectionASABKeyAndAUISessionAreNoQBitLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withQBitAndSABnzbd(&nullEngine{}))
	sess := e.login()
	sabKey, _ := e.sabKey(sess)
	apiKey := e.newKey("write")

	login := func(user, pass string) string {
		return e.do("POST", "/api/v2/auth/login", url.Values{"username": {user}, "password": {pass}}.Encode(),
			withHeader("Content-Type", "application/x-www-form-urlencoded")).Body.String()
	}

	assert.Equal(t, "Fails.", login("radarr", sabKey))
	assert.Equal(t, "Fails.", login("admin", adminPassword), "the UI account is no qBit account")
	assert.Equal(t, "Fails.", login("radarr", apiKey))
	for _, c := range []string{sess.cookie.Value, apiKey, sabKey} {
		assert.Equal(t, http.StatusForbidden, e.do("GET", "/api/v2/app/version", "", withCookie(&http.Cookie{Name: "SID", Value: c})).Code)
		assert.Equal(t, http.StatusForbidden, e.do("GET", "/api/v2/app/version", "", withBearer(c)).Code)
	}
	assert.Equal(t, http.StatusForbidden, e.do("GET", "/api/v2/app/version", "", withCookie(sess.cookie)).Code, "the UI session cookie")
}

func TestDisabledAuthAndLANBypassNeverReachTheQBitRoute(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withQBit(&nullEngine{}))
	sess := e.login()
	require.Equal(t, http.StatusOK, e.do("PUT", "/api/v1/settings", openAuthJSON, sess.mods()...).Code)
	require.Equal(t, http.StatusOK, e.do("GET", "/api/v1/downloads", "").Code, "the UI is open")

	rec := e.do("GET", "/api/v2/torrents/info", "")

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestRevokingTheQBitCredentialKillsItsSessionAtOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withQBit(&nullEngine{}))
	sess := e.login()
	sid, _, _ := e.qbitLogin(sess)
	rec := e.do("GET", "/api/v1/adapter-credentials", "", sess.mods()...)
	var list struct {
		Items []adapterCredJSON `json:"items"`
	}
	decode(t, rec, &list)
	require.Len(t, list.Items, 1)
	require.Equal(t, http.StatusOK, e.do("GET", "/api/v2/app/version", "", withCookie(sid)).Code)

	require.Equal(t, http.StatusNoContent, e.do("DELETE", "/api/v1/adapter-credentials/"+list.Items[0].ID, "", sess.mods()...).Code)

	assert.Equal(t, http.StatusForbidden, e.do("GET", "/api/v2/app/version", "", withCookie(sid)).Code)
}

func TestTheAccessLogOfQBitHoldsThePathAndNothingElse(t *testing.T) {
	t.Parallel()
	eng := &nullEngine{}
	e := newEnv(t, withQBit(eng))
	sid, user, pass := e.qbitLogin(e.login())
	passkey := "P4SSKEY-9f8e7d"
	magnet := "magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056&tr=https%3A%2F%2Ftr.example%2Fannounce%3Fpasskey%3D" + passkey

	add := e.do("POST", "/api/v2/torrents/add?urls="+url.QueryEscape(magnet)+"&apikey="+passkey, url.Values{"urls": {magnet}}.Encode(),
		withCookie(sid), withHeader("Content-Type", "application/x-www-form-urlencoded"))
	get := e.do("GET", "/api/v2/torrents/info?hashes="+strings.Repeat("a", 40)+"&token="+passkey, "", withCookie(sid))

	require.Equal(t, "Ok.", add.Body.String())
	require.Equal(t, http.StatusOK, get.Code)
	require.NotEmpty(t, eng.urls, "the adapter did get the URL")
	logs := e.log.String()
	assert.NotContains(t, logs, passkey)
	assert.NotContains(t, logs, "tr.example")
	assert.NotContains(t, logs, pass, "the password is never logged")
	assert.NotContains(t, logs, sid.Value, "the SID is never logged")
	assert.Contains(t, logs, "/api/v2/torrents/add", "the request itself is logged")
	assert.NotContains(t, logs, "/api/v2/torrents/add?")
	assert.NotContains(t, logs, "/api/v2/torrents/info?")
	assert.Contains(t, logs, user, "the login is logged by username")
}
