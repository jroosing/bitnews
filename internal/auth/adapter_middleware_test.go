package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestRequireAdapterCredentialAcceptsTheRightKindFromEveryPlace(t *testing.T) {
	e := newAdapterEnv(t)
	plain, info, err := e.svc.Create(context.Background(), auth.KindAdapterSAB, "Sonarr", "")
	require.NoError(t, err)
	var seen auth.Principal
	h := e.svc.RequireAdapterCredential(auth.KindAdapterSAB)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = auth.FromContext(r.Context())
		w.WriteHeader(http.StatusTeapot)
	}))

	for name, mod := range map[string]func(*http.Request){
		"query":     func(r *http.Request) { r.URL.RawQuery = "apikey=" + plain },
		"bearer":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+plain) },
		"x-api-key": func(r *http.Request) { r.Header.Set("X-Api-Key", plain) },
	} {
		r := httptest.NewRequest("GET", "/sabnzbd/api", nil)
		mod(r)
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, r)

		assert.Equal(t, http.StatusTeapot, rec.Code, name)
		assert.Equal(t, auth.PrincipalAdapter, seen.Kind, name)
		assert.Equal(t, info.ID, seen.KeyID, name)
	}
}

func TestRequireAdapterCredentialRejectsEveryOtherCredential(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()
	qbit, _, err := e.svc.Create(ctx, auth.KindAdapterQBit, "Radarr", "radarr")
	require.NoError(t, err)
	apiKeys := newKeyEnv(t)
	apiKey, _ := apiKeys.create(t, core.APIKeyScopeWrite)
	session, _, _ := auth.NewToken()
	h := e.svc.RequireAdapterCredential(auth.KindAdapterSAB)(okHandler)

	for name, key := range map[string]string{"none": "", "qbit key": qbit, "api key": apiKey, "session token": session, "garbage": "x"} {
		r := httptest.NewRequest("GET", "/sabnzbd/api", nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, r)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.NotContains(t, rec.Body.String(), key+"x")
	}
}

func TestRequireAdapterCredentialIgnoresTheSessionAndBypassPrincipals(t *testing.T) {
	e := newAdapterEnv(t)
	h := e.svc.RequireAdapterCredential(auth.KindAdapterSAB)(okHandler)
	r := httptest.NewRequest("GET", "/sabnzbd/api", nil)
	r = r.WithContext(auth.ContextWithPrincipal(r.Context(), auth.Principal{Kind: auth.PrincipalAuthDisabled}))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusUnauthorized, rec.Code, "disable-auth and LAN bypass never apply to adapters")
}

func TestRequireAdapterCredentialCustomRejection(t *testing.T) {
	e := newAdapterEnv(t)
	var gotErr error
	h := e.svc.RequireAdapterCredential(auth.KindAdapterSAB,
		auth.WithAdapterKey(func(r *http.Request) string { return r.URL.Query().Get("k") }),
		auth.WithAdapterRejection(func(w http.ResponseWriter, _ *http.Request, err error) {
			gotErr = err
			w.WriteHeader(http.StatusTeapot)
		}))(okHandler)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/?k=nope", nil))

	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.ErrorIs(t, gotErr, auth.ErrInvalidAdapterCredential)
}

func TestUIPrincipalOnlyAllowsSessionBypassAndDisabledButNotAPIKeys(t *testing.T) {
	h := auth.UIPrincipalOnly(okHandler)
	with := func(k auth.PrincipalKind) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/adapter-credentials", nil)
		if k != "" {
			r = r.WithContext(auth.ContextWithPrincipal(r.Context(), auth.Principal{Kind: k}))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	assert.Equal(t, http.StatusTeapot, with(auth.PrincipalSession).Code)
	assert.Equal(t, http.StatusTeapot, with(auth.PrincipalLANBypass).Code)
	assert.Equal(t, http.StatusTeapot, with(auth.PrincipalAuthDisabled).Code)
	assert.Equal(t, http.StatusUnauthorized, with("").Code, "no principal")
	forbidden := with(auth.PrincipalAPIKey)
	assert.Equal(t, http.StatusForbidden, forbidden.Code)
	assert.Equal(t, "session_required", apiErrCode(t, forbidden))
	assert.Equal(t, http.StatusForbidden, with(auth.PrincipalAdapter).Code, "an adapter credential cannot mint credentials")
}
