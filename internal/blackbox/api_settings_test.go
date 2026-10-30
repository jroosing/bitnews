package blackbox_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

const settingsPath = "/api/v1/settings"

// invalidPrefix starts the message of every invalid_settings answer: the
// app layer passes the text of core's validation error on.
const invalidPrefix = "core: invalid settings: "

// currentSettings returns GET /api/v1/settings as a generic document, ready
// to be changed and sent back with PUT.
func currentSettings(t *testing.T, d *daemon) map[string]any {
	t.Helper()
	r := d.apiGet(settingsPath)
	requireOK(t, r)
	return obj(t, r)
}

// section returns the nested object at the path of keys.
func section(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

// putSettings sends m as the whole Settings.
func putSettings(t *testing.T, d *daemon, m map[string]any) response {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return d.api(http.MethodPut, settingsPath, string(b))
}

// putWith changes the current Settings with change and sends them back.
func putWith(t *testing.T, d *daemon, change func(m map[string]any)) response {
	t.Helper()
	m := currentSettings(t, d)
	change(m)
	return putSettings(t, d, m)
}

func TestAPISettingsDefaultsAreTheExactDocument(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.apiGet(settingsPath)

	requireOK(t, r)
	assert.Equal(t, defaultSettings+"\n", r.body, "field order, empty lists as [] and not null")
}

func TestAPISettingsNeedTheKey(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	get := d.get(settingsPath, nil)
	put := d.request(http.MethodPut, settingsPath, map[string]string{"Content-Type": "application/json"}, defaultSettings)

	assert.Equal(t, errUnauthorized, get.body)
	assert.Equal(t, errUnauthorized, put.body)
	assert.Equal(t, http.StatusUnauthorized, put.status)
}

func TestAPISettingsStoredByAGoRunAreShown(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	s := core.DefaultSettings()
	s.Queues = core.QueueLimits{Usenet: 3, Torrent: 7, PostprocConcurrency: 2, MinFree: 1 << 30}
	s.Paths.Usenet = core.Roots{Incomplete: "/srv/u/inc", Complete: "/srv/u/done"}
	s.Seeding = core.SeedingSettings{RatioLimit: 1.5, SeedingTimeMinutes: 90, Action: core.SeedingActionRemoveData}
	s.Speed.Global = core.SpeedLimit{DownKBps: 5000, UpKBps: 100}
	s.Speed.Usenet.DownKBps = 2000
	s.Torrent = core.TorrentSettings{ListenPort: 6881, DisableDHT: true, MSE: core.MSERequire}
	s.Network.Usenet = core.ProtocolBinding{Interface: "wg0", Address: "10.2.0.2"}
	s.PostProcess = core.PostProcessOptions{CleanNames: true, UnderscoresToDots: true}
	s.Auth.AllowedHosts = []string{"bitnews.example"}
	s.Auth.TrustedProxies = []string{"172.16.0.0/12"}
	saveSettings(t, cfg, s)
	d := startDaemon(t, cfg)

	r := d.apiGet(settingsPath)

	requireOK(t, r)
	assert.JSONEq(t, `{"queues":{"usenet":3,"torrent":7,"postproc_concurrency":2,"min_free":1073741824},`+
		`"auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":["172.16.0.0/12"],"allowed_hosts":["bitnews.example"]},`+
		`"paths":{"usenet":{"incomplete":"/srv/u/inc","complete":"/srv/u/done"},"torrent":{"incomplete":"/data/torrents","complete":"/data/torrents"}},`+
		`"network":{"usenet":{"interface":"wg0","address":"10.2.0.2"},"torrent":{"interface":"","address":""}},`+
		`"seeding":{"ratio_limit":1.5,"seeding_time_minutes":90,"action":"remove_data"},`+
		`"speed":{"global":{"down_kbps":5000,"up_kbps":100},"usenet":{"down_kbps":2000,"up_kbps":0},"torrent":{"down_kbps":0,"up_kbps":0}},`+
		`"torrent":{"listen_port":6881,"disable_dht":true,"disable_pex":false,"lsd":false,"mse":"require"},`+
		`"postprocess":{"clean_names":true,"underscores_to_dots":true}}`, r.body)
}

func TestAPISettingsStoredWithMissingKeysTakeTheDefaultsOfThoseKeys(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	db := openDB(t, cfg)
	_, err := db.Writer().ExecContext(t.Context(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('settings', ?, 0)`,
		`{"queues":{"usenet":4},"paths":{"torrent":{"complete":"/srv/t/done"}}}`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	d := startDaemon(t, cfg)

	m := currentSettings(t, d)

	assert.EqualValues(t, 4, section(m, "queues")["usenet"])
	assert.EqualValues(t, 5, section(m, "queues")["torrent"], "left out: the default")
	assert.EqualValues(t, 1, section(m, "queues")["postproc_concurrency"])
	assert.Equal(t, "/srv/t/done", section(m, "paths", "torrent")["complete"])
	assert.Equal(t, "/data/torrents", section(m, "paths", "torrent")["incomplete"])
	assert.Equal(t, "/data/usenet/complete", section(m, "paths", "usenet")["complete"])
	assert.Equal(t, "pause", section(m, "seeding")["action"])
}

func TestAPISettingsPutChangesEveryMainSectionAndReturnsThemBack(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	const body = `{"queues":{"usenet":2,"torrent":9,"postproc_concurrency":3,"min_free":2147483648},` +
		`"auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},` +
		`"paths":{"usenet":{"incomplete":"/srv/u/inc","complete":"/srv/u/done"},"torrent":{"incomplete":"/srv/t/inc","complete":"/srv/t/done"}},` +
		`"network":{"usenet":{"interface":"tun0","address":"10.8.0.2"},"torrent":{"interface":"wg0","address":"fd00::2"}},` +
		`"seeding":{"ratio_limit":2.25,"seeding_time_minutes":600,"action":"remove"},` +
		`"speed":{"global":{"down_kbps":8000,"up_kbps":800},"usenet":{"down_kbps":4000,"up_kbps":0},"torrent":{"down_kbps":3000,"up_kbps":300}},` +
		`"torrent":{"listen_port":6999,"disable_dht":true,"disable_pex":true,"lsd":true,"mse":"disable"},` +
		`"postprocess":{"clean_names":true,"underscores_to_dots":true}}`

	put := d.api(http.MethodPut, settingsPath, body)

	requireOK(t, put)
	assert.JSONEq(t, body, put.body)
	get := d.apiGet(settingsPath)
	assert.Equal(t, put.body, get.body, "GET shows what PUT answered, byte for byte")
	got := storedSettings(t, cfg)
	assert.Equal(t, 2, got.Queues.Usenet)
	assert.Equal(t, int64(2147483648), got.Queues.MinFree)
	assert.Equal(t, "/srv/t/done", got.Paths.Torrent.Complete)
	assert.Equal(t, "wg0", got.Network.Torrent.Interface)
	assert.Equal(t, 2.25, got.Seeding.RatioLimit)
	assert.Equal(t, 300, got.Speed.Torrent.UpKBps)
	assert.Equal(t, 6999, got.Torrent.ListenPort)
	assert.Equal(t, core.MSEDisable, got.Torrent.MSE)
	assert.True(t, got.PostProcess.UnderscoresToDots)
}

func TestAPISettingsChangesSurviveARestart(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	put := putWith(t, d, func(m map[string]any) {
		section(m, "queues")["usenet"] = 6
		section(m, "paths", "usenet")["complete"] = "/srv/other/done"
		section(m, "speed", "global")["down_kbps"] = 1234
		section(m, "seeding")["ratio_limit"] = 0.5
		section(m, "postprocess")["clean_names"] = true
	})
	requireOK(t, put)
	d.stop()

	again := startDaemon(t, cfg)

	assert.Equal(t, put.body, again.apiGet(settingsPath).body)
}

func TestAPISettingsPutIsAFullReplaceSoLeftOutSectionsBecomeZero(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.api(http.MethodPut, settingsPath, `{"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}}}`)

	requireOK(t, r)
	assert.Equal(t, `{"queues":{"usenet":0,"torrent":0,"postproc_concurrency":0,"min_free":0},`+
		`"auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},`+
		`"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}},`+
		`"network":{"usenet":{"interface":"","address":""},"torrent":{"interface":"","address":""}},`+
		`"seeding":{"ratio_limit":0,"seeding_time_minutes":0,"action":""},`+
		`"speed":{"global":{"down_kbps":0,"up_kbps":0},"usenet":{"down_kbps":0,"up_kbps":0},"torrent":{"down_kbps":0,"up_kbps":0}},`+
		`"torrent":{"listen_port":0,"disable_dht":false,"disable_pex":false,"lsd":false,"mse":""},`+
		`"postprocess":{"clean_names":false,"underscores_to_dots":false}}`+"\n", r.body,
		"what the body leaves out is zero, not the old value and not the default")
}

func TestAPISettingsPutAcceptsTheLimitsAndEmptyEnums(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for name, change := range map[string]func(m map[string]any){
		"max speed":        func(m map[string]any) { section(m, "speed", "global")["down_kbps"] = 1 << 30 },
		"max concurrency":  func(m map[string]any) { section(m, "queues")["postproc_concurrency"] = 16 },
		"max min free":     func(m map[string]any) { section(m, "queues")["min_free"] = int64(1) << 50 },
		"max port":         func(m map[string]any) { section(m, "torrent")["listen_port"] = 65535 },
		"empty mse":        func(m map[string]any) { section(m, "torrent")["mse"] = "" },
		"mse prefer":       func(m map[string]any) { section(m, "torrent")["mse"] = "prefer" },
		"empty action":     func(m map[string]any) { section(m, "seeding")["action"] = "" },
		"negative ratio":   func(m map[string]any) { section(m, "seeding")["ratio_limit"] = -1 },
		"negative minutes": func(m map[string]any) { section(m, "seeding")["seeding_time_minutes"] = -1 },
		"max minutes":      func(m map[string]any) { section(m, "seeding")["seeding_time_minutes"] = 10 * 365 * 24 * 60 },
		"equal roots": func(m map[string]any) {
			section(m, "paths", "usenet")["complete"] = section(m, "paths", "usenet")["incomplete"]
		},
		"interface chars": func(m map[string]any) { section(m, "network", "usenet")["interface"] = "Eth_0.1:a-b" },
		"15 char iface":   func(m map[string]any) { section(m, "network", "usenet")["interface"] = strings.Repeat("a", 15) },
		"ipv6 address":    func(m map[string]any) { section(m, "network", "torrent")["address"] = "2001:db8::1" },
		"zero queues":     func(m map[string]any) { section(m, "queues")["usenet"] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := putWith(t, d, change)
			assert.Equal(t, http.StatusOK, r.status, r.body)
		})
	}
}

func TestAPISettingsPutRejectsInvalidValuesWithTheProblemInTheMessage(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	const notClean = ` is not a clean path (no "..", "." or doubled and trailing separators)`

	for name, tc := range map[string]struct {
		change func(m map[string]any)
		msg    string
	}{
		"negative usenet queue":  {func(m map[string]any) { section(m, "queues")["usenet"] = -1 }, "usenet queue limit -1 is negative"},
		"negative torrent queue": {func(m map[string]any) { section(m, "queues")["torrent"] = -2 }, "torrent queue limit -2 is negative"},
		"concurrency high":       {func(m map[string]any) { section(m, "queues")["postproc_concurrency"] = 17 }, "postproc_concurrency 17 is not between 0 and 16"},
		"concurrency negative":   {func(m map[string]any) { section(m, "queues")["postproc_concurrency"] = -1 }, "postproc_concurrency -1 is not between 0 and 16"},
		"min free negative":      {func(m map[string]any) { section(m, "queues")["min_free"] = -1 }, "min_free -1 is not between 0 and 1125899906842624"},
		"min free high":          {func(m map[string]any) { section(m, "queues")["min_free"] = (int64(1) << 50) + 1 }, "min_free 1125899906842625 is not between 0 and 1125899906842624"},
		"relative path":          {func(m map[string]any) { section(m, "paths", "usenet")["incomplete"] = "rel/dir" }, `usenet incomplete root "rel/dir" is not an absolute path`},
		"empty path":             {func(m map[string]any) { section(m, "paths", "torrent")["complete"] = "" }, `torrent complete root "" is not an absolute path`},
		"trailing slash":         {func(m map[string]any) { section(m, "paths", "usenet")["complete"] = "/data/x/" }, `usenet complete root "/data/x/"` + notClean},
		"dot dot":                {func(m map[string]any) { section(m, "paths", "torrent")["incomplete"] = "/data/../x" }, `torrent incomplete root "/data/../x"` + notClean},
		"nested root": {func(m map[string]any) { section(m, "paths", "usenet")["incomplete"] = "/data/usenet/complete/inc" },
			`usenet incomplete root "/data/usenet/complete/inc" lies inside the usenet complete root "/data/usenet/complete"`},
		"root inside other protocol": {func(m map[string]any) { section(m, "paths", "torrent")["incomplete"] = "/data/usenet/incomplete/t" },
			`torrent incomplete root "/data/usenet/incomplete/t" lies inside the usenet incomplete root "/data/usenet/incomplete"`},
		"speed negative":    {func(m map[string]any) { section(m, "speed", "global")["down_kbps"] = -1 }, "speed.global.down_kbps -1 is not between 0 and 1073741824"},
		"speed high":        {func(m map[string]any) { section(m, "speed", "torrent")["up_kbps"] = (1 << 30) + 1 }, "speed.torrent.up_kbps 1073741825 is not between 0 and 1073741824"},
		"usenet upload":     {func(m map[string]any) { section(m, "speed", "usenet")["up_kbps"] = 5 }, "speed.usenet.up_kbps must be 0: Usenet only downloads"},
		"port negative":     {func(m map[string]any) { section(m, "torrent")["listen_port"] = -1 }, "torrent.listen_port -1 is not between 0 and 65535"},
		"port high":         {func(m map[string]any) { section(m, "torrent")["listen_port"] = 70000 }, "torrent.listen_port 70000 is not between 0 and 65535"},
		"unknown mse":       {func(m map[string]any) { section(m, "torrent")["mse"] = "bogus" }, `torrent.mse "bogus" is not prefer, require or disable`},
		"mse in upper case": {func(m map[string]any) { section(m, "torrent")["mse"] = "PREFER" }, `torrent.mse "PREFER" is not prefer, require or disable`},
		"interface slash": {func(m map[string]any) { section(m, "network", "torrent")["interface"] = "../wg0" },
			`network.torrent.interface "../wg0": character '/' is not allowed (use letters, digits and _ . : -)`},
		"interface too long": {func(m map[string]any) { section(m, "network", "usenet")["interface"] = strings.Repeat("a", 16) },
			`network.usenet.interface "aaaaaaaaaaaaaaaa": longer than 15 characters`},
		"address a name": {func(m map[string]any) { section(m, "network", "usenet")["address"] = "vpn.example.com" },
			`network.usenet.address "vpn.example.com": not an IP address`},
		"address unspecified": {func(m map[string]any) { section(m, "network", "torrent")["address"] = "0.0.0.0" },
			`network.torrent.address "0.0.0.0": must not be the unspecified address`},
		"address multicast": {func(m map[string]any) { section(m, "network", "torrent")["address"] = "224.0.0.1" },
			`network.torrent.address "224.0.0.1": must not be a multicast address`},
		"address with zone": {func(m map[string]any) { section(m, "network", "usenet")["address"] = "fe80::1%eth0" },
			`network.usenet.address "fe80::1%eth0": must not have a zone`},
		"unknown action": {func(m map[string]any) { section(m, "seeding")["action"] = "delete" }, `seeding action "delete" is not pause, remove or remove_data`},
		"too long seeding": {func(m map[string]any) { section(m, "seeding")["seeding_time_minutes"] = 10*365*24*60 + 1 },
			"seeding time 5256001 minutes is more than 5256000"},
	} {
		t.Run(name, func(t *testing.T) {
			r := putWith(t, d, tc.change)

			requireErrorMessage(t, r, http.StatusBadRequest, "invalid_settings", invalidPrefix+tc.msg)
		})
	}
	assert.Equal(t, defaultSettings+"\n", d.apiGet(settingsPath).body, "no refused change was stored")
}

func TestAPISettingsPutListsEveryProblemOneToALine(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := putWith(t, d, func(m map[string]any) {
		section(m, "queues")["usenet"] = -1
		section(m, "queues")["torrent"] = -1
		section(m, "paths", "usenet")["incomplete"] = "rel"
		section(m, "torrent")["mse"] = "x"
	})

	requireErrorMessage(t, r, http.StatusBadRequest, "invalid_settings", invalidPrefix+
		"usenet queue limit -1 is negative\n"+
		"torrent queue limit -1 is negative\n"+
		`usenet incomplete root "rel" is not an absolute path`+"\n"+
		`torrent.mse "x" is not prefer, require or disable`)
}

func TestAPISettingsPutWithoutPathsIsRefused(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.api(http.MethodPut, settingsPath, `{"queues":{"usenet":1,"torrent":5}}`)

	requireErrorMessage(t, r, http.StatusBadRequest, "invalid_settings", invalidPrefix+
		`usenet incomplete root "" is not an absolute path`+"\n"+
		`usenet complete root "" is not an absolute path`+"\n"+
		`torrent incomplete root "" is not an absolute path`+"\n"+
		`torrent complete root "" is not an absolute path`)
}

func TestAPISettingsPutRefusesToChangeTheAuthSectionWithTheKey(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	const msg = "the authentication settings are changed in the UI, not through the API"

	for name, change := range map[string]func(m map[string]any){
		"disable auth": func(m map[string]any) {
			section(m, "auth")["disable_auth"] = true
			section(m, "auth")["allowed_hosts"] = []string{"example.com"}
		},
		"lan bypass":      func(m map[string]any) { section(m, "auth")["lan_bypass_cidrs"] = []string{"192.168.0.0/16"} },
		"trusted proxies": func(m map[string]any) { section(m, "auth")["trusted_proxies"] = []string{"10.0.0.0/8"} },
		"allowed hosts":   func(m map[string]any) { section(m, "auth")["allowed_hosts"] = []string{"example.com"} },
		"invalid too": func(m map[string]any) {
			section(m, "auth")["allowed_hosts"] = []string{"example.com"}
			section(m, "queues")["usenet"] = -1
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := putWith(t, d, change)

			requireErrorMessage(t, r, http.StatusForbidden, "session_required", msg)
		})
	}
	assert.Equal(t, defaultSettings+"\n", d.apiGet(settingsPath).body)
}

func TestAPISettingsPutKeepsAnExistingAuthSectionWhenItIsSentBackAsIs(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	s := core.DefaultSettings()
	s.Auth = core.Auth{DisableAuth: false, LANBypassCIDRs: []string{"192.168.1.0/24"}, TrustedProxies: []string{"10.0.0.0/8"}, AllowedHosts: []string{"bitnews.lan"}}
	saveSettings(t, cfg, s)
	d := startDaemon(t, cfg)

	r := putWith(t, d, func(m map[string]any) { section(m, "queues")["usenet"] = 4 })

	requireOK(t, r)
	assert.Equal(t, []any{"192.168.1.0/24"}, section(obj(t, r), "auth")["lan_bypass_cidrs"])
	assert.Equal(t, core.Auth{LANBypassCIDRs: []string{"192.168.1.0/24"}, TrustedProxies: []string{"10.0.0.0/8"}, AllowedHosts: []string{"bitnews.lan"}}, storedSettings(t, cfg).Auth)
	assert.Equal(t, 4, storedSettings(t, cfg).Queues.Usenet)
	// An omitted auth section is not the same list, so it is a change.
	omitted := d.api(http.MethodPut, settingsPath, `{"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}}}`)
	requireError(t, omitted, http.StatusForbidden, "session_required")
}

func TestAPISettingsPutSendingTheAuthListsAsNullIsTheSameAsEmpty(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := putWith(t, d, func(m map[string]any) {
		section(m, "auth")["lan_bypass_cidrs"] = nil
		section(m, "auth")["trusted_proxies"] = nil
		section(m, "auth")["allowed_hosts"] = nil
	})

	requireOK(t, r)
	assert.Equal(t, []any{}, section(obj(t, r), "auth")["allowed_hosts"], "answered as [] never null")
}

func TestAPISettingsPutRejectsBadDocuments(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for name, body := range map[string]string{
		"unknown top field":    `{"bogus":1}`,
		"unknown nested field": `{"queues":{"usenet":1,"bogus":1}}`,
		"unknown deep field":   `{"speed":{"global":{"down_kbps":1,"rate":2}}}`,
		"old flat key":         `{"download_dir":"/x"}`,
		"array":                `[]`,
		"string":               `"x"`,
		"number":               `7`,
		"truncated":            `{"queues":`,
		"wrong type section":   `{"queues":5}`,
		"wrong type value":     `{"queues":{"usenet":"1"}}`,
		"fractional int":       `{"queues":{"usenet":1.5}}`,
		"bool as string":       `{"torrent":{"disable_dht":"yes"}}`,
		"list as string":       `{"auth":{"allowed_hosts":"x"}}`,
		"two documents":        `{} {}`,
		"trailing garbage":     `{}x`,
	} {
		t.Run(name, func(t *testing.T) {
			r := d.api(http.MethodPut, settingsPath, body)

			requireError(t, r, http.StatusBadRequest, "invalid_json")
		})
	}
	assert.Equal(t, defaultSettings+"\n", d.apiGet(settingsPath).body)
}

func TestAPISettingsPutWithAnEmptyBodyIsInvalidJSON(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.request(http.MethodPut, settingsPath, d.apiHeaders(), "")

	requireErrorMessage(t, r, http.StatusBadRequest, "invalid_json", "the request body is empty")
}

func TestAPISettingsOnlyAllowGetAndPut(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		r := d.api(m, settingsPath, "")
		assert.Equal(t, http.StatusMethodNotAllowed, r.status, m)
		assert.Equal(t, errMethod, r.body)
		assert.Equal(t, "GET, HEAD, PUT", r.header.Get("Allow"))
	}
}
