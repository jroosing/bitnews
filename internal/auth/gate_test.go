package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTeapot)
})

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestGateBlocksEverythingWith503SetupRequiredWhileSetupIsOpen(t *testing.T) {
	s, _, _ := newSetup(t, &fakeUsers{})
	h := auth.Gate(okHandler, s)

	for _, tc := range []struct{ method, target string }{
		{"GET", "/"},
		{"GET", "/api/v1/downloads"},
		{"POST", "/api/v1/auth/login"},
		{"GET", "/api/v1/setup"},   // only POST is exempt
		{"POST", "/api/v1/health"}, // only GET is exempt
		{"GET", "/api/v1/setup/extra"},
		{"POST", "/api/v1/setup/"},
		{"GET", "/api/v1/healthz"},
		{"GET", "/static"},
		{"GET", "/staticfoo/x.js"},
		{"GET", "/static/../api/v1/downloads"},
		{"GET", "/static/a/../../api/v1/downloads"},
		{"GET", "/static//..//api/v1/downloads"},
		{"GET", "/api/v1/events"},
	} {
		rec := do(h, tc.method, tc.target)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s %s", tc.method, tc.target)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		var body struct {
			Error struct{ Code, Message string }
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
		assert.Equal(t, "setup_required", body.Error.Code)
		assert.NotEmpty(t, body.Error.Message)
	}
}

func TestGateLetsSetupHealthAndStaticAssetsThroughWhileSetupIsOpen(t *testing.T) {
	s, _, _ := newSetup(t, &fakeUsers{})
	h := auth.Gate(okHandler, s)

	for _, tc := range []struct{ method, target string }{
		{"POST", "/api/v1/setup"},
		{"GET", "/api/v1/health"},
		{"GET", "/static/app.js"},
		{"GET", "/static/assets/css/app.css"},
		{"GET", "/static/"},
	} {
		rec := do(h, tc.method, tc.target)

		assert.Equal(t, http.StatusTeapot, rec.Code, "%s %s", tc.method, tc.target)
	}
}

func TestGatePassesEverythingOnceSetupIsDone(t *testing.T) {
	s, tok, _ := newSetup(t, &fakeUsers{})
	h := auth.Gate(okHandler, s)
	require.Equal(t, http.StatusServiceUnavailable, do(h, "GET", "/").Code)

	require.NoError(t, s.Complete(context.Background(), tok, "admin", goodPassword))

	for _, target := range []string{"/", "/api/v1/downloads", "/api/v1/setup"} {
		assert.Equal(t, http.StatusTeapot, do(h, "GET", target).Code, target)
	}
}

func TestGatePassesEverythingWhenAUserExistedAtStartup(t *testing.T) {
	users := &fakeUsers{users: []core.User{{ID: core.NewID(), Username: "admin"}}}
	logger, _ := newLogger()
	s, err := auth.NewSetup(context.Background(), users, logger)
	require.NoError(t, err)

	assert.Equal(t, http.StatusTeapot, do(auth.Gate(okHandler, s), "GET", "/").Code)
}

func TestGateDoesNotQueryTheRepoPerRequest(t *testing.T) {
	users := &countingUsers{}
	logger, _ := newLogger()
	s, err := auth.NewSetup(context.Background(), users, logger)
	require.NoError(t, err)
	h := auth.Gate(okHandler, s)

	for range 50 {
		do(h, "GET", "/api/v1/downloads")
	}

	assert.Equal(t, 1, users.counts, "only NewSetup counts")
}

type countingUsers struct {
	fakeUsers
	counts int
}

func (c *countingUsers) Count(ctx context.Context) (int, error) {
	c.counts++
	return c.fakeUsers.Count(ctx)
}

func TestGateBlockedBodyIsStableAndLeaksNoToken(t *testing.T) {
	s, _, _ := newSetup(t, &fakeUsers{})
	h := auth.Gate(okHandler, s)

	a := do(h, "GET", "/x").Body.String()
	b := do(h, "GET", "/y").Body.String()

	assert.Equal(t, a, b)
	assert.False(t, strings.Contains(a, "setup_token"))
}
