package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

const serverPassword = "s3cret-Pa55w0rd!"

// recordingProber answers every probe with result and remembers what it saw.
type recordingProber struct {
	mu     sync.Mutex
	result app.ProbeResult
	probed []core.Server
}

func (p *recordingProber) Probe(_ context.Context, s core.Server) app.ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probed = append(p.probed, s)
	return p.result
}

func (p *recordingProber) last(t *testing.T) core.Server {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.NotEmpty(t, p.probed)
	return p.probed[len(p.probed)-1]
}

func okProbe() app.ProbeResult {
	return app.ProbeResult{OK: true, Class: app.ProbeOK, ConnectTime: 123 * time.Millisecond, Latency: 45 * time.Millisecond}
}

// serverForm is a valid dialog post for a server called name.
func serverForm(name string) url.Values {
	return url.Values{
		"name": {name}, "host": {name + ".example.com"}, "port": {"563"}, "username": {"joe"}, "password": {serverPassword},
		"max_connections": {"10"}, "retention_days": {"3000"}, "priority": {"0"}, "enabled": {"on"},
	}
}

func mustCreateServer(t *testing.T, e *env, name string) core.Server {
	t.Helper()
	return mustCreateServerAt(t, e, name, 0)
}

func mustCreateServerAt(t *testing.T, e *env, name string, priority int) core.Server {
	t.Helper()
	srv, err := e.servers.Create(context.Background(), app.ServerSpec{
		Name: name, Host: name + ".example.com", Username: "joe", Password: core.Secret(serverPassword),
		MaxConnections: 10, Priority: priority,
	})
	require.NoError(t, err)
	return srv
}

func serverOrder(t *testing.T, e *env) []string {
	t.Helper()
	list, err := e.servers.List(context.Background())
	require.NoError(t, err)
	var names []string
	for _, s := range list {
		names = append(names, s.Name+":"+strconv.Itoa(s.Priority))
	}
	return names
}

func serverRows(doc *html.Node) []*html.Node {
	return findAll(doc, func(n *html.Node) bool { return n.Data == "tr" && strings.HasPrefix(attr(n, "id"), "server-") })
}

func TestServersPageEmptyStateExplainsServerPriority(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.get("/settings/servers")

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	body := text(byID(doc, "servers-list"))
	assert.Contains(t, body, "No Usenet servers yet")
	assert.Contains(t, body, "lower priority numbers are tried first")
	assert.Contains(t, body, "block accounts that should only fill gaps")
	assert.Empty(t, serverRows(doc))
	add := find(doc, func(n *html.Node) bool { return n.Data == "button" && attr(n, "hx-get") == "/settings/servers/new" })
	require.NotNil(t, add, "an Add server button")
	assert.Equal(t, "#server-dialog-body", attr(add, "hx-target"))
	assert.NotNil(t, find(doc, func(n *html.Node) bool { return n.Data == "dialog" && attr(n, "id") == "server-dialog" }))
}

func TestServersListShowsEachServerWithTLSOnlyAndTheirControls(t *testing.T) {
	e, b := settingsSignedIn(t)
	a := mustCreateServerAt(t, e, "alpha", 0)
	mustCreateServerAt(t, e, "bravo", 1)

	doc := parseHTML(t, b.get("/settings/servers").Body.String())

	rows := serverRows(doc)
	require.Len(t, rows, 2)
	assert.Equal(t, "server-"+string(a.ID), attr(rows[0], "id"), "ordered by priority")
	first := text(rows[0])
	assert.Contains(t, first, "alpha")
	assert.Contains(t, first, "alpha.example.com:563")
	assert.Contains(t, first, "TLS only")
	assert.Contains(t, first, "10 connections")
	assert.Contains(t, first, "Retention: Unlimited")
	sw := find(rows[0], func(n *html.Node) bool { return attr(n, "role") == "switch" })
	require.NotNil(t, sw)
	assert.True(t, hasAttr(sw, "checked"))
	assert.Equal(t, "/settings/servers/"+string(a.ID), attr(sw, "hx-patch"))
	up := find(rows[0], func(n *html.Node) bool {
		return n.Data == "button" && strings.HasSuffix(attr(n, "hx-post"), "/move/up")
	})
	require.NotNil(t, up)
	assert.True(t, hasAttr(up, "disabled"), "the first server cannot move up")
	down := find(rows[1], func(n *html.Node) bool {
		return n.Data == "button" && strings.HasSuffix(attr(n, "hx-post"), "/move/down")
	})
	assert.True(t, hasAttr(down, "disabled"), "the last server cannot move down")
}

