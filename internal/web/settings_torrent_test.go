package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/web"
)

func connectionForm(port string, dht, pex, lsd bool, mse string) url.Values {
	v := url.Values{"listen_port": {port}, "mse": {mse}}
	for name, ticked := range map[string]bool{"dht": dht, "pex": pex, "lsd": lsd} {
		if ticked {
			v.Set(name, "on")
		}
	}
	return v
}

func TestTorrentPageShowsTheConnectionSettings(t *testing.T) {
	e, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/torrent").Body.String())

	form := byID(doc, "connection")
	require.NotNil(t, form)
	assert.Equal(t, "post", attr(form, "method"))
	assert.Equal(t, "/settings/torrent", attr(form, "hx-post"))
	assert.NotNil(t, find(form, func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "_csrf" }))
	assert.Equal(t, "51413", attr(byID(doc, "listen-port"), "value"), "the default port")
	for _, id := range []string{"dht", "pex"} {
		assert.True(t, hasAttr(byID(doc, id), "checked"), id)
	}
	assert.False(t, hasAttr(byID(doc, "lsd"), "checked"))
	sel := byID(doc, "mse")
	require.NotNil(t, sel)
	opts := findAll(sel, func(n *html.Node) bool { return n.Data == "option" })
	require.Len(t, opts, 3)
	for _, o := range opts {
		assert.Equal(t, attr(o, "value") == "prefer", hasAttr(o, "selected"), attr(o, "value"))
	}
	card := text(byID(doc, "connection-card"))
	assert.Contains(t, card, "Forward")
	assert.Contains(t, card, "restart")

	cur := e.settings.Current()
	cur.Torrent = core.TorrentSettings{ListenPort: 6881, DisableDHT: true, LSD: true, MSE: core.MSERequire}
	require.NoError(t, e.settings.Update(t.Context(), cur))
	doc = parseHTML(t, b.get("/settings/torrent").Body.String())
	assert.Equal(t, "6881", attr(byID(doc, "listen-port"), "value"))
	assert.False(t, hasAttr(byID(doc, "dht"), "checked"))
	assert.True(t, hasAttr(byID(doc, "pex"), "checked"))
	assert.True(t, hasAttr(byID(doc, "lsd"), "checked"))
}

func TestSavingTheConnectionChangesOnlyTheTorrentSettings(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	before.Auth.AllowedHosts = []string{"example.com"}
	before.Network.Torrent.Address = "10.1.2.3"
	before.Seeding.RatioLimit = 2
	before.Speed.Global.DownKBps = 50
	require.NoError(t, e.settings.Update(t.Context(), before))

	rec := b.settingsPost("/settings/torrent", connectionForm(" 6881 ", false, true, true, "require"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := e.settings.Current()
	assert.Equal(t, core.TorrentSettings{ListenPort: 6881, DisableDHT: true, LSD: true, MSE: core.MSERequire}, after.Torrent)
	before.Torrent = after.Torrent
	assert.Equal(t, before, after)
	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	assert.Contains(t, msg, "Connection settings saved")
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "connection-card"))
}

func TestBadConnectionSettingsAreRejectedNextToTheirFields(t *testing.T) {
	for name, tc := range map[string]struct{ field, id, value string }{
		"empty port":   {"listen_port", "listen-port", ""},
		"zero port":    {"listen_port", "listen-port", "0"},
		"big port":     {"listen_port", "listen-port", "65536"},
		"negative":     {"listen_port", "listen-port", "-1"},
		"words":        {"listen_port", "listen-port", `<u>net</u>`},
		"unknown mse":  {"mse", "mse", "maybe"},
		"mse injected": {"mse", "mse", `"><script>x</script>`},
	} {
		t.Run(name, func(t *testing.T) {
			e, b := settingsSignedIn(t)
			before := e.settings.Current()
			form := connectionForm("6881", true, true, false, "prefer")
			form.Set(tc.field, tc.value)

			rec := b.settingsPost("/settings/torrent", form)

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, before, e.settings.Current())
			assert.NotContains(t, rec.Body.String(), "<script>x")
			assert.NotContains(t, rec.Body.String(), "<u>net")
			doc := parseHTML(t, rec.Body.String())
			assert.NotEmpty(t, text(byID(doc, tc.id+"-error")))
			assert.Equal(t, "true", attr(byID(doc, tc.id), "aria-invalid"))
			assert.Equal(t, "true", attr(byID(doc, "connection"), "data-dirty"))
		})
	}
}

