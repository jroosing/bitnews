package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type adapterCredJSON struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Name       string  `json:"name"`
	Username   string  `json:"username"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	Key        string  `json:"key"`
}

func TestCreateAdapterCredentialShowsThePlaintextOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()

	created := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"Sonarr"}`, sess.mods()...)
	list := e.do("GET", "/api/v1/adapter-credentials", "", withCookie(sess.cookie))

	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var got adapterCredJSON
	decode(t, created, &got)
	assert.Regexp(t, `^bns_[a-z2-7]{52}$`, got.Key)
	assert.Equal(t, "adapter_sab", got.Kind)
	assert.Equal(t, "Sonarr", got.Name)
	assert.Empty(t, got.Username)
	assert.Equal(t, "no-store", created.Header().Get("Cache-Control"))
	require.Equal(t, http.StatusOK, list.Code)
	assert.NotContains(t, list.Body.String(), got.Key)
	assert.Contains(t, list.Body.String(), got.ID)
	assert.Contains(t, list.Body.String(), `"items"`)
}

func TestCreateQBitAdapterCredentialReturnsUsernameAndKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()

	rec := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_qbit","name":"My Radarr"}`, sess.mods()...)
	explicit := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_qbit","name":"Other","username":"tv"}`, sess.mods()...)
	taken := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_qbit","name":"Dup","username":"tv"}`, sess.mods()...)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var got adapterCredJSON
	decode(t, rec, &got)
	assert.Equal(t, "my-radarr", got.Username)
	assert.True(t, strings.HasPrefix(got.Key, "bnq_"))
	var ex adapterCredJSON
	decode(t, explicit, &ex)
	assert.Equal(t, "tv", ex.Username)
	require.Equal(t, http.StatusConflict, taken.Code)
	assert.Equal(t, "username_taken", errorOf(t, taken))
}

func TestRevokeAdapterCredential(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	var c adapterCredJSON
	decode(t, e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"x"}`, sess.mods()...), &c)

	rec := e.do("DELETE", "/api/v1/adapter-credentials/"+c.ID, "", sess.mods()...)
	again := e.do("DELETE", "/api/v1/adapter-credentials/"+c.ID, "", sess.mods()...)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, http.StatusNotFound, again.Code)
}

func TestCreateAdapterCredentialRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	for name, tc := range map[string]struct{ body, code string }{
		"blank name":    {`{"kind":"adapter_sab","name":"  "}`, "invalid_name"},
		"unknown kind":  {`{"kind":"api","name":"x"}`, "invalid_kind"},
		"no kind":       {`{"name":"x"}`, "invalid_kind"},
		"bad username":  {`{"kind":"adapter_qbit","name":"x","username":"Big Name"}`, "invalid_username"},
		"unknown field": {`{"kind":"adapter_sab","name":"x","id":"1"}`, "invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("POST", "/api/v1/adapter-credentials", tc.body, sess.mods()...)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
		})
	}
}

func TestAdapterCredentialRoutesRefuseAPIKeysAndAdapterCredentials(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	var c adapterCredJSON
	decode(t, e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"x"}`, sess.mods()...), &c)
	writeKey := e.newKey("write")
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/api/v1/adapter-credentials", ""},
		{"POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"y"}`},
		{"DELETE", "/api/v1/adapter-credentials/" + c.ID, ""},
	} {
		t.Run(rt.method, func(t *testing.T) {
			byKey := e.do(rt.method, rt.path, rt.body, withBearer(writeKey))
			byAdapter := e.do(rt.method, rt.path, rt.body, withBearer(c.Key))
			anonymous := e.do(rt.method, rt.path, rt.body)

			assert.Equal(t, http.StatusForbidden, byKey.Code, "an API key cannot manage adapter credentials")
			assert.Equal(t, http.StatusUnauthorized, byAdapter.Code, "an adapter credential is not an API key")
			assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
		})
	}
	noCSRF := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"y"}`, withCookie(sess.cookie))
	assert.Equal(t, http.StatusForbidden, noCSRF.Code)
}

func TestAdapterCredentialRoutesAcceptDisabledAuthPrincipalsButStillNeedCSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	require.Equal(t, http.StatusOK, e.do("PUT", "/api/v1/settings", openAuthJSON, sess.mods()...).Code)
	writeKey := e.newKey("write")
	var anon struct {
		CSRFToken string `json:"csrf_token"`
	}
	decode(t, e.do("GET", "/api/v1/auth/session", ""), &anon)

	noCSRF := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"Sonarr"}`)
	created := e.do("POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"Sonarr"}`,
		withHeader("X-CSRF-Token", anon.CSRFToken))
	list := e.do("GET", "/api/v1/adapter-credentials", "")
	byKey := e.do("GET", "/api/v1/adapter-credentials", "", withBearer(writeKey))

	assert.Equal(t, http.StatusForbidden, noCSRF.Code)
	assert.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	assert.Equal(t, http.StatusOK, list.Code)
	assert.Equal(t, http.StatusForbidden, byKey.Code, "an API key is refused even though the bypass would let it in")
	assert.Equal(t, "session_required", errorOf(t, byKey))
}