func TestThereIsNoControlForPlaintextOrSkippingCertificateChecks(t *testing.T) {
	e, b := settingsSignedIn(t)
	mustCreateServer(t, e, "alpha")
	pages := []string{
		b.get("/settings/servers").Body.String(),
		b.settingsDo("GET", "/settings/servers/new", nil).Body.String(),
		b.settingsDo("GET", "/settings/servers/"+string(mustServerID(t, e, "alpha"))+"/edit", nil).Body.String(),
	}
	for _, body := range pages {
		doc := parseHTML(t, body)
		for _, in := range findAll(doc, func(n *html.Node) bool { return n.Data == "input" || n.Data == "select" }) {
			name := strings.ToLower(attr(in, "name") + attr(in, "id"))
			for _, banned := range []string{"tls", "ssl", "verify", "insecure", "plain", "skip", "starttls"} {
				assert.NotContains(t, name, banned, "control %q", name)
			}
		}
	}
}

func mustServerID(t *testing.T, e *env, name string) core.ID {
	t.Helper()
	list, err := e.servers.List(context.Background())
	require.NoError(t, err)
	for _, s := range list {
		if s.Name == name {
			return s.ID
		}
	}
	t.Fatalf("no server %q", name)
	return ""
}

func TestAddFormHasLabelledFieldsInGroupsWithDefaults(t *testing.T) {
	e, b := settingsSignedIn(t)
	mustCreateServerAt(t, e, "alpha", 4)

	rec := b.settingsDo("GET", "/settings/servers/new", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	form := byID(doc, "server-form")
	require.NotNil(t, form)
	assert.Equal(t, "/settings/servers", attr(form, "hx-post"))
	for id, want := range map[string]string{
		"server-port": "563", "server-connections": "8", "server-retention": "0", "server-priority": "5",
	} {
		assert.Equal(t, want, attr(byID(doc, id), "value"), id)
	}
	labels := map[string]string{}
	for _, l := range findAll(form, func(n *html.Node) bool { return n.Data == "label" }) {
		labels[attr(l, "for")] = text(l)
	}
	for id, label := range map[string]string{
		"server-name": "Name", "server-host": "Host", "server-port": "Port", "server-username": "User name (optional)",
		"server-password": "Password (optional)", "server-connections": "Connections", "server-retention": "Retention (days)",
		"server-priority": "Priority", "server-enabled": "Enabled",
	} {
		assert.Equal(t, label, labels[id], id)
	}
	legends := findAll(form, func(n *html.Node) bool { return n.Data == "legend" })
	var got []string
	for _, l := range legends {
		got = append(got, text(l))
	}
	assert.Equal(t, []string{"Connection", "Login", "Limits"}, got)
	assert.True(t, hasAttr(byID(doc, "server-enabled"), "checked"))
	pw := byID(doc, "server-password")
	assert.False(t, hasAttr(pw, "value"), "the password field never has a value")
	test := find(form, func(n *html.Node) bool { return n.Data == "button" && attr(n, "hx-post") == "/settings/servers/test" })
	require.NotNil(t, test, "Test connection button")
	assert.Equal(t, "#server-test-result", attr(test, "hx-target"))
}

func TestAddingAServerStoresItAndReturnsTheListAndClosesTheDialog(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/servers", serverForm("alpha"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "#servers-list", rec.Header().Get("HX-Retarget"))
	assert.Equal(t, "outerHTML", rec.Header().Get("HX-Reswap"))
	assert.Contains(t, rec.Header().Get("HX-Trigger"), `"dialog-close":true`)
	level, _ := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	list, err := e.servers.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	got := list[0]
	assert.Equal(t, "alpha", got.Name)
	assert.Equal(t, "alpha.example.com", got.Host)
	assert.Equal(t, 563, got.Port)
	assert.Equal(t, "joe", got.Username)
	assert.Equal(t, serverPassword, got.Password.Reveal())
	assert.Equal(t, 10, got.MaxConnections)
	assert.Equal(t, 3000, got.RetentionDays)
	assert.True(t, got.Enabled)
	assert.Len(t, serverRows(parseHTML(t, rec.Body.String())), 1)
}

func TestAddingADisabledServer(t *testing.T) {
	e, b := settingsSignedIn(t)
	form := serverForm("alpha")
	form.Del("enabled")

	rec := b.settingsPost("/settings/servers", form)

	require.Equal(t, http.StatusOK, rec.Code)
	list, _ := e.servers.List(context.Background())
	require.Len(t, list, 1)
	assert.False(t, list[0].Enabled)
}

func TestEachValidationErrorShowsNextToItsField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(url.Values)
		field  string // the id of the control with the error
		want   string
	}{
		{"blank name", func(v url.Values) { v.Set("name", " ") }, "server-name", "Enter a name"},
		{"name too long", func(v url.Values) { v.Set("name", strings.Repeat("n", 65)) }, "server-name", "1 to 64"},
		{"host with scheme", func(v url.Values) { v.Set("host", "https://news.example.com") }, "server-host", "no https://"},
		{"host with port", func(v url.Values) { v.Set("host", "news.example.com:563") }, "server-host", "no https://"},
		{"blank host", func(v url.Values) { v.Set("host", "") }, "server-host", "host name"},
		{"port zero", func(v url.Values) { v.Set("port", "0") }, "server-port", "1 to 65535"},
		{"port too big", func(v url.Values) { v.Set("port", "70000") }, "server-port", "1 to 65535"},
		{"port not a number", func(v url.Values) { v.Set("port", "abc") }, "server-port", "whole number"},
		{"connections zero", func(v url.Values) { v.Set("max_connections", "0") }, "server-connections", "1 to 100"},
		{"connections 101", func(v url.Values) { v.Set("max_connections", "101") }, "server-connections", "1 to 100"},
		{"connections blank", func(v url.Values) { v.Set("max_connections", "") }, "server-connections", "whole number"},
		{"negative retention", func(v url.Values) { v.Set("retention_days", "-1") }, "server-retention", "0 for unlimited"},
		{"negative priority", func(v url.Values) { v.Set("priority", "-2") }, "server-priority", "0 or higher"},
		{"username too long", func(v url.Values) { v.Set("username", strings.Repeat("u", 257)) }, "server-username", "256"},
		{"password with control character", func(v url.Values) { v.Set("password", "a\x01b") }, "server-password", "control characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, b := settingsSignedIn(t)
			form := serverForm("alpha")
			tc.change(form)

			rec := b.settingsPost("/settings/servers", form)

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Empty(t, rec.Header().Get("HX-Retarget"), "the form is shown again in the dialog")
			doc := parseHTML(t, rec.Body.String())
			in := byID(doc, tc.field)
			require.NotNil(t, in)
			assert.Equal(t, "true", attr(in, "aria-invalid"))
			assert.Contains(t, attr(in, "aria-describedby"), tc.field+"-error")
			assert.Contains(t, text(byID(doc, tc.field+"-error")), tc.want)
			assert.Equal(t, "alert", attr(byID(doc, "server-form-error"), "role"))
			list, _ := e.servers.List(context.Background())
			assert.Empty(t, list, "nothing is saved")
			assert.NotContains(t, rec.Body.String(), serverPassword, "the password is not echoed back")
		})
	}
}

