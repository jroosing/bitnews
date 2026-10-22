package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

type keyEnv struct {
	svc   *auth.APIKeyService
	repo  *fakeAPIKeys
	clock *fakeClock
	log   *bytes.Buffer
}

func newKeyEnv(t *testing.T) keyEnv {
	t.Helper()
	e := keyEnv{repo: &fakeAPIKeys{}, clock: newClock(), log: &bytes.Buffer{}}
	e.svc = auth.NewAPIKeyService(e.repo,
		auth.WithAPIKeyClock(e.clock.Now),
		auth.WithAPIKeyLogger(slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	return e
}

func (e keyEnv) create(t *testing.T, scope core.APIKeyScope) (string, auth.APIKeyInfo) {
	t.Helper()
	plain, info, err := e.svc.Create(context.Background(), "script", scope)
	require.NoError(t, err)
	return plain, info
}

func TestAPIKeyCreateReturnsPrefixedKeyAndStoresOnlyItsHash(t *testing.T) {
	e := newKeyEnv(t)

	plain, info := e.create(t, core.APIKeyScopeRead)

	assert.Regexp(t, `^bn_[a-z2-7]{52}$`, plain)
	assert.Equal(t, "script", info.Name)
	assert.Equal(t, core.APIKeyScopeRead, info.Scope)
	assert.Equal(t, e.clock.Now(), info.CreatedAt)
	assert.Nil(t, info.LastUsedAt)
	require.Len(t, e.repo.keys, 1)
	assert.Equal(t, auth.HashToken(plain), e.repo.keys[0].Hash)
	assert.Equal(t, info.ID, e.repo.keys[0].ID)
}

func TestAPIKeyCreateValidatesNameAndScope(t *testing.T) {
	e := newKeyEnv(t)
	ctx := context.Background()

	for _, name := range []string{"", "   ", "bad\nname", strings.Repeat("x", 101)} {
		_, _, err := e.svc.Create(ctx, name, core.APIKeyScopeRead)
		assert.ErrorIs(t, err, auth.ErrInvalidAPIKeyName, "name %q", name)
	}
	_, _, err := e.svc.Create(ctx, "ok", "admin")
	assert.ErrorIs(t, err, auth.ErrInvalidAPIKeyScope)
	assert.Empty(t, e.repo.keys)
}

func TestAPIKeyCreateTrimsTheName(t *testing.T) {
	e := newKeyEnv(t)

	_, info, err := e.svc.Create(context.Background(), "  sonarr  ", core.APIKeyScopeWrite)

	require.NoError(t, err)
	assert.Equal(t, "sonarr", info.Name)
}

func TestAPIKeyListNeverExposesHashesOrPlaintext(t *testing.T) {
	e := newKeyEnv(t)
	plain, _ := e.create(t, core.APIKeyScopeWrite)

	list, err := e.svc.List(context.Background())
	require.NoError(t, err)
	raw, err := json.Marshal(list)
	require.NoError(t, err)

	require.Len(t, list, 1)
	assert.NotContains(t, string(raw), plain)
	assert.NotContains(t, string(raw), "hash")
	assert.JSONEq(t, `[{"id":"`+string(list[0].ID)+`","name":"script","scope":"write","created_at":"`+
		e.clock.Now().Format(time.RFC3339Nano)+`","last_used_at":null}]`, string(raw))
}

func TestAPIKeyCreateUseRevoke(t *testing.T) {
	e := newKeyEnv(t)
	ctx := context.Background()
	plain, info := e.create(t, core.APIKeyScopeWrite)

	got, err := e.svc.Authenticate(ctx, plain)
	require.NoError(t, err)
	assert.Equal(t, info.ID, got.ID)

	require.NoError(t, e.svc.Revoke(ctx, info.ID))
	_, err = e.svc.Authenticate(ctx, plain)

	assert.ErrorIs(t, err, auth.ErrInvalidAPIKey, "revocation is immediate")
	assert.ErrorIs(t, e.svc.Revoke(ctx, info.ID), core.ErrNotFound)
}

func TestAPIKeyAuthenticateRejectsUnknownMalformedAndForeignKinds(t *testing.T) {
	e := newKeyEnv(t)
	e.create(t, core.APIKeyScopeRead)
	session, _, _ := auth.NewToken()
	sab, _, _ := auth.NewKindToken(auth.KindAdapterSAB)
	unknown, _, _ := auth.NewKindToken(auth.KindAPI)

	for _, k := range []string{"", "bn_", "bn_short", session, sab, unknown, strings.Repeat("a", 100_000)} {
		_, err := e.svc.Authenticate(context.Background(), k)

		assert.ErrorIs(t, err, auth.ErrInvalidAPIKey)
	}
}

func TestAPIKeyAuthenticateComparesTheHashItselfNotJustTheLookup(t *testing.T) {
	e := newKeyEnv(t)
	e.create(t, core.APIKeyScopeWrite)
	e.repo.wrongHash = true // a faulty repository answers with the wrong row
	other, _, _ := auth.NewKindToken(auth.KindAPI)

	_, err := e.svc.Authenticate(context.Background(), other)

	assert.ErrorIs(t, err, auth.ErrInvalidAPIKey)
}

func TestAPIKeyLastUsedIsUpdatedAtMostOncePerMinute(t *testing.T) {
	e := newKeyEnv(t)
	ctx := context.Background()
	plain, _ := e.create(t, core.APIKeyScopeRead)

	first, err := e.svc.Authenticate(ctx, plain)
	require.NoError(t, err)
	require.NotNil(t, first.LastUsedAt)
	assert.Equal(t, e.clock.Now(), *first.LastUsedAt)
	assert.Equal(t, 1, e.repo.touchCount())

	e.clock.Advance(59 * time.Second)
	_, _ = e.svc.Authenticate(ctx, plain)
	assert.Equal(t, 1, e.repo.touchCount(), "inside the minute")

	e.clock.Advance(time.Second)
	_, _ = e.svc.Authenticate(ctx, plain)
	assert.Equal(t, 2, e.repo.touchCount())
}

func TestAPIKeyNeverAppearsInLogsOrErrors(t *testing.T) {
	e := newKeyEnv(t)
	plain, _ := e.create(t, core.APIKeyScopeRead)
	bad := plain[:len(plain)-1] + "x"
	if bad == plain {
		bad = plain[:len(plain)-1] + "y"
	}

	_, err := e.svc.Authenticate(context.Background(), bad)
	_, _ = e.svc.Authenticate(context.Background(), plain)
	h := e.svc.RequireScope(core.APIKeyScopeRead)(okHandler)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+bad)
	h.ServeHTTP(rec, r)

	require.Error(t, err)
	for _, s := range []string{err.Error(), e.log.String(), rec.Body.String()} {
		assert.NotContains(t, s, plain)
		assert.NotContains(t, s, bad)
	}
}

// --- middleware ---

func (e keyEnv) request(h http.Handler, method string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/downloads", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func bearer(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }

func TestRequireScopeAcceptsBearerAndXAPIKeyHeaders(t *testing.T) {
	e := newKeyEnv(t)
	plain, _ := e.create(t, core.APIKeyScopeRead)
	h := e.svc.RequireScope(core.APIKeyScopeRead)(okHandler)

	for name, hdr := range map[string]map[string]string{
		"bearer":    bearer(plain),
		"bearer ci": {"Authorization": "bEaReR " + plain},
		"x-api-key": {"X-Api-Key": plain},
	} {
		assert.Equal(t, http.StatusTeapot, e.request(h, "GET", hdr).Code, name)
	}
}

func TestRequireScopeRejectsMissingAndInvalidCredentialsWith401(t *testing.T) {
	e := newKeyEnv(t)
	e.create(t, core.APIKeyScopeRead)
	other, _, _ := auth.NewKindToken(auth.KindAPI)
	h := e.svc.RequireScope(core.APIKeyScopeRead)(okHandler)

	for name, hdr := range map[string]map[string]string{
		"none":      nil,
		"unknown":   bearer(other),
		"basic":     {"Authorization": "Basic Zm9vOmJhcg=="},
		"x-api-key": {"X-Api-Key": other},
		"empty":     {"Authorization": "Bearer "},
	} {
		rec := e.request(h, "GET", hdr)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.Equal(t, "unauthorized", apiErrCode(t, rec), name)
		assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestReadScopeKeyMayOnlyUseSafeMethods(t *testing.T) {
	e := newKeyEnv(t)
	plain, _ := e.create(t, core.APIKeyScopeRead)
	h := e.svc.RequireScope(core.APIKeyScopeRead)(okHandler)

	for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
		assert.Equal(t, http.StatusTeapot, e.request(h, m, bearer(plain)).Code, m)
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		rec := e.request(h, m, bearer(plain))

		assert.Equal(t, http.StatusForbidden, rec.Code, m)
		assert.Equal(t, "insufficient_scope", apiErrCode(t, rec))
	}
}

func TestWriteScopeKeyMayUseEveryMethod(t *testing.T) {
	e := newKeyEnv(t)
	plain, _ := e.create(t, core.APIKeyScopeWrite)
	h := e.svc.RequireScope(core.APIKeyScopeWrite)(okHandler)

	for _, m := range []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"} {
		assert.Equal(t, http.StatusTeapot, e.request(h, m, bearer(plain)).Code, m)
	}
}

func TestRouteRequiringWriteRejectsReadKeyEvenForGET(t *testing.T) {
	e := newKeyEnv(t)
	read, _ := e.create(t, core.APIKeyScopeRead)
	h := e.svc.RequireScope(core.APIKeyScopeWrite)(okHandler)

	assert.Equal(t, http.StatusForbidden, e.request(h, "GET", bearer(read)).Code)
}

func TestRequireScopePutsTheKeyPrincipalInTheContext(t *testing.T) {
	e := newKeyEnv(t)
	plain, info := e.create(t, core.APIKeyScopeWrite)
	var got auth.Principal
	h := e.svc.RequireScope(core.APIKeyScopeRead)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = auth.FromContext(r.Context())
	}))

	e.request(h, "GET", bearer(plain))

	assert.Equal(t, auth.Principal{Kind: auth.PrincipalAPIKey, Scope: core.APIKeyScopeWrite, KeyID: info.ID}, got)
}

