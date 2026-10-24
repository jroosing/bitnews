package auth_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

type appKeyEnv struct {
	svc  *auth.AppKeyService
	repo *fakeAppKeys
	log  *bytes.Buffer
}

func newAppKeyEnv(t *testing.T, repo *fakeAppKeys) appKeyEnv {
	t.Helper()
	e := appKeyEnv{repo: repo, log: &bytes.Buffer{}}
	var err error
	e.svc, err = auth.NewAppKeyService(context.Background(), e.repo,
		auth.WithAppKeyLogger(slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	require.NoError(t, err)
	return e
}

func TestFirstStartGeneratesAPrefixedKeyAndStoresIt(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})

	key := e.svc.Current().Reveal()

	assert.Regexp(t, `^bitnews_[a-z2-7]{52}$`, key)
	assert.Equal(t, key, e.repo.stored().Reveal())
}

func TestARestartKeepsTheStoredKey(t *testing.T) {
	repo := &fakeAppKeys{}
	first := newAppKeyEnv(t, repo).svc.Current()

	second := newAppKeyEnv(t, repo).svc.Current()

	assert.Equal(t, first, second)
}

func TestAStoredKeyOfTheWrongShapeIsReplacedByANewOne(t *testing.T) {
	repo := &fakeAppKeys{key: "not-a-key", set: true}
	e := newAppKeyEnv(t, repo)

	assert.Regexp(t, `^bitnews_[a-z2-7]{52}$`, e.svc.Current().Reveal())
	assert.Equal(t, e.svc.Current(), repo.stored())
	assert.Contains(t, e.log.String(), "malformed")
	assert.NotContains(t, e.log.String(), "not-a-key")
}

func TestStartingFailsWhenTheStoreFails(t *testing.T) {
	boom := errors.New("disk on fire")

	_, err := auth.NewAppKeyService(context.Background(), &fakeAppKeys{getErr: boom})
	assert.ErrorIs(t, err, boom)

	_, err = auth.NewAppKeyService(context.Background(), &fakeAppKeys{setErr: boom})
	assert.ErrorIs(t, err, boom)
}

func TestAuthenticateAcceptsOnlyTheCurrentKeyAndNeverReadsTheStore(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	key := e.svc.Current().Reveal()
	reads := e.repo.getCount()

	assert.True(t, e.svc.Authenticate(key))
	for name, bad := range map[string]string{
		"empty":     "",
		"truncated": key[:len(key)-1],
		"extended":  key + "a",
		"one char":  key[:len(key)-1] + otherChar(key[len(key)-1]),
		"no prefix": key[len("bitnews_"):],
		"upper":     "BITNEWS_" + key[len("bitnews_"):],
	} {
		assert.False(t, e.svc.Authenticate(bad), name)
	}
	assert.Equal(t, reads, e.repo.getCount(), "the key is compared in memory")
}

func otherChar(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}

func TestRotateMakesANewKeyAndRejectsTheOldOneAtOnce(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	old := e.svc.Current().Reveal()
	gen := e.svc.Generation()

	got, err := e.svc.Rotate(context.Background())

	require.NoError(t, err)
	assert.NotEqual(t, old, got.Reveal())
	assert.Regexp(t, `^bitnews_[a-z2-7]{52}$`, got.Reveal())
	assert.Equal(t, got, e.svc.Current())
	assert.Equal(t, got, e.repo.stored(), "persisted")
	assert.False(t, e.svc.Authenticate(old))
	assert.True(t, e.svc.Authenticate(got.Reveal()))
	assert.NotEqual(t, gen, e.svc.Generation(), "sessions bound to the old key can tell")
}

func TestAFailedRotateKeepsTheOldKeyWorking(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	old := e.svc.Current()
	e.repo.setErr = errors.New("disk full")
	gen := e.svc.Generation()

	_, err := e.svc.Rotate(context.Background())

	require.Error(t, err)
	assert.Equal(t, old, e.svc.Current())
	assert.True(t, e.svc.Authenticate(old.Reveal()))
	assert.Equal(t, gen, e.svc.Generation())
}

func TestTheQBitLoginIsTheFixedUsernameAndTheKey(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	key := e.svc.Current().Reveal()

	assert.Equal(t, "bitnews", auth.QBitUsername)
	assert.True(t, e.svc.AuthenticateQBit("bitnews", key))
	assert.False(t, e.svc.AuthenticateQBit("radarr", key), "wrong username")
	assert.False(t, e.svc.AuthenticateQBit("", key))
	assert.False(t, e.svc.AuthenticateQBit("bitnews", "bitnews_wrong"))
	assert.False(t, e.svc.AuthenticateQBit("bitnews", ""))
}

