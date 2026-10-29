package qbit_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/qbit"
)

func (s *stack) loginAs(user, pass string) *httptest.ResponseRecorder {
	return s.serve(formRequest("POST", "/api/v2/auth/login", url.Values{"username": {user}, "password": {pass}}))
}

func TestLoginWithTheCredentialSetsAHardenedSIDCookie(t *testing.T) {
	t.Parallel()
	s := newStack(t)

	rec := s.loginAs(s.user, s.pass)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	sid := sidOf(t, rec)
	assert.Regexp(t, `^[0-9a-f]{32}$`, sid.Value, "a random 128-bit token")
	assert.True(t, sid.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, sid.SameSite)
	assert.Equal(t, "/", sid.Path)
	assert.False(t, sid.Secure, "plain HTTP")
}

func TestEverySessionGetsItsOwnToken(t *testing.T) {
	t.Parallel()
	s := newStack(t)

	assert.NotEqual(t, s.login().Value, s.login().Value)
}

func TestLoginWithAWrongPasswordOrUserAnswersFailsWithStatus200(t *testing.T) {
	t.Parallel()
	s := newStack(t)

	for name, creds := range map[string][2]string{
		"wrong password":           {s.user, "bitnews_wrong"},
		"unknown user":             {"nobody", s.pass},
		"old username":             {"radarr", s.pass},
		"empty":                    {"", ""},
		"the username as password": {s.user, s.user},
	} {
		rec := s.loginAs(creds[0], creds[1])

		assert.Equal(t, http.StatusOK, rec.Code, name)
		assert.Equal(t, "Fails.", rec.Body.String(), name)
		for _, c := range rec.Result().Cookies() {
			assert.NotEqual(t, "SID", c.Name, name+": no session")
		}
	}
}

func TestAfterFiveFailuresTheIPIsBannedEvenForTheRightPassword(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	for range 5 {
		require.Equal(t, "Fails.", s.loginAs(s.user, "bitnews_wrong").Body.String())
	}

	wrong := s.loginAs(s.user, "bitnews_wrong")
	right := s.loginAs(s.user, s.pass)

	for _, rec := range []*httptest.ResponseRecorder{wrong, right} {
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, "Your IP address has been banned after too many failed authentication attempts.", rec.Body.String())
		for _, c := range rec.Result().Cookies() {
			assert.NotEqual(t, "SID", c.Name)
		}
	}
}

func TestTheBanEndsAndOtherIPsAreNotBanned(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	for range 5 {
		s.loginAs(s.user, "bitnews_wrong")
	}
	other := formRequest("POST", "/api/v2/auth/login", url.Values{"username": {s.user}, "password": {s.pass}})
	other.RemoteAddr = "192.0.2.77:4000"

	assert.Equal(t, "Ok.", s.serve(other).Body.String(), "the limit is per IP, not per username")

	s.clock.Advance(2 * time.Second)
	assert.Equal(t, "Ok.", s.loginAs(s.user, s.pass).Body.String(), "the first block is one second")
}

func TestFailuresOfOneIPDoNotLockTheUsernameForTheOthers(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	for range 20 {
		bad := formRequest("POST", "/api/v2/auth/login", url.Values{"username": {s.user}, "password": {"bitnews_wrong"}})
		bad.RemoteAddr = "198.51.100.9:1234"
		s.serve(bad)
	}

	assert.Equal(t, "Ok.", s.loginAs(s.user, s.pass).Body.String())
}

func TestTheLimiterBehindAProxyUsesTheForwardedClientIP(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	cur := s.settings.Current()
	cur.Auth.TrustedProxies = []string{"192.0.2.0/24"}
	require.NoError(t, s.settings.Update(context.Background(), cur))
	attempt := func(client, pass string) *httptest.ResponseRecorder {
		r := formRequest("POST", "/api/v2/auth/login", url.Values{"username": {s.user}, "password": {pass}})
		r.Header.Set("X-Forwarded-For", client)
		return s.serve(r)
	}
	for range 5 {
		attempt("203.0.113.5", "bitnews_wrong")
	}

	assert.Equal(t, http.StatusForbidden, attempt("203.0.113.5", s.pass).Code, "that client is banned")
	assert.Equal(t, "Ok.", attempt("203.0.113.6", s.pass).Body.String(), "another client behind the proxy is not")
}

