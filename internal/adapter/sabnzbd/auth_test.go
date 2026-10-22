package sabnzbd_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/sabnzbd"
	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

func TestVersionNeedsNoKey(t *testing.T) {
	s := newStack(t)

	rec := s.callWith("", "mode=version")

	assert.JSONEq(t, `{"version":"`+sabnzbd.DefaultVersion+`"}`, rec.Body.String())
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}

func TestVersionIsConfigurable(t *testing.T) {
	s := newStack(t, withDeps(func(d *sabnzbd.Deps) { d.Version = "3.7.2" }))

	rec := s.callWith("", "mode=version")

	assert.JSONEq(t, `{"version":"3.7.2"}`, rec.Body.String())
}

func TestEveryOtherModeWithoutAKeyIsAPIKeyRequiredWithHTTP200(t *testing.T) {
	s := newStack(t)
	for _, mode := range []string{"get_config", "get_cats", "fullstatus", "status", "queue", "history", "addurl", "addfile", "retry", "nonsense", ""} {
		rec := s.callWith("", "mode="+mode)

		assert.Equal(t, http.StatusOK, rec.Code, mode)
		assert.JSONEq(t, `{"status":false,"error":"API Key Required"}`, rec.Body.String(), mode)
	}
}

func TestAWrongOrMalformedKeyIsAPIKeyIncorrect(t *testing.T) {
	s := newStack(t)
	unknown, _, err := auth.NewKindToken(auth.KindAdapterSAB)
	require.NoError(t, err)

	for _, key := range []string{"nope", unknown, s.key + "x", strings.ToUpper(s.key), strings.Repeat("a", 100_000)} {
		rec := s.callWith(key, "mode=queue")

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, rec.Body.String())
	}
}

func TestKeysOfOtherKindsAreRefused(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	qbit, _, err := s.creds.Create(ctx, auth.KindAdapterQBit, "Radarr", "radarr")
	require.NoError(t, err)
	apiKeys := auth.NewAPIKeyService(store.NewAPIKeyRepo(s.db))
	apiKey, _, err := apiKeys.Create(ctx, "script", "write")
	require.NoError(t, err)
	session, _, err := auth.NewToken()
	require.NoError(t, err)

	for name, key := range map[string]string{"qbit credential": qbit, "own API key": apiKey, "session token": session} {
		rec := s.callWith(key, "mode=queue")

		assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, rec.Body.String(), name)
	}
}

func TestRevokingAKeyTakesEffectImmediately(t *testing.T) {
	s := newStack(t)
	assert.Contains(t, s.call("mode=get_cats").Body.String(), `"categories"`)
	list, err := s.creds.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)

	require.NoError(t, s.creds.Revoke(context.Background(), list[0].ID))

	assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, s.call("mode=get_cats").Body.String())
}

func TestEachMediaManagerHasItsOwnKey(t *testing.T) {
	s := newStack(t)
	radarr, info, err := s.creds.Create(context.Background(), auth.KindAdapterSAB, "Radarr", "")
	require.NoError(t, err)
	require.NoError(t, s.creds.Revoke(context.Background(), info.ID))

	assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, s.callWith(radarr, "mode=get_cats").Body.String())
	assert.Contains(t, s.call("mode=get_cats").Body.String(), `"categories"`, "Sonarr's key still works")
}

func TestAKeyUseIsRecordedAsLastUsed(t *testing.T) {
	s := newStack(t)

	s.call("mode=get_cats")

	list, err := s.creds.List(context.Background())
	require.NoError(t, err)
	require.NotNil(t, list[0].LastUsedAt)
	assert.True(t, list[0].LastUsedAt.Equal(s.clock.Now()), "%v", list[0].LastUsedAt)
}

func TestTheKeyMayComeInAFormBodyOrBeforeTheFileInAnUpload(t *testing.T) {
	s := newStack(t)
	form := httptest.NewRequest("POST", "/sabnzbd/api", strings.NewReader(url.Values{
		"mode": {"get_cats"}, "apikey": {s.key}, "output": {"json"},
	}.Encode()))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	assert.Contains(t, s.serve(form).Body.String(), `"categories"`)

	rec := s.serve(uploadRequest(t, "/sabnzbd/api?mode=addfile", "name", "x.nzb", tinyNZB, [2]string{"apikey", s.key}))
	assert.Contains(t, rec.Body.String(), `"nzo_ids"`)
}

func TestAnUploadWithoutAKeyIsRefusedBeforeTheNZBIsStaged(t *testing.T) {
	s := newStack(t)

	rec := s.serve(uploadRequest(t, "/sabnzbd/api?mode=addfile&output=json", "name", "x.nzb", tinyNZB))
	late := s.serve(uploadRequestFieldsLast(t, "/sabnzbd/api?mode=addfile&output=json", "x.nzb", tinyNZB, [2]string{"apikey", s.key}))

	assert.JSONEq(t, `{"status":false,"error":"API Key Required"}`, rec.Body.String())
	assert.JSONEq(t, `{"status":false,"error":"API Key Required"}`, late.Body.String(),
		"an apikey after the file is too late: the file is not read before the caller is known")
	assert.NoDirExists(t, s.incomplete, "nothing was written for an unauthenticated upload")
}

func TestAnUnknownModeIsNotImplementedAndLoggedAtDebug(t *testing.T) {
	s := newStack(t)

	rec := s.call("mode=frobnicate")

	assert.JSONEq(t, `{"status":false,"error":"not implemented"}`, rec.Body.String())
	assert.Contains(t, s.log.String(), "mode=frobnicate")
	assert.NotContains(t, s.log.String(), s.key)
}

func TestTheOutputParameterIsIgnoredAndTheAnswerIsAlwaysJSON(t *testing.T) {
	s := newStack(t)

	rec := s.serve(httptest.NewRequest("GET", "/sabnzbd/api?mode=version&output=xml", nil))

	assert.JSONEq(t, `{"version":"`+sabnzbd.DefaultVersion+`"}`, rec.Body.String())
}

func TestModeIsCaseInsensitive(t *testing.T) {
	s := newStack(t)

	assert.Contains(t, s.call("mode=GET_CATS").Body.String(), `"categories"`)
}