func TestChangingTheConnectionTellsYouToRestart(t *testing.T) {
	cfg := &fakeConfig{started: core.DefaultSettings()}
	_, b := settingsSignedIn(t, withConfig(cfg))

	same := b.settingsPost("/settings/torrent", connectionForm("51413", true, true, false, "prefer"))
	level, _ := settingsToastOf(t, same)
	assert.Equal(t, "success", level)
	assert.Nil(t, byID(parseHTML(t, same.Body.String()), "connection-restart"))

	changed := b.settingsPost("/settings/torrent", connectionForm("51413", true, true, false, "require"))

	level, msg := settingsToastOf(t, changed)
	assert.Equal(t, "info", level)
	assert.Contains(t, msg, "Restart bitnews")
	require.NotNil(t, byID(parseHTML(t, changed.Body.String()), "connection-restart"))

	back := b.settingsPost("/settings/torrent", connectionForm("51413", true, true, false, "prefer"))
	assert.Nil(t, byID(parseHTML(t, back.Body.String()), "connection-restart"), "back to what is running")
}

func TestChangingOnlyTheListenPortNeedsNoRestart(t *testing.T) {
	cfg := &fakeConfig{started: core.DefaultSettings()}
	e, b := settingsSignedIn(t, withConfig(cfg))

	rec := b.settingsPost("/settings/torrent", connectionForm("6881", true, true, false, "prefer"))

	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level, msg)
	assert.Nil(t, byID(parseHTML(t, rec.Body.String()), "connection-restart"))
	assert.Equal(t, 6881, e.settings.Current().Torrent.ListenPort)
}

func TestAPortFixedByTheOperatorIsShownAndNotEditable(t *testing.T) {
	started := core.DefaultSettings()
	cfg := &fakeConfig{started: started, portOverride: 7000}
	e, b := settingsSignedIn(t, withConfig(cfg))

	doc := parseHTML(t, b.get("/settings/torrent").Body.String())

	in := byID(doc, "listen-port")
	assert.Equal(t, "7000", attr(in, "value"), "what the engine really uses")
	assert.True(t, hasAttr(in, "disabled"))
	assert.Contains(t, text(byID(doc, "connection-card")), "BITNEWS_TORRENT_PORT")

	// Saving the card leaves the stored port alone and does not ask for a restart for it.
	form := connectionForm("", true, true, false, "require") // a disabled field is not sent
	rec := b.settingsPost("/settings/torrent", form)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 0, e.settings.Current().Torrent.ListenPort)
	assert.Equal(t, core.MSERequire, e.settings.Current().Torrent.MSE)
	_, msg := settingsToastOf(t, rec)
	assert.Contains(t, msg, "Restart bitnews", "the encryption policy still needs one")

	same := b.settingsPost("/settings/torrent", connectionForm("", true, true, false, "prefer"))
	level, _ := settingsToastOf(t, same)
	assert.Equal(t, "success", level, "the port does not count when the operator fixes it")
}

func bindingForm(kv ...string) url.Values { return speedLimitsForm(kv...) }

func TestTorrentPageShowsTheBindingsWithTheirLiveStatus(t *testing.T) {
	cfg := &fakeConfig{bindings: map[core.SourceKind]web.BindingStatus{
		core.SourceNZB:     {},
		core.SourceTorrent: {Enabled: true, Interface: "wg0", Address: "10.2.0.2", Available: false, Reason: `interface "wg0" does not exist`},
	}}
	e, b := settingsSignedIn(t, withConfig(cfg))
	cur := e.settings.Current()
	cur.Network.Torrent = core.ProtocolBinding{Interface: "wg0", Address: "10.2.0.2"}
	require.NoError(t, e.settings.Update(t.Context(), cur))

	doc := parseHTML(t, b.get("/settings/torrent").Body.String())

	form := byID(doc, "binding")
	require.NotNil(t, form)
	assert.Equal(t, "/settings/torrent/binding", attr(form, "hx-post"))
	assert.Equal(t, "wg0", attr(byID(doc, "torrent-interface"), "value"))
	assert.Equal(t, "10.2.0.2", attr(byID(doc, "torrent-address"), "value"))
	assert.Empty(t, attr(byID(doc, "usenet-interface"), "value"))
	assert.Contains(t, text(byID(doc, "usenet-binding-status")), "Not bound")
	status := text(byID(doc, "torrent-binding-status"))
	assert.Contains(t, status, "Unavailable")
	assert.Contains(t, status, `interface "wg0" does not exist`)
	assert.Equal(t, "unavailable", attr(byID(doc, "torrent-binding-status"), "data-state"))
	assert.Equal(t, "off", attr(byID(doc, "usenet-binding-status"), "data-state"))
}

