package api_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func loginBody(user, password string) string {
	return `{"username":"` + user + `","password":"` + password + `"}`
}

func TestLoginSetsAHardenedSessionCookieAndReturnsTheSessionInfo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("POST", "/api/v1/auth/login", loginBody("admin", adminPassword))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		AuthMode  string `json:"auth_mode"`
		CSRFToken string `json:"csrf_token"`
		User      struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	decode(t, rec, &body)
	assert.Equal(t, "session", body.AuthMode)
	assert.NotEmpty(t, body.CSRFToken)
	assert.Equal(t, "admin", body.User.Username)
	assert.NotEmpty(t, body.User.ID)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, auth.SessionCookieName, cookies[0].Name)
	assert.True(t, cookies[0].HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, cookies[0].SameSite)
	assert.Equal(t, "/", cookies[0].Path)
	assert.WithinDuration(t, e.clock.Now().Add(auth.SessionAbsoluteTimeout), cookies[0].Expires, time.Second)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.NotContains(t, rec.Body.String(), adminPassword)
}

func TestSessionInfoShowsTheLoggedInUserAndRequiresAuthentication(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()

	anonymous := e.do("GET", "/api/v1/auth/session", "")
	authed := e.do("GET", "/api/v1/auth/session", "", withCookie(sess.cookie))

	assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
	assert.Equal(t, "unauthorized", errorOf(t, anonymous))
	require.Equal(t, http.StatusOK, authed.Code)
	assert.Contains(t, authed.Body.String(), sess.csrf)
}

func TestLoginRejectsWrongPasswordAndUnknownUserAlike(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	wrong := e.do("POST", "/api/v1/auth/login", loginBody("admin", "wrong password here"))
	unknown := e.do("POST", "/api/v1/auth/login", loginBody("nobody", "wrong password here"))

	for _, rec := range []*httptest.ResponseRecorder{wrong, unknown} {
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, "invalid_credentials", errorOf(t, rec))
		assert.Empty(t, rec.Result().Cookies())
	}
	assert.Equal(t, wrong.Body.String(), unknown.Body.String())
}

func TestLoginIsRateLimitedAfterRepeatedFailuresWithRetryAfter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for range 5 {
		rec := e.do("POST", "/api/v1/auth/login", loginBody("admin", "wrong password here"))
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}

	rec := e.do("POST", "/api/v1/auth/login", loginBody("admin", adminPassword))

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "rate_limited", errorOf(t, rec))
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	e.clock.Advance(2 * time.Second)
	assert.Equal(t, http.StatusOK, e.do("POST", "/api/v1/auth/login", loginBody("admin", adminPassword)).Code)
}

func TestLoginRejectsMalformedRequests(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for name, tc := range map[string]struct {
		body   string
		mods   []func(*http.Request)
		status int
		code   string
	}{
		"not json":      {"{nope", nil, 400, "invalid_json"},
		"unknown field": {`{"username":"admin","password":"x","admin":true}`, nil, 400, "invalid_json"},
		"trailing data": {loginBody("admin", "x") + `{}`, nil, 400, "invalid_json"},
		"empty body":    {"", []func(*http.Request){withHeader("Content-Type", "application/json")}, 400, "invalid_json"},
		"wrong type":    {loginBody("admin", "x"), []func(*http.Request){withHeader("Content-Type", "text/plain")}, 415, "unsupported_media_type"},
		"too large":     {`{"username":"` + strings.Repeat("a", 20<<10) + `"}`, nil, 413, "request_too_large"},
		"foreign origin": {loginBody("admin", adminPassword), []func(*http.Request){withHeader("Origin", "https://evil.example")},
			403, "csrf_origin_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("POST", "/api/v1/auth/login", tc.body, tc.mods...)

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
		})
	}
}

func TestLogoutEndsTheSessionAndClearsTheCookie(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()

	rec := e.do("POST", "/api/v1/auth/logout", "", sess.mods()...)

	require.Equal(t, http.StatusNoContent, rec.Code)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, auth.SessionCookieName, cookies[0].Name)
	assert.Less(t, cookies[0].MaxAge, 0)
	assert.Equal(t, http.StatusUnauthorized, e.do("GET", "/api/v1/auth/session", "", withCookie(sess.cookie)).Code)
}

func TestLogoutRequiresTheCSRFToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()

	rec := e.do("POST", "/api/v1/auth/logout", "", withCookie(sess.cookie))

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "csrf_token_invalid", errorOf(t, rec))
	assert.Equal(t, http.StatusOK, e.do("GET", "/api/v1/auth/session", "", withCookie(sess.cookie)).Code)
}

func TestVersionReportsBuildInfoToAnyReader(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	readKey := e.newKey(core.APIKeyScopeRead)

	anonymous := e.do("GET", "/api/v1/version", "")
	viaSession := e.do("GET", "/api/v1/version", "", withCookie(sess.cookie))
	viaKey := e.do("GET", "/api/v1/version", "", withBearer(readKey))

	assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
	for _, rec := range []*httptest.ResponseRecorder{viaSession, viaKey} {
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"version":"1.2.3","commit":"abc1234","go_version":"go9.99"}`, rec.Body.String())
	}
}

var setupTokenRE = regexp.MustCompile(`setup_token=(\S+)`)

func TestSetupCreatesTheAdminWithTheTokenFromTheLogAndOpensTheGate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutAdmin())
	m := setupTokenRE.FindStringSubmatch(e.log.String())
	require.NotNil(t, m, "no setup token in the log")
	gated := e.do("POST", "/api/v1/auth/login", loginBody("root", adminPassword))

	rec := e.do("POST", "/api/v1/setup", `{"token":"`+m[1]+`","username":"root","password":"`+adminPassword+`"}`)
	login := e.do("POST", "/api/v1/auth/login", loginBody("root", adminPassword))

	assert.Equal(t, http.StatusServiceUnavailable, gated.Code)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), adminPassword)
	assert.Equal(t, http.StatusOK, login.Code)
}

func TestSetupWithAWrongTokenIsForbidden(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutAdmin())

	rec := e.do("POST", "/api/v1/setup", `{"token":"nope","username":"root","password":"`+adminPassword+`"}`)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "invalid_setup_token", errorOf(t, rec))
}