func TestSeveralErrorsAreShownAtOnceAndTypedValuesStay(t *testing.T) {
	_, b := settingsSignedIn(t)
	form := serverForm("alpha")
	form.Set("host", "bad host")
	form.Set("port", "0")

	rec := b.settingsPost("/settings/servers", form)

	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, byID(doc, "server-host-error"))
	assert.NotNil(t, byID(doc, "server-port-error"))
	assert.Equal(t, "bad host", attr(byID(doc, "server-host"), "value"))
	assert.Equal(t, "alpha", attr(byID(doc, "server-name"), "value"))
}

func TestADuplicateNameIsReportedOnTheNameField(t *testing.T) {
	e, b := settingsSignedIn(t)
	mustCreateServer(t, e, "alpha")

	rec := b.settingsPost("/settings/servers", serverForm("alpha"))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "server-name-error")), "already exists")
}

func TestHostileServerNamesAreEscaped(t *testing.T) {
	e, b := settingsSignedIn(t)
	hostile := `<img src=x onerror=alert(1)>"'&`
	_, err := e.servers.Create(context.Background(), app.ServerSpec{Name: hostile, Host: "h.example.com", MaxConnections: 2})
	require.NoError(t, err)

	rec := b.get("/settings/servers")

	assert.NotContains(t, rec.Body.String(), "<img src=x")
	doc := parseHTML(t, rec.Body.String())
	assert.Nil(t, find(doc, func(n *html.Node) bool { return n.Data == "img" }))
	assert.Contains(t, text(serverRows(doc)[0]), hostile)
}

