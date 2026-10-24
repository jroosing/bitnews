package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// uiEnv wires a SessionService, UIAuth and CSRF middleware around a handler
// that records the principal it sees.
type uiEnv struct {
	sessionEnv
	ui       *auth.UIAuth
	settings core.Settings
	handler  http.Handler
	seen     *auth.Principal
}

func newUIEnv(t *testing.T) *uiEnv {
	t.Helper()
	e := &uiEnv{sessionEnv: newSessionEnv(t), settings: core.DefaultSettings()}
	getSettings := func() core.Settings { return e.settings }
	var err error
	e.ui, err = auth.NewUIAuth(e.svc, getSettings)
	require.NoError(t, err)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if ok {
			e.seen = &p
		}
		w.WriteHeader(http.StatusTeapot)
	})
	e.handler = e.ui.Middleware(auth.CSRF(getSettings)(inner))
	return e
}

func (e *uiEnv) login(t *testing.T) string {
	t.Helper()
	token, _, err := e.svc.CreateSession(context.Background(), e.user.ID)
	require.NoError(t, err)
	return token
}

type uiReq struct {
	method, host, remote, xff, cookie, csrf, origin string
}

func (e *uiEnv) do(q uiReq) *httptest.ResponseRecorder {
	e.seen = nil
	method := q.method
	if method == "" {
		method = "GET"
	}
	r := httptest.NewRequest(method, "http://bitnews.lan:9494/api/v1/downloads", nil)
	if q.host != "" {
		r.Host = q.host
	}
	r.RemoteAddr = "203.0.113.50:5555"
	if q.remote != "" {
		r.RemoteAddr = q.remote
	}
	if q.xff != "" {
		r.Header.Set("X-Forwarded-For", q.xff)
	}
	if q.cookie != "" {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: q.cookie})
	}
	if q.csrf != "" {
		r.Header.Set("X-CSRF-Token", q.csrf)
	}
	if q.origin != "" {
		r.Header.Set("Origin", q.origin)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return rec
}

func apiErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code
}

func TestUIAuthAcceptsAValidSessionCookie(t *testing.T) {
	e := newUIEnv(t)
	token := e.login(t)

	rec := e.do(uiReq{cookie: token})

	assert.Equal(t, http.StatusTeapot, rec.Code)
	require.NotNil(t, e.seen)
	assert.Equal(t, auth.PrincipalSession, e.seen.Kind)
	assert.Equal(t, e.user.ID, e.seen.UserID)
	assert.Equal(t, "admin", e.seen.Username)
}

func TestPrincipalCSRFTokenIsTheSessionsSynchronizerToken(t *testing.T) {
	e := newUIEnv(t)
	token := e.login(t)

	e.do(uiReq{cookie: token})

	require.NotNil(t, e.seen)
	assert.Equal(t, auth.CSRFToken(token), e.seen.CSRFToken())
}

func TestUIAuthRejectsMissingOrInvalidCookieWith401(t *testing.T) {
	e := newUIEnv(t)

	for _, cookie := range []string{"", "garbage", "bitnews_" + e.login(t)} {
		rec := e.do(uiReq{cookie: cookie})

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "cookie %q", cookie)
		assert.Equal(t, "unauthorized", apiErrCode(t, rec))
		assert.Nil(t, e.seen, "handler must not run")
	}
}

func TestUIAuthRejectsAnExpiredSession(t *testing.T) {
	e := newUIEnv(t)
	token := e.login(t)
	e.clock.Advance(auth.SessionIdleTimeout + time.Second)

	rec := e.do(uiReq{cookie: token})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestUIAuthDisableAuthLetsAnonymousRequestsThrough(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}

	rec := e.do(uiReq{host: "bitnews.lan:9494"})

	assert.Equal(t, http.StatusTeapot, rec.Code)
	require.NotNil(t, e.seen)
	assert.Equal(t, auth.PrincipalAuthDisabled, e.seen.Kind)
	assert.Empty(t, e.seen.UserID)
}

func TestUIAuthLANBypassMatchesTheClientCIDR(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{LANBypassCIDRs: []string{"192.168.1.0/24"}, AllowedHosts: []string{"bitnews.lan"}}

	inside := e.do(uiReq{host: "bitnews.lan", remote: "192.168.1.20:1234"})
	require.Equal(t, http.StatusTeapot, inside.Code)
	assert.Equal(t, auth.PrincipalLANBypass, e.seen.Kind)

	outside := e.do(uiReq{host: "bitnews.lan", remote: "192.168.2.20:1234"})
	assert.Equal(t, http.StatusUnauthorized, outside.Code)
}

