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
	unknown, _, err := auth.NewKindToken(auth.KindAppKey)
	require.NoError(t, err)
	session, _, err := auth.NewToken()
	require.NoError(t, err)

	for _, key := range []string{"nope", unknown, session, s.key + "x", strings.ToUpper(s.key), strings.Repeat("a", 100_000)} {
		rec := s.callWith(key, "mode=queue")

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, rec.Body.String())
	}
}

func TestRefreshingTheKeyTakesEffectImmediately(t *testing.T) {
	s := newStack(t)
	assert.Contains(t, s.call("mode=get_cats").Body.String(), `"categories"`)

	fresh, err := s.appKey.Refresh(context.Background())
	require.NoError(t, err)

	assert.JSONEq(t, `{"status":false,"error":"API Key Incorrect"}`, s.call("mode=get_cats").Body.String(), "the old key")
	assert.Contains(t, s.callWith(fresh.Reveal(), "mode=get_cats").Body.String(), `"categories"`)
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