func TestABoundAndAvailableBindingIsShownAsBound(t *testing.T) {
	cfg := &fakeConfig{bindings: map[core.SourceKind]web.BindingStatus{
		core.SourceNZB: {Enabled: true, Interface: "tun0", Available: true},
	}}
	e, b := settingsSignedIn(t, withConfig(cfg))
	cur := e.settings.Current()
	cur.Network.Usenet.Interface = "tun0"
	require.NoError(t, e.settings.Update(t.Context(), cur))

	doc := parseHTML(t, b.get("/settings/torrent").Body.String())

	st := byID(doc, "usenet-binding-status")
	assert.Equal(t, "bound", attr(st, "data-state"))
	assert.Contains(t, text(st), "Bound")
	assert.Contains(t, text(st), "tun0")
}

func TestWithoutADaemonThereIsNoLiveStatus(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/torrent").Body.String())

	assert.Nil(t, byID(doc, "usenet-binding-status"))
	assert.NotNil(t, byID(doc, "usenet-interface"), "the form is still there")
}

func TestSavingBindingsChangesOnlyTheNetworkSettings(t *testing.T) {
	e, b := settingsSignedIn(t, withServer(func(s *web.Server) { web.SetInterfaceBinding(s, true) }))
	before := e.settings.Current()
	before.Torrent.ListenPort = 6881
	before.Queues.Usenet = 4
	require.NoError(t, e.settings.Update(t.Context(), before))

	rec := b.settingsPost("/settings/torrent/binding", bindingForm(
		"usenet_interface", " wg0 ", "usenet_address", "10.2.0.2", "torrent_interface", "", "torrent_address", " fd00::2 "))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := e.settings.Current()
	assert.Equal(t, core.NetworkSettings{
		Usenet:  core.ProtocolBinding{Interface: "wg0", Address: "10.2.0.2"},
		Torrent: core.ProtocolBinding{Address: "fd00::2"},
	}, after.Network)
	before.Network = after.Network
	assert.Equal(t, before, after)
	_, msg := settingsToastOf(t, rec)
	assert.Contains(t, msg, "Network binding saved")
}

func TestClearingBothBindingFieldsTurnsTheBindingOff(t *testing.T) {
	e, b := settingsSignedIn(t)
	cur := e.settings.Current()
	cur.Network.Usenet = core.ProtocolBinding{Interface: "wg0"}
	require.NoError(t, e.settings.Update(t.Context(), cur))

	rec := b.settingsPost("/settings/torrent/binding", url.Values{})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, core.NetworkSettings{}, e.settings.Current().Network)
}

func TestBadBindingsAreRejectedNextToTheirFields(t *testing.T) {
	for name, tc := range map[string]struct{ field, id, value, msg string }{
		"space in name":    {"usenet_interface", "usenet-interface", "my vpn", "letters"},
		"long name":        {"torrent_interface", "torrent-interface", "abcdefghijklmnop", "15"},
		"injected name":    {"usenet_interface", "usenet-interface", `"><script>x</script>`, "letters"},
		"not an ip":        {"usenet_address", "usenet-address", "vpn.example.com", "IP address"},
		"unspecified":      {"torrent_address", "torrent-address", "0.0.0.0", "IP address"},
		"multicast":        {"torrent_address", "torrent-address", "224.0.0.1", "IP address"},
		"with port":        {"usenet_address", "usenet-address", "10.0.0.2:51413", "IP address"},
		"zone":             {"usenet_address", "usenet-address", "fe80::1%eth0", "IP address"},
		"control char":     {"usenet_interface", "usenet-interface", "wg\x000", "letters"},
		"markup in field":  {"torrent_address", "torrent-address", "<b>1</b>", "IP address"},
		"interface of two": {"usenet_interface", "usenet-interface", "wg0,wg1", "letters"},
	} {
		t.Run(name, func(t *testing.T) {
			e, b := settingsSignedIn(t, withServer(func(s *web.Server) { web.SetInterfaceBinding(s, true) }))
			before := e.settings.Current()
			form := bindingForm("usenet_interface", "wg0")
			form.Set(tc.field, tc.value)

			rec := b.settingsPost("/settings/torrent/binding", form)

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, before, e.settings.Current())
			assert.NotContains(t, rec.Body.String(), "<script>x")
			assert.NotContains(t, rec.Body.String(), "<b>1")
			doc := parseHTML(t, rec.Body.String())
			assert.Contains(t, text(byID(doc, tc.id+"-error")), tc.msg)
			assert.Equal(t, "true", attr(byID(doc, tc.id), "aria-invalid"))
			assert.Equal(t, "true", attr(byID(doc, "binding"), "data-dirty"))
		})
	}
}