func TestProtectedEndpointsAnswer403ForbiddenWithoutASession(t *testing.T) {
	t.Parallel()
	s := newStack(t)

	for _, target := range []string{
		"/api/v2/app/version", "/api/v2/app/webapiVersion", "/api/v2/app/preferences",
		"/api/v2/torrents/info", "/api/v2/torrents/categories", "/api/v2/sync/maindata",
		"/api/v2/does/not/exist",
	} {
		rec := s.get(nil, target)

		assert.Equal(t, http.StatusForbidden, rec.Code, target)
		assert.Equal(t, "Forbidden", rec.Body.String(), target)
	}
	post := s.post(nil, "/api/v2/torrents/delete", url.Values{"hashes": {"all"}, "deleteFiles": {"true"}})
	assert.Equal(t, http.StatusForbidden, post.Code)
	assert.Equal(t, "Forbidden", post.Body.String())
}

func TestAMadeUpOrMalformedSIDIsForbidden(t *testing.T) {
	t.Parallel()
	s := newStack(t)

	for _, v := range []string{"", "x", strings.Repeat("0", 32), strings.Repeat("f", 4096), "a b", s.pass} {
		rec := s.get(&http.Cookie{Name: "SID", Value: v}, "/api/v2/app/version")

		assert.Equal(t, http.StatusForbidden, rec.Code, v)
	}
}

func TestOnlyTheSIDCookieCounts(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.get(&http.Cookie{Name: "session", Value: sid.Value}, "/api/v2/app/version")

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestTheSessionWorksForAHourOfIdleTimeThenExpires(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	s.clock.Advance(59 * time.Minute)
	require.Equal(t, http.StatusOK, s.get(sid, "/api/v2/app/version").Code)
	s.clock.Advance(59 * time.Minute)
	assert.Equal(t, http.StatusOK, s.get(sid, "/api/v2/app/version").Code, "use rotatees the idle timer")

	s.clock.Advance(61 * time.Minute)

	assert.Equal(t, http.StatusForbidden, s.get(sid, "/api/v2/app/version").Code)
}

func TestLogoutEndsTheSession(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	other := s.login()

	rec := s.post(sid, "/api/v2/auth/logout", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, http.StatusForbidden, s.get(sid, "/api/v2/app/version").Code)
	assert.Equal(t, http.StatusOK, s.get(other, "/api/v2/app/version").Code, "other sessions stay")
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == "SID" && c.MaxAge < 0)
	}
	assert.True(t, cleared, "the cookie is dropped")
}

func TestRotatingTheKeyKillsEverySessionAtOnceAndOnlyTheNewKeyLogsIn(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid, other := s.login(), s.login()
	require.Equal(t, http.StatusOK, s.get(sid, "/api/v2/app/version").Code)

	fresh, err := s.appKey.Rotate(context.Background())
	require.NoError(t, err)

	assert.Equal(t, http.StatusForbidden, s.get(sid, "/api/v2/app/version").Code)
	assert.Equal(t, http.StatusForbidden, s.get(other, "/api/v2/app/version").Code)
	assert.Equal(t, "Fails.", s.loginAs(s.user, s.pass).Body.String(), "the old key")
	fresh1 := s.loginAs(s.user, fresh.Reveal())
	assert.Equal(t, "Ok.", fresh1.Body.String())
	assert.Equal(t, http.StatusOK, s.get(sidOf(t, fresh1), "/api/v2/app/version").Code)
}

func TestTheSessionTableIsCapped(t *testing.T) {
	t.Parallel()
	s := newStack(t, withDeps(func(d *qbit.Deps) { d.MaxSessions = 3 }))
	first := s.login()
	s.clock.Advance(time.Second)
	second := s.login()
	s.clock.Advance(time.Second)
	third := s.login()
	s.clock.Advance(time.Second)
	require.Equal(t, http.StatusOK, s.get(first, "/api/v2/app/version").Code, "first becomes recently used")
	s.clock.Advance(time.Second)

	fourth := s.login()

	assert.Equal(t, http.StatusForbidden, s.get(second, "/api/v2/app/version").Code, "the least recently used session is evicted")
	for _, ok := range []*http.Cookie{first, third, fourth} {
		assert.Equal(t, http.StatusOK, s.get(ok, "/api/v2/app/version").Code)
	}
}

func TestExpiredSessionsMakeRoomBeforeLiveOnesAreEvicted(t *testing.T) {
	t.Parallel()
	s := newStack(t, withDeps(func(d *qbit.Deps) { d.MaxSessions = 2 }))
	old := s.login()
	s.clock.Advance(30 * time.Minute)
	live := s.login()
	s.clock.Advance(40 * time.Minute) // old is idle 70 min, live 40

	fresh := s.login()

	assert.Equal(t, http.StatusForbidden, s.get(old, "/api/v2/app/version").Code)
	assert.Equal(t, http.StatusOK, s.get(live, "/api/v2/app/version").Code)
	assert.Equal(t, http.StatusOK, s.get(fresh, "/api/v2/app/version").Code)
}

