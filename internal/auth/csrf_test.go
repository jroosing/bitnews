package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// csrfCase sends one request through the CSRF middleware with a principal in
// the context and returns the response.
type csrfCase struct {
	method    string
	principal *auth.Principal
	origin    string
	fetchSite string
	host      string
	allowed   []string
}

func (c csrfCase) run(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	settings := core.DefaultSettings()
	settings.Auth.AllowedHosts = c.allowed
	h := auth.CSRF(func() core.Settings { return settings }, nil)(okHandler)
	r := httptest.NewRequest(c.method, "http://example.test/queue/x/pause", nil)
	if c.host != "" {
		r.Host = c.host
	}
	if c.origin != "" {
		r.Header.Set("Origin", c.origin)
	}
	if c.fetchSite != "" {
		r.Header.Set("Sec-Fetch-Site", c.fetchSite)
	}
	if c.principal != nil {
		r = r.WithContext(auth.ContextWithPrincipal(r.Context(), *c.principal))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

var sessionPrincipal = &auth.Principal{Kind: auth.PrincipalSession, UserID: "u1"}

func TestCSRFRefusesACrossSiteRequestWith403(t *testing.T) {
	for name, c := range map[string]csrfCase{
		"Sec-Fetch-Site":       {fetchSite: "cross-site"},
		"same-site is no same": {fetchSite: "same-site"},
		"foreign Origin":       {origin: "https://evil.test", host: "example.test"},
		"null Origin":          {origin: "null", host: "example.test"},
	} {
		c.method, c.principal = "POST", sessionPrincipal

		assert.Equal(t, http.StatusForbidden, c.run(t).Code, name)
	}
}

func TestCSRFAcceptsSameOriginAndRequestsWithoutBrowserHeaders(t *testing.T) {
	for name, c := range map[string]csrfCase{
		"Sec-Fetch-Site same-origin": {fetchSite: "same-origin", origin: "http://example.test"},
		"Sec-Fetch-Site none":        {fetchSite: "none"},
		"same Origin":                {origin: "http://example.test", host: "example.test"},
		"no headers (curl)":          {},
	} {
		c.method, c.principal = "POST", sessionPrincipal

		assert.Equal(t, http.StatusTeapot, c.run(t).Code, name)
	}
}

func TestCSRFExemptsSafeMethods(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
		rec := csrfCase{method: m, principal: sessionPrincipal, fetchSite: "cross-site", origin: "https://evil.test"}.run(t)

		assert.Equal(t, http.StatusTeapot, rec.Code, m)
	}
}

func TestCSRFTrustsPagesUnderAnAllowedHost(t *testing.T) {
	for _, origin := range []string{"http://bitnews.lan", "https://bitnews.lan"} {
		rec := csrfCase{method: "POST", principal: sessionPrincipal, fetchSite: "cross-site", origin: origin,
			host: "10.0.0.5:9494", allowed: []string{"bitnews.lan"}}.run(t)

		assert.Equal(t, http.StatusTeapot, rec.Code, origin)
	}
}

func TestCSRFExemptsAPIKeyRequests(t *testing.T) {
	p := &auth.Principal{Kind: auth.PrincipalAPIKey}

	rec := csrfCase{method: "POST", principal: p, origin: "https://evil.test", fetchSite: "cross-site", host: "example.test"}.run(t)

	assert.Equal(t, http.StatusTeapot, rec.Code, "bearer credentials are not ambient, so CSRF does not apply")
}

func TestCSRFStillAppliesToBypassedAndAuthDisabledRequests(t *testing.T) {
	for _, kind := range []auth.PrincipalKind{auth.PrincipalLANBypass, auth.PrincipalAuthDisabled} {
		p := &auth.Principal{Kind: kind}

		foreign := csrfCase{method: "POST", principal: p, fetchSite: "cross-site"}.run(t)
		same := csrfCase{method: "POST", principal: p, fetchSite: "same-origin"}.run(t)

		assert.Equal(t, http.StatusForbidden, foreign.Code, kind)
		assert.Equal(t, http.StatusTeapot, same.Code, kind)
	}
}

func TestCSRFWithoutAPrincipalProtectsTheLoginToo(t *testing.T) {
	foreign := csrfCase{method: "POST", origin: "https://evil.test", host: "example.test"}.run(t)
	plain := csrfCase{method: "POST", host: "example.test"}.run(t)

	assert.Equal(t, http.StatusForbidden, foreign.Code, "login CSRF")
	assert.Equal(t, http.StatusTeapot, plain.Code)
}

func TestCSRFErrorBodyUsesTheAPIErrorShape(t *testing.T) {
	rec := csrfCase{method: "POST", principal: sessionPrincipal, fetchSite: "cross-site"}.run(t)

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.True(t, strings.HasPrefix(rec.Body.String(), `{"error":{"code":"csrf_origin_invalid"`))
}