func TestInterfaceBindingIsRefusedWhereTheHostCannotDoIt(t *testing.T) {
	e, b := settingsSignedIn(t, withServer(func(s *web.Server) { web.SetInterfaceBinding(s, false) }))
	before := e.settings.Current()

	rec := b.settingsPost("/settings/torrent/binding", bindingForm("torrent_interface", "wg0"))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, before, e.settings.Current(), "bitnews would refuse to start with it")
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "torrent-interface-error")), "Linux")

	ok := b.settingsPost("/settings/torrent/binding", bindingForm("torrent_address", "10.0.0.2"))
	assert.Equal(t, http.StatusOK, ok.Code, "an address works everywhere")
}

func TestChangingABindingTellsYouToRestart(t *testing.T) {
	cfg := &fakeConfig{started: core.DefaultSettings()}
	_, b := settingsSignedIn(t, withConfig(cfg), withServer(func(s *web.Server) { web.SetInterfaceBinding(s, true) }))

	rec := b.settingsPost("/settings/torrent/binding", bindingForm("torrent_interface", "wg0"))

	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "info", level)
	assert.Contains(t, msg, "Restart bitnews")
	note := byID(parseHTML(t, rec.Body.String()), "binding-restart")
	require.NotNil(t, note)
	assert.Contains(t, text(note), "restart")
}

func seedingForm(ratio, minutes, action string) url.Values {
	return url.Values{"ratio_limit": {ratio}, "seeding_minutes": {minutes}, "seeding_action": {action}}
}

func TestTorrentPageShowsTheShareLimitDefaults(t *testing.T) {
	e, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/torrent").Body.String())

	form := byID(doc, "seeding")
	require.NotNil(t, form)
	assert.Equal(t, "/settings/torrent/seeding", attr(form, "hx-post"))
	assert.Empty(t, attr(byID(doc, "ratio-limit"), "value"), "no limit by default")
	assert.Empty(t, attr(byID(doc, "seeding-minutes"), "value"))
	for _, o := range findAll(byID(doc, "seeding-action"), func(n *html.Node) bool { return n.Data == "option" }) {
		assert.Equal(t, attr(o, "value") == "pause", hasAttr(o, "selected"), attr(o, "value"))
	}
	assert.Contains(t, text(byID(doc, "seeding-card")), "Share Limit")

	cur := e.settings.Current()
	cur.Seeding = core.SeedingSettings{RatioLimit: 1.5, SeedingTimeMinutes: 120, Action: core.SeedingActionRemoveData}
	require.NoError(t, e.settings.Update(t.Context(), cur))
	doc = parseHTML(t, b.get("/settings/torrent").Body.String())
	assert.Equal(t, "1.5", attr(byID(doc, "ratio-limit"), "value"))
	assert.Equal(t, "120", attr(byID(doc, "seeding-minutes"), "value"))
}