func TestThePasswordIsNeverRenderedOrLogged(t *testing.T) {
	prober := &recordingProber{result: okProbe()}
	e, b := settingsSignedIn(t, withProber(prober))
	srv := mustCreateServer(t, e, "alpha")
	id := string(srv.ID)

	bodies := map[string]string{
		"page":      b.get("/settings/servers").Body.String(),
		"new":       b.settingsDo("GET", "/settings/servers/new", nil).Body.String(),
		"edit":      b.settingsDo("GET", "/settings/servers/"+id+"/edit", nil).Body.String(),
		"row test":  b.settingsPost("/settings/servers/"+id+"/test", nil).Body.String(),
		"form test": b.settingsPost("/settings/servers/test", serverForm("alpha")).Body.String(),
		"enable":    b.settingsDo("PATCH", "/settings/servers/"+id, url.Values{"enabled": {"on"}}).Body.String(),
		"move":      b.settingsPost("/settings/servers/"+id+"/move/down", nil).Body.String(),
	}
	bad := serverForm("beta")
	bad.Set("port", "0")
	bodies["rejected create"] = b.settingsPost("/settings/servers", bad).Body.String()
	upd := serverForm("alpha")
	upd.Set("host", "bad host")
	bodies["rejected update"] = b.settingsPost("/settings/servers/"+id, upd).Body.String()
	bodies["saved update"] = b.settingsPost("/settings/servers/"+id, serverForm("alpha")).Body.String()

	for name, body := range bodies {
		assert.NotContains(t, body, serverPassword, name)
	}
	assert.NotContains(t, e.log.String(), serverPassword, "logs")
	pw := byID(parseHTML(t, bodies["edit"]), "server-password")
	require.NotNil(t, pw)
	assert.False(t, hasAttr(pw, "value"))
	assert.Contains(t, text(parseHTML(t, bodies["edit"])), "Leave empty to keep the stored password")
}

func TestEditFormShowsStoredValuesButNotThePassword(t *testing.T) {
	e, b := settingsSignedIn(t)
	srv := mustCreateServerAt(t, e, "alpha", 2)

	rec := b.settingsDo("GET", "/settings/servers/"+string(srv.ID)+"/edit", nil)

	doc := parseHTML(t, rec.Body.String())
	assert.Equal(t, "/settings/servers/"+string(srv.ID), attr(byID(doc, "server-form"), "hx-post"))
	assert.Equal(t, "alpha", attr(byID(doc, "server-name"), "value"))
	assert.Equal(t, "alpha.example.com", attr(byID(doc, "server-host"), "value"))
	assert.Equal(t, "joe", attr(byID(doc, "server-username"), "value"))
	assert.Equal(t, "2", attr(byID(doc, "server-priority"), "value"))
	assert.Equal(t, "Leave empty to keep it", attr(byID(doc, "server-password"), "placeholder"))
}

