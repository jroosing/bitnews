package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

type apiKeyJSON struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Scope      string  `json:"scope"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	Key        string  `json:"key"`
}

func TestCreateAPIKeyShowsThePlaintextOnceAndTheKeyWorks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()

	created := e.do("POST", "/api/v1/api-keys", `{"name":"sonarr","scope":"read"}`, sess.mods()...)
	list := e.do("GET", "/api/v1/api-keys", "", withCookie(sess.cookie))

	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var got apiKeyJSON
	decode(t, created, &got)
	assert.True(t, strings.HasPrefix(got.Key, "bn_"))
	assert.Equal(t, "sonarr", got.Name)
	assert.Equal(t, "read", got.Scope)
	assert.Equal(t, "no-store", created.Header().Get("Cache-Control"))
	assert.Equal(t, http.StatusOK, e.do("GET", "/api/v1/downloads", "", withBearer(got.Key)).Code)
	require.Equal(t, http.StatusOK, list.Code)
	assert.NotContains(t, list.Body.String(), got.Key)
	assert.Contains(t, list.Body.String(), got.ID)
	assert.Contains(t, list.Body.String(), `"items"`)
}

func TestRevokeAPIKeyStopsItWorking(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	var k apiKeyJSON
	decode(t, e.do("POST", "/api/v1/api-keys", `{"name":"x","scope":"write"}`, sess.mods()...), &k)

	rec := e.do("DELETE", "/api/v1/api-keys/"+k.ID, "", sess.mods()...)
	again := e.do("DELETE", "/api/v1/api-keys/"+k.ID, "", sess.mods()...)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, http.StatusNotFound, again.Code)
	assert.Equal(t, http.StatusUnauthorized, e.do("GET", "/api/v1/downloads", "", withBearer(k.Key)).Code)
}

func TestCreateAPIKeyRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	for name, tc := range map[string]struct{ body, code string }{
		"blank name":    {`{"name":"  ","scope":"read"}`, "invalid_name"},
		"unknown scope": {`{"name":"x","scope":"admin"}`, "invalid_scope"},
		"no scope":      {`{"name":"x"}`, "invalid_scope"},
		"unknown field": {`{"name":"x","scope":"read","id":"1"}`, "invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("POST", "/api/v1/api-keys", tc.body, sess.mods()...)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
		})
	}
}

func TestAPIKeyRoutesRefuseAPIKeysAnonymousCallersAndMissingCSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	writeKey := e.newKey(core.APIKeyScopeWrite)
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/api/v1/api-keys", ""},
		{"POST", "/api/v1/api-keys", `{"name":"x","scope":"read"}`},
		{"DELETE", "/api/v1/api-keys/x", ""},
	} {
		t.Run(rt.method, func(t *testing.T) {
			byKey := e.do(rt.method, rt.path, rt.body, withBearer(writeKey))
			anonymous := e.do(rt.method, rt.path, rt.body)

			assert.Equal(t, http.StatusUnauthorized, byKey.Code, "a key must not manage keys")
			assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
		})
	}
	noCSRF := e.do("POST", "/api/v1/api-keys", `{"name":"x","scope":"read"}`, withCookie(sess.cookie))
	assert.Equal(t, http.StatusForbidden, noCSRF.Code)
}

func TestAPIKeyRoutesRefuseRequestsThatAuthenticateWithoutALogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	open := `{"queues":{"usenet":1,"torrent":5},
"auth":{"disable_auth":true,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":["example.com"]},
"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}}}`
	require.Equal(t, http.StatusOK, e.do("PUT", "/api/v1/settings", open, sess.mods()...).Code)

	rec := e.do("GET", "/api/v1/api-keys", "")

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "session_required", errorOf(t, rec))
}