func TestTheCookieIsSecureOverTLSAndBehindATrustedHTTPSProxy(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	login := func(mod func(*http.Request)) *http.Cookie {
		r := formRequest("POST", "/api/v2/auth/login", url.Values{"username": {s.user}, "password": {s.pass}})
		mod(r)
		return sidOf(t, s.serve(r))
	}

	assert.True(t, login(func(r *http.Request) { r.TLS = &tls.ConnectionState{} }).Secure, "TLS")
	assert.False(t, login(func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }).Secure,
		"an untrusted peer may not claim HTTPS")

	cur := s.settings.Current()
	cur.Auth.TrustedProxies = []string{"192.0.2.0/24"}
	require.NoError(t, s.settings.Update(context.Background(), cur))

	assert.True(t, login(func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }).Secure, "a trusted proxy")
	assert.False(t, login(func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }).Secure)
}

func TestLoginNeedsPOSTAndRejectsCredentialsInTheQuery(t *testing.T) {
	t.Parallel()
	s := newStack(t)

	get := s.get(nil, "/api/v2/auth/login?username="+s.user+"&password="+s.pass)
	queryOnly := s.serve(httptest.NewRequest("POST", "/api/v2/auth/login?username="+s.user+"&password="+s.pass, nil))

	assert.Equal(t, http.StatusMethodNotAllowed, get.Code)
	assert.Equal(t, "Fails.", queryOnly.Body.String(), "the credentials are read from the form body only")
}

func TestLoginRefusesAHugeBody(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	form := url.Values{"username": {s.user}, "password": {strings.Repeat("x", 1<<20)}}

	rec := s.serve(formRequest("POST", "/api/v2/auth/login", form))

	assert.NotEqual(t, "Ok.", rec.Body.String())
	assert.Contains(t, []int{http.StatusOK, http.StatusRequestEntityTooLarge, http.StatusBadRequest}, rec.Code)
}

func TestCSRFACrossSiteRequestIsRefusedAndOneWithoutHeadersIsFine(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	post := func(mod func(*http.Request)) *httptest.ResponseRecorder {
		r := formRequest("POST", "/api/v2/torrents/createCategory", url.Values{"category": {"cat" + strings.ToLower(time.Now().Format("150405.000000000"))}})
		r.AddCookie(sid)
		mod(r)
		return s.serve(r)
	}

	assert.Equal(t, http.StatusOK, post(func(*http.Request) {}).Code, "Sonarr sends neither header")
	assert.Equal(t, http.StatusOK, post(func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }).Code)
	assert.Equal(t, http.StatusOK, post(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }).Code)
	for name, mod := range map[string]func(*http.Request){
		"origin":         func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"null origin":    func(r *http.Request) { r.Header.Set("Origin", "null") },
		"sec-fetch-site": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		rec := post(mod)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.Equal(t, "Unauthorized", rec.Body.String(), name)
	}
}

func TestCSRFAlsoGuardsLogin(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	r := formRequest("POST", "/api/v2/auth/login", url.Values{"username": {s.user}, "password": {s.pass}})
	r.Header.Set("Origin", "https://evil.example")

	rec := s.serve(r)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	for _, c := range rec.Result().Cookies() {
		assert.NotEqual(t, "SID", c.Name)
	}
}

func TestAnAllowedHostMayBeTheOrigin(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	cur := s.settings.Current()
	cur.Auth.AllowedHosts = []string{"bitnews.lan"}
	require.NoError(t, s.settings.Update(context.Background(), cur))
	sid := s.login()
	r := formRequest("POST", "/api/v2/torrents/createCategory", url.Values{"category": {"viaproxy"}})
	r.AddCookie(sid)
	r.Header.Set("Origin", "https://bitnews.lan:8443")

	assert.Equal(t, http.StatusOK, s.serve(r).Code)
}

func TestTheKeyItselfIsNotASID(t *testing.T) {
	t.Parallel()
	s := newStack(t)

	assert.Equal(t, http.StatusForbidden, s.get(&http.Cookie{Name: "SID", Value: s.pass}, "/api/v2/app/version").Code)
}

func TestAnUnknownEndpointOfASessionIs404AndAWrongMethodIs405(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	assert.Equal(t, http.StatusNotFound, s.get(sid, "/api/v2/nope/nothing").Code)
	assert.Equal(t, http.StatusNotFound, s.get(sid, "/api/v2/").Code)
	assert.Equal(t, http.StatusMethodNotAllowed, s.get(sid, "/api/v2/torrents/delete?hashes=all").Code)
	assert.Equal(t, http.StatusOK, s.post(sid, "/api/v2/app/version", nil).Code, "read endpoints take POST too")
}