func TestEditingKeepsTheStoredPasswordWhenTheFieldIsEmpty(t *testing.T) {
	e, b := settingsSignedIn(t)
	srv := mustCreateServer(t, e, "alpha")
	form := serverForm("alpha")
	form.Set("password", "")
	form.Set("host", "new.example.com")
	form.Set("max_connections", "20")

	rec := b.settingsPost("/settings/servers/"+string(srv.ID), form)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := e.servers.Get(context.Background(), srv.ID)
	require.NoError(t, err)
	assert.Equal(t, "new.example.com", got.Host)
	assert.Equal(t, 20, got.MaxConnections)
	assert.Equal(t, serverPassword, got.Password.Reveal(), "password kept")
}

func TestEditingWithANewPasswordReplacesIt(t *testing.T) {
	e, b := settingsSignedIn(t)
	srv := mustCreateServer(t, e, "alpha")
	form := serverForm("alpha")
	form.Set("password", "another-password")

	rec := b.settingsPost("/settings/servers/"+string(srv.ID), form)

	require.Equal(t, http.StatusOK, rec.Code)
	got, _ := e.servers.Get(context.Background(), srv.ID)
	assert.Equal(t, "another-password", got.Password.Reveal())
}

func TestEditingValidationErrorsKeepTheDialogOpen(t *testing.T) {
	e, b := settingsSignedIn(t)
	srv := mustCreateServer(t, e, "alpha")
	other := mustCreateServer(t, e, "bravo")
	form := serverForm("alpha")
	form.Set("name", "bravo")
	form.Set("port", "99999")

	rec := b.settingsPost("/settings/servers/"+string(srv.ID), form)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, byID(doc, "server-port-error"))
	assert.Equal(t, "/settings/servers/"+string(srv.ID), attr(byID(doc, "server-form"), "hx-post"), "still editing the same server")

	form.Set("port", "563")
	rec = b.settingsPost("/settings/servers/"+string(srv.ID), form)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "server-name-error")), "already exists")
	_ = other
}

func TestUnknownServerIDsAre404ForEveryRoute(t *testing.T) {
	_, b := settingsSignedIn(t)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/settings/servers/nope/edit"},
		{"POST", "/settings/servers/nope"},
		{"PATCH", "/settings/servers/nope"},
		{"DELETE", "/settings/servers/nope"},
		{"POST", "/settings/servers/nope/move/up"},
		{"POST", "/settings/servers/nope/test"},
	} {
		rec := b.settingsDo(rt.method, rt.path, serverForm("alpha"))

		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", rt.method, rt.path)
		level, _ := settingsToastOf(t, rec)
		assert.Equal(t, "danger", level)
	}
	// The form test with an unknown saved server id too.
	form := serverForm("alpha")
	form.Set("server_id", "nope")
	form.Set("password", "")
	assert.Equal(t, http.StatusNotFound, b.settingsPost("/settings/servers/test", form).Code)
}