func TestRevokedKeyStopsWorkingOnTheNextRequest(t *testing.T) {
	e := newKeyEnv(t)
	plain, info := e.create(t, core.APIKeyScopeWrite)
	h := e.svc.RequireScope(core.APIKeyScopeRead)(okHandler)
	require.Equal(t, http.StatusTeapot, e.request(h, "GET", bearer(plain)).Code)

	require.NoError(t, e.svc.Revoke(context.Background(), info.ID))

	assert.Equal(t, http.StatusUnauthorized, e.request(h, "GET", bearer(plain)).Code)
}

func TestAuthBypassSettingsNeverApplyToKeyRoutes(t *testing.T) {
	u := newUIEnv(t)
	u.settings.Auth = core.Auth{DisableAuth: true, LANBypassCIDRs: []string{"0.0.0.0/0"}, AllowedHosts: []string{"bitnews.lan"}}
	k := newKeyEnv(t)
	h := k.svc.RequireScope(core.APIKeyScopeRead)(okHandler)
	require.Equal(t, http.StatusTeapot, u.do(uiReq{host: "bitnews.lan"}).Code, "the UI is open")

	rec := k.request(h, "GET", nil)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// --- composition with the UI chain ---

func TestKeyOrFallsBackToTheUIChainOnlyWhenNoKeyIsSent(t *testing.T) {
	u := newUIEnv(t)
	u.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}
	k := newKeyEnv(t)
	plain, _ := k.create(t, core.APIKeyScopeRead)
	var kinds []auth.PrincipalKind
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		kinds = append(kinds, p.Kind)
		w.WriteHeader(http.StatusTeapot)
	})
	h := k.svc.KeyOr(core.APIKeyScopeRead, u.ui.Middleware)(inner)
	do := func(method string, hdr map[string]string) int {
		r := httptest.NewRequest(method, "http://bitnews.lan/api/v1/downloads", nil)
		r.RemoteAddr = "203.0.113.50:1"
		for n, v := range hdr {
			r.Header.Set(n, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	assert.Equal(t, http.StatusTeapot, do("GET", bearer(plain)))
	assert.Equal(t, http.StatusTeapot, do("GET", nil), "no key: UI chain, which allows anonymous here")
	assert.Equal(t, []auth.PrincipalKind{auth.PrincipalAPIKey, auth.PrincipalAuthDisabled}, kinds)

	other, _, _ := auth.NewKindToken(auth.KindAPI)
	assert.Equal(t, http.StatusUnauthorized, do("GET", bearer(other)), "a bad key is never rescued by the bypass")
	assert.Equal(t, http.StatusForbidden, do("POST", bearer(plain)), "scope still enforced")
}

func TestSessionOnlyRejectsAPIKeysBypassAndAnonymous(t *testing.T) {
	u := newUIEnv(t)
	k := newKeyEnv(t)
	plain, _ := k.create(t, core.APIKeyScopeWrite)
	token := u.login(t)
	guarded := auth.SessionOnly(okHandler)
	h := k.svc.KeyOr(core.APIKeyScopeWrite, func(next http.Handler) http.Handler {
		return u.ui.Middleware(auth.CSRF(func() core.Settings { return u.settings })(next))
	})(guarded)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		r.Host = "bitnews.lan"
		r.RemoteAddr = "203.0.113.50:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	viaKey := httptest.NewRequest("POST", "/api/v1/api-keys", nil)
	viaKey.Header.Set("Authorization", "Bearer "+plain)
	rec := do(viaKey)
	assert.Equal(t, http.StatusForbidden, rec.Code, "an API key cannot manage API keys")
	assert.Equal(t, "session_required", apiErrCode(t, rec))

	viaSession := httptest.NewRequest("POST", "/api/v1/api-keys", nil)
	viaSession.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	viaSession.Header.Set("X-CSRF-Token", auth.CSRFToken(token))
	assert.Equal(t, http.StatusTeapot, do(viaSession).Code)

	u.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}
	anon := httptest.NewRequest("GET", "/api/v1/api-keys", nil)
	assert.Equal(t, http.StatusForbidden, do(anon).Code, "disable-auth cannot mint credentials")
}

func TestSessionOnlyWithoutAPrincipalIs401(t *testing.T) {
	rec := httptest.NewRecorder()

	auth.SessionOnly(okHandler).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