func TestSavingShareLimitDefaultsChangesOnlySeeding(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	before.Torrent.ListenPort = 6881
	before.Queues.Torrent = 9
	require.NoError(t, e.settings.Update(t.Context(), before))

	rec := b.settingsPost("/settings/torrent/seeding", seedingForm(" 2 ", "90", "remove"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := e.settings.Current()
	assert.Equal(t, core.SeedingSettings{RatioLimit: 2, SeedingTimeMinutes: 90, Action: core.SeedingActionRemove}, after.Seeding)
	before.Seeding = after.Seeding
	assert.Equal(t, before, after)
	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	assert.Contains(t, msg, "Share Limit defaults saved")
}

func TestEmptyShareLimitsMeanNoLimit(t *testing.T) {
	e, b := settingsSignedIn(t)
	cur := e.settings.Current()
	cur.Seeding = core.SeedingSettings{RatioLimit: 2, SeedingTimeMinutes: 90, Action: core.SeedingActionRemove}
	require.NoError(t, e.settings.Update(t.Context(), cur))

	rec := b.settingsPost("/settings/torrent/seeding", seedingForm("", "0", "pause"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, core.SeedingSettings{Action: core.SeedingActionPause}, e.settings.Current().Seeding)
}

func TestBadShareLimitsAreRejectedNextToTheirFields(t *testing.T) {
	for name, tc := range map[string]struct{ field, id, value string }{
		"negative ratio":   {"ratio_limit", "ratio-limit", "-1"},
		"ratio words":      {"ratio_limit", "ratio-limit", "two"},
		"ratio exponent":   {"ratio_limit", "ratio-limit", "1e2"},
		"ratio NaN":        {"ratio_limit", "ratio-limit", "NaN"},
		"ratio comma":      {"ratio_limit", "ratio-limit", "1,5"},
		"ratio too big":    {"ratio_limit", "ratio-limit", "100000"},
		"ratio markup":     {"ratio_limit", "ratio-limit", "<i>2</i>"},
		"minutes decimal":  {"seeding_minutes", "seeding-minutes", "1.5"},
		"minutes negative": {"seeding_minutes", "seeding-minutes", "-5"},
		"minutes too big":  {"seeding_minutes", "seeding-minutes", "99999999"},
		"unknown action":   {"seeding_action", "seeding-action", "explode"},
	} {
		t.Run(name, func(t *testing.T) {
			e, b := settingsSignedIn(t)
			before := e.settings.Current()
			form := seedingForm("1", "60", "pause")
			form.Set(tc.field, tc.value)

			rec := b.settingsPost("/settings/torrent/seeding", form)

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, before, e.settings.Current())
			assert.NotContains(t, rec.Body.String(), "<i>2")
			doc := parseHTML(t, rec.Body.String())
			assert.NotEmpty(t, text(byID(doc, tc.id+"-error")))
			assert.Equal(t, "true", attr(byID(doc, tc.id), "aria-invalid"))
			assert.Equal(t, "true", attr(byID(doc, "seeding"), "data-dirty"))
		})
	}
}

func TestTorrentPlainFormPostRendersThePageAgain(t *testing.T) {
	e, b := settingsSignedIn(t)
	form := seedingForm("3", "", "pause")
	form.Set("_csrf", b.csrf())

	rec := b.post("/settings/torrent/seeding", form)

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, find(doc, func(n *html.Node) bool { return n.Data == "html" }))
	assert.Contains(t, text(byID(doc, "seeding-card")), "Your changes were saved")
	assert.Equal(t, 3.0, e.settings.Current().Seeding.RatioLimit)
	assert.NotNil(t, byID(doc, "connection-card"))
	assert.NotNil(t, byID(doc, "binding-card"))
}

// addTransferResponses renders the pages and cards of Speed & queue and
// Torrent network in their states (saved, invalid, restart pending, binding
// unavailable, plain form) for the shared HTML invariant tests.
func addTransferResponses(t *testing.T, out map[string]*httptest.ResponseRecorder) {
	t.Helper()
	cfg := &fakeConfig{started: core.DefaultSettings(), bindings: map[core.SourceKind]web.BindingStatus{
		core.SourceNZB:     {Enabled: true, Interface: "wg0", Available: true},
		core.SourceTorrent: {Enabled: true, Address: "10.2.0.2", Reason: "address <10.2.0.2> is not assigned"},
	}}
	_, b := settingsSignedIn(t, withConfig(cfg), withServer(func(s *web.Server) { web.SetInterfaceBinding(s, true) }))
	post := func(name, path string, form url.Values) {
		out["POST "+path+" ("+name+")"] = b.settingsPost(path, form)
	}
	plain := func(path string, form url.Values) {
		form.Set("_csrf", b.csrf())
		out["POST "+path+" (plain form)"] = b.post(path, form)
	}
	post("saved", "/settings/speed", speedLimitsForm("global_down_kbps", "1000", "torrent_up_kbps", "100"))
	post("invalid", "/settings/speed", speedLimitsForm("global_down_kbps", `<x>`))
	plain("/settings/speed", speedLimitsForm("usenet_down_kbps", "5"))
	post("restart pending", "/settings/speed/queue", queueForm("2", "6", "3"))
	post("invalid", "/settings/speed/queue", queueForm("0", "", "99"))
	post("saved", "/settings/speed/space", url.Values{"min_free_gb": {"7.5"}})
	post("invalid", "/settings/speed/space", url.Values{"min_free_gb": {`"><b>`}})
	post("restart pending", "/settings/torrent", connectionForm("6881", true, false, true, "require"))
	post("invalid", "/settings/torrent", connectionForm("0", true, true, false, "x"))
	post("restart pending", "/settings/torrent/binding", bindingForm("usenet_interface", "wg0", "torrent_address", "10.2.0.2"))
	post("invalid", "/settings/torrent/binding", bindingForm("usenet_interface", "my vpn", "torrent_address", "nope"))
	post("saved", "/settings/torrent/seeding", seedingForm("2", "60", "remove_data"))
	post("invalid", "/settings/torrent/seeding", seedingForm("x", "y", "z"))
	plain("/settings/torrent/seeding", seedingForm("1", "", "pause"))
	out["GET /settings/speed (transfer)"] = b.get("/settings/speed")
	out["GET /settings/torrent (transfer)"] = b.get("/settings/torrent")
}