func TestEnabledSwitchTurnsAServerOnAndOff(t *testing.T) {
	e, b := settingsSignedIn(t)
	srv := mustCreateServer(t, e, "alpha")

	rec := b.settingsDo("PATCH", "/settings/servers/"+string(srv.ID), nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, _ := e.servers.Get(context.Background(), srv.ID)
	assert.False(t, got.Enabled)
	sw := find(parseHTML(t, rec.Body.String()), func(n *html.Node) bool { return attr(n, "role") == "switch" })
	assert.False(t, hasAttr(sw, "checked"))

	rec = b.settingsDo("PATCH", "/settings/servers/"+string(srv.ID), url.Values{"enabled": {"on"}})
	require.Equal(t, http.StatusOK, rec.Code)
	got, _ = e.servers.Get(context.Background(), srv.ID)
	assert.True(t, got.Enabled)
}

func TestDeletingAServerEmptiesTheListAndShowsTheEmptyState(t *testing.T) {
	e, b := settingsSignedIn(t)
	srv := mustCreateServer(t, e, "alpha")
	doc := parseHTML(t, b.get("/settings/servers").Body.String())
	del := find(doc, func(n *html.Node) bool { return n.Data == "button" && attr(n, "hx-delete") != "" })
	require.NotNil(t, del)
	assert.Contains(t, attr(del, "hx-confirm"), "alpha")
	assert.Equal(t, "danger", attr(del, "data-confirm-tone"))

	rec := b.settingsDo("DELETE", "/settings/servers/"+string(srv.ID), nil)

	require.Equal(t, http.StatusOK, rec.Code)
	list, _ := e.servers.List(context.Background())
	assert.Empty(t, list)
	assert.Contains(t, text(parseHTML(t, rec.Body.String())), "No Usenet servers yet")
}

func TestMovingServersSwapsTheirPriorities(t *testing.T) {
	e, b := settingsSignedIn(t)
	a := mustCreateServerAt(t, e, "alpha", 0)
	bb := mustCreateServerAt(t, e, "bravo", 1)
	c := mustCreateServerAt(t, e, "charlie", 2)
	_ = a

	rec := b.settingsPost("/settings/servers/"+string(c.ID)+"/move/up", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"alpha:0", "charlie:1", "bravo:2"}, serverOrder(t, e))
	rows := serverRows(parseHTML(t, rec.Body.String()))
	require.Len(t, rows, 3)
	assert.Contains(t, text(rows[1]), "charlie")

	rec = b.settingsPost("/settings/servers/"+string(bb.ID)+"/move/down", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"alpha:0", "charlie:1", "bravo:2"}, serverOrder(t, e), "the last one stays last")
}

func TestMovingServersWithTheSamePriorityOrdersThemWithoutTouchingOthers(t *testing.T) {
	e, b := settingsSignedIn(t)
	mustCreateServerAt(t, e, "alpha", 1)
	mustCreateServerAt(t, e, "bravo", 1)
	c := mustCreateServerAt(t, e, "charlie", 1)
	mustCreateServerAt(t, e, "delta", 5)

	rec := b.settingsPost("/settings/servers/"+string(c.ID)+"/move/up", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"alpha:1", "charlie:1", "bravo:2", "delta:5"}, serverOrder(t, e))
}

func TestMoveRejectsAnythingButUpAndDown(t *testing.T) {
	e, b := settingsSignedIn(t)
	srv := mustCreateServer(t, e, "alpha")

	rec := b.settingsPost("/settings/servers/"+string(srv.ID)+"/move/sideways", nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAServerWithAnUnreadablePasswordIsFlaggedAndCanBeFixed(t *testing.T) {
	prober := &recordingProber{result: okProbe()}
	e, b := settingsSignedIn(t, withProber(prober))
	srv := mustCreateServer(t, e, "alpha")
	e.breakServerKey()
	id := string(srv.ID)

	doc := parseHTML(t, b.get("/settings/servers").Body.String())
	row := serverRows(doc)[0]
	assert.Contains(t, text(row), "Password unavailable")
	reenter := find(row, func(n *html.Node) bool { return n.Data == "button" && text(n) == "Re-enter password" })
	require.NotNil(t, reenter)
	assert.Equal(t, "/settings/servers/"+id+"/edit?reenter=1", attr(reenter, "hx-get"))

	form := parseHTML(t, b.settingsDo("GET", "/settings/servers/"+id+"/edit?reenter=1", nil).Body.String())
	assert.Contains(t, text(form), "The stored password cannot be read")
	assert.Contains(t, text(byID(form, "server-password-error")), "Enter the password again")

	// The row test explains instead of failing.
	test := b.settingsPost("/settings/servers/"+id+"/test", nil)
	assert.Equal(t, http.StatusOK, test.Code)
	assert.Contains(t, test.Body.String(), "Password needs to be entered again")
	assert.Empty(t, prober.probed, "no connection is tried without a password")

	// Saving without a password does not work, with one it does.
	noPw := serverForm("alpha")
	noPw.Set("password", "")
	rec := b.settingsPost("/settings/servers/"+id, noPw)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "server-password-error"))

	withPw := serverForm("alpha")
	withPw.Set("password", "fresh-password")
	rec = b.settingsPost("/settings/servers/"+id, withPw)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := e.servers.Get(context.Background(), srv.ID)
	require.NoError(t, err)
	assert.False(t, got.PasswordUnavailable)
	assert.Equal(t, "fresh-password", got.Password.Reveal())
	assert.NotContains(t, text(parseHTML(t, rec.Body.String())), "Password unavailable")
}