func TestUIAuthIgnoresSpoofedForwardedForFromAnUntrustedPeer(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{
		LANBypassCIDRs: []string{"192.168.1.0/24"},
		TrustedProxies: []string{"10.0.0.0/8"},
		AllowedHosts:   []string{"bitnews.lan"},
	}

	rec := e.do(uiReq{host: "bitnews.lan", remote: "203.0.113.50:1", xff: "192.168.1.20"})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestUIAuthHonoursForwardedForFromATrustedProxy(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{
		LANBypassCIDRs: []string{"192.168.1.0/24"},
		TrustedProxies: []string{"10.0.0.0/8"},
		AllowedHosts:   []string{"bitnews.lan"},
	}

	lan := e.do(uiReq{host: "bitnews.lan", remote: "10.0.0.2:1", xff: "192.168.1.20"})
	wan := e.do(uiReq{host: "bitnews.lan", remote: "10.0.0.2:1", xff: "198.51.100.9"})
	spoof := e.do(uiReq{host: "bitnews.lan", remote: "10.0.0.2:1", xff: "192.168.1.20, 198.51.100.9"})

	assert.Equal(t, http.StatusTeapot, lan.Code)
	assert.Equal(t, http.StatusUnauthorized, wan.Code)
	assert.Equal(t, http.StatusUnauthorized, spoof.Code, "the client-supplied left entry is ignored")
}

func TestUIAuthSessionWinsOverBypass(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}
	token := e.login(t)

	_ = e.do(uiReq{host: "bitnews.lan", cookie: token})

	assert.Equal(t, auth.PrincipalSession, e.seen.Kind)
}

func TestUIAuthAnInvalidCookieDoesNotBlockTheBypass(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}

	rec := e.do(uiReq{host: "bitnews.lan", cookie: "stale"})

	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, auth.PrincipalAuthDisabled, e.seen.Kind)
}

func TestUIAuthAnswers421ForAHostOutsideAllowedHostsWhileBypassIsOn(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"Bitnews.LAN"}}
	token := e.login(t)

	rebind := e.do(uiReq{host: "evil.test:9494"})
	rebindWithSession := e.do(uiReq{host: "evil.test", cookie: token})
	ok := e.do(uiReq{host: "bitnews.lan:9494"})

	assert.Equal(t, http.StatusMisdirectedRequest, rebind.Code)
	assert.Equal(t, "misdirected_request", apiErrCode(t, rebind))
	assert.Equal(t, http.StatusMisdirectedRequest, rebindWithSession.Code, "a valid session does not bypass the host check")
	assert.Equal(t, http.StatusTeapot, ok.Code)
}

func TestUIAuthHostCheckAlsoAppliesWithLANBypassOnly(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{LANBypassCIDRs: []string{"192.168.1.0/24"}, AllowedHosts: []string{"bitnews.lan"}}

	rec := e.do(uiReq{host: "evil.test", remote: "192.168.1.20:1"})

	assert.Equal(t, http.StatusMisdirectedRequest, rec.Code)
}

func TestUIAuthSkipsTheHostCheckWhenNoBypassIsConfigured(t *testing.T) {
	e := newUIEnv(t)
	token := e.login(t)

	rec := e.do(uiReq{host: "anything.example", cookie: token})

	assert.Equal(t, http.StatusTeapot, rec.Code)
}

func TestUIAuthReadsSettingsOnEveryRequest(t *testing.T) {
	e := newUIEnv(t)
	assert.Equal(t, http.StatusUnauthorized, e.do(uiReq{host: "bitnews.lan"}).Code)

	e.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}
	assert.Equal(t, http.StatusTeapot, e.do(uiReq{host: "bitnews.lan"}).Code)

	e.settings.Auth = core.Auth{}
	assert.Equal(t, http.StatusUnauthorized, e.do(uiReq{host: "bitnews.lan"}).Code)
}

func TestUIAuthRunsCSRFChecksForSessionRequests(t *testing.T) {
	e := newUIEnv(t)
	token := e.login(t)

	missing := e.do(uiReq{method: "POST", cookie: token})
	ok := e.do(uiReq{method: "POST", cookie: token, csrf: auth.CSRFToken(token)})
	foreign := e.do(uiReq{method: "POST", cookie: token, csrf: auth.CSRFToken(token), origin: "https://evil.test"})

	assert.Equal(t, http.StatusForbidden, missing.Code)
	assert.Equal(t, http.StatusTeapot, ok.Code)
	assert.Equal(t, http.StatusForbidden, foreign.Code)
}

func TestUIAuthKeepsCSRFEnforcedUnderDisableAuth(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}
	info := e.sessionInfo(t, uiReq{host: "bitnews.lan"})

	missing := e.do(uiReq{method: "POST", host: "bitnews.lan"})
	ok := e.do(uiReq{method: "POST", host: "bitnews.lan", csrf: info.CSRFToken})
	foreign := e.do(uiReq{method: "POST", host: "bitnews.lan", csrf: info.CSRFToken, origin: "https://evil.test"})

	assert.Equal(t, http.StatusForbidden, missing.Code)
	assert.Equal(t, http.StatusTeapot, ok.Code)
	assert.Equal(t, http.StatusForbidden, foreign.Code)
}

