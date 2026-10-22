package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

const settingsJSON = `{"queues":{"usenet":2,"torrent":4},
"auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},
"paths":{"usenet":{"incomplete":"/d/u/inc","complete":"/d/u/done"},"torrent":{"incomplete":"/d/t","complete":"/d/t"}},
"network":{"usenet":{"interface":"","address":"10.2.0.2"},"torrent":{"interface":"wg0","address":""}},
"seeding":{"action":"pause","ratio_limit":0,"seeding_time_minutes":0}}`

func TestGetSettingsReturnsDefaultsWithEmptyArraysNotNull(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("GET", "/api/v1/settings", "", withBearer(e.apiKey()))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"queues":{"usenet":1,"torrent":5},
"auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},
"paths":{"usenet":{"incomplete":"/data/usenet/incomplete","complete":"/data/usenet/complete"},"torrent":{"incomplete":"/data/torrents","complete":"/data/torrents"}},
"network":{"usenet":{"interface":"","address":""},"torrent":{"interface":"","address":""}},
"seeding":{"action":"pause","ratio_limit":0,"seeding_time_minutes":0}}`, rec.Body.String())
	assert.Equal(t, http.StatusUnauthorized, e.do("GET", "/api/v1/settings", "").Code)
}

func TestPutSettingsStoresThemPublishesSettingsChangedAndGetReturnsThem(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	events := e.subscribe()

	put := e.do("PUT", "/api/v1/settings", settingsJSON, withBearer(key))
	get := e.do("GET", "/api/v1/settings", "", withBearer(key))

	require.Equal(t, http.StatusOK, put.Code, put.Body.String())
	assert.JSONEq(t, settingsJSON, put.Body.String())
	assert.JSONEq(t, settingsJSON, get.Body.String())
	assert.Equal(t, []core.Event{core.SettingsChanged{}}, events())
}

func TestPutSettingsRejectsInvalidSettingsListingTheProblems(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	for name, tc := range map[string]struct{ body, code, contains string }{
		"negative limit": {`{"queues":{"usenet":-1,"torrent":5},"auth":{},"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}}}`,
			"invalid_settings", "usenet queue limit"},
		"relative path": {`{"queues":{"usenet":1,"torrent":5},"auth":{},"paths":{}}`, "invalid_settings", "not an absolute path"},
		"bad interface name": {`{"queues":{"usenet":1,"torrent":5},"auth":{},"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}},"network":{"torrent":{"interface":"../wg0"}}}`,
			"invalid_settings", "network.torrent.interface"},
		"bad bind address": {`{"queues":{"usenet":1,"torrent":5},"auth":{},"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}},"network":{"usenet":{"address":"vpn.example.com"}}}`,
			"invalid_settings", "network.usenet.address"},
		"unknown field": {`{"queues":{},"bogus":1}`, "invalid_json", "bogus"},
		"not an object": {`[]`, "invalid_json", ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("PUT", "/api/v1/settings", tc.body, withBearer(key))

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
			assert.Contains(t, rec.Body.String(), tc.contains)
		})
	}
}

func TestPutSettingsThatChangeAuthRequireASessionNotAnAPIKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	sess := e.login()
	openAuth := `{"queues":{"usenet":1,"torrent":5},
"auth":{"disable_auth":true,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":["example.com"]},
"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}}}`

	byKey := e.do("PUT", "/api/v1/settings", openAuth, withBearer(key))
	bySession := e.do("PUT", "/api/v1/settings", openAuth, sess.mods()...)

	require.Equal(t, http.StatusForbidden, byKey.Code, byKey.Body.String())
	assert.Equal(t, "session_required", errorOf(t, byKey))
	require.Equal(t, http.StatusOK, bySession.Code, bySession.Body.String())
	assert.True(t, e.settings.Current().Auth.DisableAuth, "the session's change must apply")
}

func TestSettingsChangesTakeEffectOnAuthenticationAtOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()
	open := `{"queues":{"usenet":1,"torrent":5},
"auth":{"disable_auth":true,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":["example.com"]},
"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}}}`
	require.Equal(t, http.StatusOK, e.do("PUT", "/api/v1/settings", open, sess.mods()...).Code)

	anonymous := e.do("GET", "/api/v1/downloads", "")
	wrongHost := e.do("GET", "/api/v1/downloads", "", func(r *http.Request) { r.Host = "evil.example" })

	assert.Equal(t, http.StatusOK, anonymous.Code)
	assert.Equal(t, http.StatusMisdirectedRequest, wrongHost.Code)
}
