package auth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestCSRFTokenIsHexHMACOfTheSessionToken(t *testing.T) {
	mac := hmac.New(sha256.New, []byte("session-token"))
	mac.Write([]byte("bitnews-csrf-v1"))
	want := hex.EncodeToString(mac.Sum(nil))

	got := auth.CSRFToken("session-token")

	assert.Equal(t, want, got)
	assert.Len(t, got, 64)
}

func TestCSRFTokenDiffersPerSessionAndIsStable(t *testing.T) {
	assert.Equal(t, auth.CSRFToken("a"), auth.CSRFToken("a"))
	assert.NotEqual(t, auth.CSRFToken("a"), auth.CSRFToken("b"))
}

// csrfCase sends one request through the CSRF middleware with a principal in
// the context and returns the response.
type csrfCase struct {
	method    string
	principal *auth.Principal
	token     string
	origin    string
	host      string
	allowed   []string
}

func (c csrfCase) run(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	settings := core.DefaultSettings()
	settings.Auth.AllowedHosts = c.allowed
	h := auth.CSRF(func() core.Settings { return settings })(okHandler)
	r := httptest.NewRequest(c.method, "http://example.test/api/v1/downloads", nil)
	if c.host != "" {
		r.Host = c.host
	}
	if c.token != "" {
		r.Header.Set("X-CSRF-Token", c.token)
	}
	if c.origin != "" {
		r.Header.Set("Origin", c.origin)
	}
	if c.principal != nil {
		r = r.WithContext(auth.ContextWithPrincipal(r.Context(), *c.principal))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func sessionPrincipal(sessionToken string) *auth.Principal {
	p := auth.PrincipalWithCSRF(auth.Principal{Kind: auth.PrincipalSession, UserID: "u1"}, auth.CSRFToken(sessionToken))
	return &p
}

func TestCSRFRejectsUnsafeCookieRequestWithoutTokenWith403(t *testing.T) {
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		rec := csrfCase{method: m, principal: sessionPrincipal("s")}.run(t)

		assert.Equal(t, http.StatusForbidden, rec.Code, m)
		assert.Contains(t, rec.Body.String(), `"code":"csrf_token_invalid"`)
	}
}

func TestCSRFRejectsWrongToken(t *testing.T) {
	rec := csrfCase{method: "POST", principal: sessionPrincipal("s"), token: auth.CSRFToken("other-session")}.run(t)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestCSRFAcceptsTheSessionsToken(t *testing.T) {
	rec := csrfCase{method: "POST", principal: sessionPrincipal("s"), token: auth.CSRFToken("s")}.run(t)

	assert.Equal(t, http.StatusTeapot, rec.Code)
}

func TestCSRFNeverAcceptsAnEmptyToken(t *testing.T) {
	p := auth.PrincipalWithCSRF(auth.Principal{Kind: auth.PrincipalSession}, "")

	rec := csrfCase{method: "POST", principal: &p}.run(t)

	assert.Equal(t, http.StatusForbidden, rec.Code, "empty expected token must not match an empty header")
}

func TestCSRFExemptsSafeMethods(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
		rec := csrfCase{method: m, principal: sessionPrincipal("s")}.run(t)

		assert.Equal(t, http.StatusTeapot, rec.Code, m)
	}
}

func TestCSRFRejectsForeignOriginEvenWithAValidToken(t *testing.T) {
	rec := csrfCase{
		method: "POST", principal: sessionPrincipal("s"), token: auth.CSRFToken("s"),
		origin: "https://evil.test", host: "example.test",
	}.run(t)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"csrf_origin_invalid"`)
}

func TestCSRFAcceptsSameOriginAndAbsentOrigin(t *testing.T) {
	tok := auth.CSRFToken("s")
	same := csrfCase{method: "POST", principal: sessionPrincipal("s"), token: tok, origin: "https://Example.test:9494", host: "example.test:9494"}.run(t)
	absent := csrfCase{method: "POST", principal: sessionPrincipal("s"), token: tok, host: "example.test"}.run(t)

	assert.Equal(t, http.StatusTeapot, same.Code)
	assert.Equal(t, http.StatusTeapot, absent.Code)
}

func TestCSRFAcceptsOriginOnAnAllowedHostWhenTheRequestHostDiffers(t *testing.T) {
	rec := csrfCase{
		method: "POST", principal: sessionPrincipal("s"), token: auth.CSRFToken("s"),
		origin: "https://bitnews.lan", host: "10.0.0.5:9494", allowed: []string{"bitnews.lan"},
	}.run(t)

	assert.Equal(t, http.StatusTeapot, rec.Code)
}

func TestCSRFRejectsNullAndMalformedOrigins(t *testing.T) {
	for _, o := range []string{"null", "://bad", "ftp://example.test", "https://", "https://user@example.test", "example.test"} {
		rec := csrfCase{
			method: "POST", principal: sessionPrincipal("s"), token: auth.CSRFToken("s"),
			origin: o, host: "example.test",
		}.run(t)

		assert.Equal(t, http.StatusForbidden, rec.Code, "origin %q", o)
	}
}

func TestCSRFExemptsAPIKeyRequests(t *testing.T) {
	p := &auth.Principal{Kind: auth.PrincipalAPIKey}

	rec := csrfCase{method: "POST", principal: p, origin: "https://evil.test", host: "example.test"}.run(t)

	assert.Equal(t, http.StatusTeapot, rec.Code, "bearer credentials are not ambient, so CSRF does not apply")
}

func TestCSRFStillAppliesToBypassedAndAuthDisabledRequests(t *testing.T) {
	for _, kind := range []auth.PrincipalKind{auth.PrincipalLANBypass, auth.PrincipalAuthDisabled} {
		p := auth.PrincipalWithCSRF(auth.Principal{Kind: kind}, "anon-token")

		missing := csrfCase{method: "POST", principal: &p}.run(t)
		right := csrfCase{method: "POST", principal: &p, token: "anon-token"}.run(t)

		assert.Equal(t, http.StatusForbidden, missing.Code, kind)
		assert.Equal(t, http.StatusTeapot, right.Code, kind)
	}
}

func TestCSRFWithoutAPrincipalChecksOnlyTheOrigin(t *testing.T) {
	foreign := csrfCase{method: "POST", origin: "https://evil.test", host: "example.test"}.run(t)
	plain := csrfCase{method: "POST", host: "example.test"}.run(t)

	assert.Equal(t, http.StatusForbidden, foreign.Code, "login CSRF")
	assert.Equal(t, http.StatusTeapot, plain.Code)
}

func TestCSRFErrorBodyUsesTheAPIErrorShape(t *testing.T) {
	rec := csrfCase{method: "POST", principal: sessionPrincipal("s")}.run(t)

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.True(t, strings.HasPrefix(rec.Body.String(), `{"error":{`))
}