func TestHTMLPostsWithoutHTMXRedirectOrExplainTheError(t *testing.T) {
	e, b := settingsSignedIn(t)
	form := serverForm("alpha")

	rec := b.post("/settings/servers", form)

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/settings/servers", location(rec))
	list, _ := e.servers.List(context.Background())
	assert.Len(t, list, 1)

	form.Set("name", "beta")
	form.Set("port", "0")
	rec = b.post("/settings/servers", form)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "The server was not saved")
}

// ---- connection tests ---------------------------------------------------

func TestTheTestButtonShowsTheSpecificMessageForEachFailureClass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result app.ProbeResult
		title  string
		body   string
	}{
		{"dns not found", app.ProbeResult{Class: app.ProbeDNS, Detail: "not_found", Message: "the host name does not exist"}, "Host name not found", "alpha.example.com"},
		{"dns timeout", app.ProbeResult{Class: app.ProbeDNS, Detail: "timeout"}, "Host name lookup timed out", "DNS"},
		{"tcp refused", app.ProbeResult{Class: app.ProbeTCP, Detail: "refused"}, "Connection refused", "alpha.example.com:563"},
		{"tcp timeout", app.ProbeResult{Class: app.ProbeTCP, Detail: "timeout"}, "Connection timed out", "firewall"},
		{"tcp unreachable", app.ProbeResult{Class: app.ProbeTCP, Detail: "unreachable"}, "Host unreachable", "route"},
		{"tls untrusted", app.ProbeResult{Class: app.ProbeTLS, Detail: "certificate_untrusted"}, "Certificate not trusted", "never skips certificate checks"},
		{"tls hostname", app.ProbeResult{Class: app.ProbeTLS, Detail: "certificate_hostname"}, "Certificate is for a different host name", "never skips certificate checks"},
		{"tls expired", app.ProbeResult{Class: app.ProbeTLS, Detail: "certificate_expired"}, "Certificate expired or not valid yet", "clock"},
		{"tls invalid", app.ProbeResult{Class: app.ProbeTLS, Detail: "certificate_invalid"}, "Certificate not valid", "never skips certificate checks"},
		{"tls not tls", app.ProbeResult{Class: app.ProbeTLS, Detail: "not_tls"}, "That port does not speak TLS", "only connects over TLS"},
		{"tls handshake", app.ProbeResult{Class: app.ProbeTLS, Detail: "handshake"}, "Secure connection failed", "TLS handshake"},
		{"auth", app.ProbeResult{Class: app.ProbeAuth, Detail: "rejected", Message: "the server rejected the user name or password (481)"}, "Login rejected", "user name or password"},
		{"protocol", app.ProbeResult{Class: app.ProbeProtocol, Detail: "greeting", Message: "unexpected response from the server (502 go away)"}, "Unexpected reply from the server", "NNTP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prober := &recordingProber{result: tc.result}
			_, b := settingsSignedIn(t, withProber(prober))

			rec := b.settingsPost("/settings/servers/test", serverForm("alpha"))

			require.Equal(t, http.StatusOK, rec.Code)
			doc := parseHTML(t, rec.Body.String())
			res := find(doc, func(n *html.Node) bool { return attr(n, "data-probe") == "failed" })
			require.NotNil(t, res, rec.Body.String())
			assert.Equal(t, "alert", attr(res, "role"))
			assert.Contains(t, text(res), tc.title)
			assert.Contains(t, text(res), tc.body)
			if tc.result.Class == app.ProbeAuth || tc.result.Class == app.ProbeProtocol {
				assert.Contains(t, text(res), tc.result.Message)
			}
			// Exactly the failing step is marked failed, the earlier ones passed.
			steps := findAll(res, func(n *html.Node) bool { return n.Data == "li" })
			require.Len(t, steps, 5)
			var states []string
			for _, s := range steps {
				states = append(states, strings.TrimSpace(strings.SplitN(text(s), ":", 2)[1]))
			}
			failed := strings.Count(strings.Join(states, ","), "failed")
			assert.Equal(t, 1, failed, "%v", states)
		})
	}
}