type sessionInfoBody struct {
	AuthMode  string `json:"auth_mode"`
	CSRFToken string `json:"csrf_token"`
	User      *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

// sessionInfo calls the session info handler behind the same middleware.
func (e *uiEnv) sessionInfo(t *testing.T, q uiReq) sessionInfoBody {
	t.Helper()
	h := e.ui.Middleware(e.ui.SessionInfo())
	r := httptest.NewRequest("GET", "http://bitnews.lan/api/v1/auth/session", nil)
	r.Host = q.host
	r.RemoteAddr = "203.0.113.50:5555"
	if q.cookie != "" {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: q.cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var body sessionInfoBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

func TestSessionInfoReportsUserCSRFTokenAndSessionMode(t *testing.T) {
	e := newUIEnv(t)
	token := e.login(t)

	body := e.sessionInfo(t, uiReq{cookie: token})

	assert.Equal(t, "session", body.AuthMode)
	assert.Equal(t, auth.CSRFToken(token), body.CSRFToken)
	require.NotNil(t, body.User)
	assert.Equal(t, "admin", body.User.Username)
	assert.Equal(t, string(e.user.ID), body.User.ID)
}

func TestSessionInfoReportsDisabledAndBypassModesWithoutAUser(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}
	disabled := e.sessionInfo(t, uiReq{host: "bitnews.lan"})

	e.settings.Auth = core.Auth{LANBypassCIDRs: []string{"203.0.113.0/24"}, AllowedHosts: []string{"bitnews.lan"}}
	bypass := e.sessionInfo(t, uiReq{host: "bitnews.lan"})

	assert.Equal(t, "disabled", disabled.AuthMode)
	assert.Equal(t, "bypass", bypass.AuthMode)
	assert.Nil(t, disabled.User)
	assert.Len(t, disabled.CSRFToken, 64)
	assert.Equal(t, disabled.CSRFToken, bypass.CSRFToken, "one anonymous token per process")
}

func TestSessionInfoRejectsAPIKeyPrincipals(t *testing.T) {
	e := newUIEnv(t)
	h := e.ui.SessionInfo()
	r := httptest.NewRequest("GET", "/api/v1/auth/session", nil)
	r = r.WithContext(auth.ContextWithPrincipal(r.Context(), auth.Principal{Kind: auth.PrincipalAPIKey}))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func cookieOf(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cs := rec.Result().Cookies()
	require.Len(t, cs, 1)
	return cs[0]
}

func TestSessionCookieFlags(t *testing.T) {
	e := newUIEnv(t)
	expires := e.clock.Now().Add(30 * 24 * time.Hour)
	rec := httptest.NewRecorder()

	e.ui.SetSessionCookie(rec, httptest.NewRequest("GET", "http://bitnews.lan/", nil), "tok", expires)

	c := cookieOf(t, rec)
	assert.Equal(t, "bitnews_session", c.Name)
	assert.Equal(t, "tok", c.Value)
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, c.SameSite)
	assert.Equal(t, "/", c.Path)
	assert.False(t, c.Secure, "plain HTTP")
	assert.True(t, c.Expires.Equal(expires.Truncate(time.Second)))
}

func TestSessionCookieIsSecureOverTLS(t *testing.T) {
	e := newUIEnv(t)
	r := httptest.NewRequest("GET", "https://bitnews.lan/", nil)
	rec := httptest.NewRecorder()

	e.ui.SetSessionCookie(rec, r, "tok", e.clock.Now().Add(time.Hour))

	assert.True(t, cookieOf(t, rec).Secure)
}

func TestSessionCookieSecureFollowsForwardedProtoOnlyFromATrustedProxy(t *testing.T) {
	e := newUIEnv(t)
	e.settings.Auth.TrustedProxies = []string{"10.0.0.0/8"}
	for _, tc := range []struct {
		remote string
		secure bool
	}{{"10.0.0.2:1", true}, {"203.0.113.50:1", false}} {
		r := httptest.NewRequest("GET", "http://bitnews.lan/", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()

		e.ui.SetSessionCookie(rec, r, "tok", e.clock.Now().Add(time.Hour))

		assert.Equal(t, tc.secure, cookieOf(t, rec).Secure, tc.remote)
	}
}

func TestClearSessionCookieExpiresIt(t *testing.T) {
	e := newUIEnv(t)
	rec := httptest.NewRecorder()

	e.ui.ClearSessionCookie(rec, httptest.NewRequest("POST", "http://bitnews.lan/", nil))

	c := cookieOf(t, rec)
	assert.Equal(t, "bitnews_session", c.Name)
	assert.Empty(t, c.Value)
	assert.Negative(t, c.MaxAge)
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, c.SameSite)
}