func TestTheKeyNeverAppearsInLogsErrorsOrFormattedOutput(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	key := e.svc.Current()
	old := key.Reveal()
	e.svc.Authenticate("bitnews_not-it")
	newKey, err := e.svc.Rotate(context.Background())
	require.NoError(t, err)
	h := e.svc.KeyOr(denyAll)(okHandler)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+old)
	h.ServeHTTP(rec, r)

	for _, s := range []string{
		e.log.String(), rec.Body.String(),
		fmt.Sprintf("%v %+v %#v %s", key, newKey, e.svc.Current(), e.svc.Current()),
	} {
		assert.NotContains(t, s, old)
		assert.NotContains(t, s, newKey.Reveal())
	}
}

// --- middleware ---

func denyAll(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func (e appKeyEnv) request(h http.Handler, method string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/downloads", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func bearer(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }

func TestKeyOrAcceptsBearerAndXAPIKeyForEveryMethod(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	key := e.svc.Current().Reveal()
	h := e.svc.KeyOr(denyAll)(okHandler)

	for name, hdr := range map[string]map[string]string{
		"bearer":    bearer(key),
		"bearer ci": {"Authorization": "bEaReR " + key},
		"x-api-key": {"X-Api-Key": key},
	} {
		for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"} {
			assert.Equal(t, http.StatusTeapot, e.request(h, m, hdr).Code, name+" "+m)
		}
	}
}

func TestKeyOrRejectsAWrongKeyWith401AndNeverFallsBack(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	h := e.svc.KeyOr(func(next http.Handler) http.Handler { return next })(okHandler) // a fallback that lets everything in

	for name, hdr := range map[string]map[string]string{
		"unknown":   bearer("bitnews_wrong"),
		"empty":     {"Authorization": "Bearer "},
		"x-api-key": {"X-Api-Key": "bitnews_wrong"},
	} {
		rec := e.request(h, "GET", hdr)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.Equal(t, "unauthorized", apiErrCode(t, rec), name)
		assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"), name)
	}
	assert.Equal(t, http.StatusTeapot, e.request(h, "GET", map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}).Code,
		"another scheme is no key: the UI chain decides")
}

func TestKeyOrPutsTheKeyPrincipalInTheContext(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	var got auth.Principal
	h := e.svc.KeyOr(denyAll)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = auth.FromContext(r.Context())
	}))

	e.request(h, "GET", bearer(e.svc.Current().Reveal()))

	assert.Equal(t, auth.Principal{Kind: auth.PrincipalAPIKey}, got)
}

func TestARotatedKeyIsRejectedByTheMiddlewareOnTheNextRequest(t *testing.T) {
	e := newAppKeyEnv(t, &fakeAppKeys{})
	old := e.svc.Current().Reveal()
	h := e.svc.KeyOr(denyAll)(okHandler)
	require.Equal(t, http.StatusTeapot, e.request(h, "GET", bearer(old)).Code)

	fresh, err := e.svc.Rotate(context.Background())
	require.NoError(t, err)

	assert.Equal(t, http.StatusUnauthorized, e.request(h, "GET", bearer(old)).Code)
	assert.Equal(t, http.StatusTeapot, e.request(h, "GET", bearer(fresh.Reveal())).Code)
}

func TestKeyOrFallsBackToTheUIChainOnlyWhenNoKeyIsSent(t *testing.T) {
	u := newUIEnv(t)
	u.settings.Auth = core.Auth{DisableAuth: true, AllowedHosts: []string{"bitnews.lan"}}
	k := newAppKeyEnv(t, &fakeAppKeys{})
	key := k.svc.Current().Reveal()
	var kinds []auth.PrincipalKind
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		kinds = append(kinds, p.Kind)
		w.WriteHeader(http.StatusTeapot)
	})
	h := k.svc.KeyOr(u.ui.Middleware)(inner)
	do := func(hdr map[string]string) int {
		r := httptest.NewRequest("GET", "http://bitnews.lan/api/v1/downloads", nil)
		r.RemoteAddr = "203.0.113.50:1"
		for n, v := range hdr {
			r.Header.Set(n, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	assert.Equal(t, http.StatusTeapot, do(bearer(key)))
	assert.Equal(t, http.StatusTeapot, do(nil), "no key: UI chain, which allows anonymous here")
	assert.Equal(t, []auth.PrincipalKind{auth.PrincipalAPIKey, auth.PrincipalAuthDisabled}, kinds)
	assert.Equal(t, http.StatusUnauthorized, do(bearer("bitnews_wrong")), "a bad key is never rescued by the bypass")
}