func TestTheTestButtonShowsSuccessWithTimings(t *testing.T) {
	prober := &recordingProber{result: okProbe()}
	_, b := settingsSignedIn(t, withProber(prober))

	rec := b.settingsPost("/settings/servers/test", serverForm("alpha"))

	res := find(parseHTML(t, rec.Body.String()), func(n *html.Node) bool { return attr(n, "data-probe") == "ok" })
	require.NotNil(t, res)
	assert.Equal(t, "status", attr(res, "role"))
	assert.Contains(t, text(res), "Connection works")
	assert.Contains(t, text(res), "123 ms")
	assert.Contains(t, text(res), "45 ms")
	probed := prober.last(t)
	assert.Equal(t, "alpha.example.com", probed.Host)
	assert.Equal(t, serverPassword, probed.Password.Reveal())
}

func TestTestingNeedsValidConnectionDetailsButNotAName(t *testing.T) {
	prober := &recordingProber{result: okProbe()}
	_, b := settingsSignedIn(t, withProber(prober))
	form := serverForm("alpha")
	form.Set("name", "")
	form.Set("max_connections", "")

	rec := b.settingsPost("/settings/servers/test", form)
	assert.Contains(t, rec.Body.String(), "Connection works", "name and limits do not matter for a test")

	form.Set("host", "https://bad")
	rec = b.settingsPost("/settings/servers/test", form)
	assert.Contains(t, text(parseHTML(t, rec.Body.String())), "Fix the connection details first")
	assert.Len(t, prober.probed, 1, "no probe for invalid details")
}

func TestTestingASavedServerWithAnEmptyPasswordUsesTheStoredOne(t *testing.T) {
	prober := &recordingProber{result: okProbe()}
	e, b := settingsSignedIn(t, withProber(prober))
	srv := mustCreateServer(t, e, "alpha")
	form := serverForm("alpha")
	form.Set("password", "")
	form.Set("server_id", string(srv.ID))

	rec := b.settingsPost("/settings/servers/test", form)
	require.Contains(t, rec.Body.String(), "Connection works")
	assert.Equal(t, serverPassword, prober.last(t).Password.Reveal())

	// Edited details are tested with the stored password too.
	form.Set("host", "other.example.com")
	rec = b.settingsPost("/settings/servers/test", form)
	require.Contains(t, rec.Body.String(), "Connection works")
	got := prober.last(t)
	assert.Equal(t, "other.example.com", got.Host)
	assert.Equal(t, serverPassword, got.Password.Reveal())
}

func TestTestingFromTheRowUsesTheSavedServer(t *testing.T) {
	prober := &recordingProber{result: app.ProbeResult{Class: app.ProbeAuth, Detail: "rejected", Message: "nope"}}
	e, b := settingsSignedIn(t, withProber(prober))
	srv := mustCreateServer(t, e, "alpha")

	rec := b.settingsPost("/settings/servers/"+string(srv.ID)+"/test", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, text(parseHTML(t, rec.Body.String())), "Login rejected")
	assert.Equal(t, "alpha", prober.last(t).Name)
	assert.Equal(t, serverPassword, prober.last(t).Password.Reveal())
}
