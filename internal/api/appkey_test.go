package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheAppKeyOpensEveryOwnAPIRouteWithFullAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()

	assert.Regexp(t, `^bitnews_[a-z2-7]{52}$`, key)
	assert.Equal(t, http.StatusOK, e.do("GET", "/api/v1/downloads", "", withBearer(key)).Code)
	assert.Equal(t, http.StatusOK, e.do("GET", "/api/v1/downloads", "", withHeader("X-Api-Key", key)).Code)
	assert.Equal(t, http.StatusCreated, e.do("POST", "/api/v1/categories", `{"name":"tv"}`, withBearer(key)).Code, "write routes too")
	assert.Equal(t, http.StatusOK, e.do("GET", "/api/v1/settings", "", withBearer(key)).Code)
}

func TestRotatingTheKeyRejectsTheOldOneOnTheOwnAPIAtOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	old := e.apiKey()
	require.Equal(t, http.StatusOK, e.do("GET", "/api/v1/downloads", "", withBearer(old)).Code)

	fresh, err := e.appKey.Rotate(context.Background())
	require.NoError(t, err)

	assert.Equal(t, http.StatusUnauthorized, e.do("GET", "/api/v1/downloads", "", withBearer(old)).Code)
	assert.Equal(t, http.StatusOK, e.do("GET", "/api/v1/downloads", "", withBearer(fresh.Reveal())).Code)
}

func TestTheAPIHasNoKeyEndpoints(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	key := e.apiKey()

	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/api/v1/api-keys", ""},
		{"POST", "/api/v1/api-keys", `{"name":"x","scope":"write"}`},
		{"DELETE", "/api/v1/api-keys/x", ""},
		{"GET", "/api/v1/adapter-credentials", ""},
		{"POST", "/api/v1/adapter-credentials", `{"kind":"adapter_sab","name":"Sonarr"}`},
		{"DELETE", "/api/v1/adapter-credentials/x", ""},
		{"GET", "/api/v1/app-key", ""},
		{"POST", "/api/v1/app-key/rotate", ""},
	} {
		viaSession := e.do(rt.method, rt.path, rt.body, sess.mods()...)
		viaKey := e.do(rt.method, rt.path, rt.body, withBearer(key))

		assert.Equal(t, http.StatusNotFound, viaSession.Code, rt.method+" "+rt.path)
		assert.Equal(t, http.StatusNotFound, viaKey.Code, rt.method+" "+rt.path)
	}
}
