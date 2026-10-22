package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

const serverPassword = "pw-SECRET-4711"

// fakeProber answers every probe with result and records what it probed.
type fakeProber struct {
	mu     sync.Mutex
	result app.ProbeResult
	probed []core.Server
}

func (p *fakeProber) Probe(_ context.Context, s core.Server) app.ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probed = append(p.probed, s)
	return p.result
}

type serverJSON struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Host                string `json:"host"`
	Port                int    `json:"port"`
	Username            string `json:"username"`
	HasPassword         bool   `json:"has_password"`
	PasswordUnavailable bool   `json:"password_unavailable"`
	MaxConnections      int    `json:"max_connections"`
	RetentionDays       int    `json:"retention_days"`
	Priority            int    `json:"priority"`
	Enabled             bool   `json:"enabled"`
	CreatedAt           string `json:"created_at"`
}

func (e *env) createServer(key, body string) serverJSON {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/servers", body, withBearer(key))
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	var s serverJSON
	decode(e.t, rec, &s)
	return s
}

func newServerBody(name string, extra string) string {
	return `{"name":"` + name + `","host":"` + name + `.example.com","username":"joe","password":"` + serverPassword + `"` + extra + `}`
}

func TestCreateServerReturns201WithDefaultsAndNoPassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()

	rec := e.do("POST", "/api/v1/servers", newServerBody("main", ""), withBearer(key))

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var got serverJSON
	decode(t, rec, &got)
	assert.NotEmpty(t, got.ID)
	assert.Equal(t, "main", got.Name)
	assert.Equal(t, "main.example.com", got.Host)
	assert.Equal(t, 563, got.Port)
	assert.Equal(t, "joe", got.Username)
	assert.True(t, got.HasPassword)
	assert.False(t, got.PasswordUnavailable)
	assert.Equal(t, 8, got.MaxConnections)
	assert.True(t, got.Enabled)
	assert.Equal(t, epoch.Add(24*time.Hour).Format(time.RFC3339), got.CreatedAt)
	stored, err := e.servers.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, serverPassword, stored.Password.Reveal())
}

func TestServerResponseShapeHasNoPasswordAndNoTLSSwitches(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()

	rec := e.do("POST", "/api/v1/servers", newServerBody("main", ""), withBearer(key))

	var fields map[string]json.RawMessage
	decode(t, rec, &fields)
	var names []string
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"created_at", "enabled", "has_password", "host", "id", "max_connections",
		"name", "password_unavailable", "port", "priority", "retention_days", "username"}, names)
}

func TestServerRequestsRejectTLSWeakeningFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	for _, field := range []string{"tls", "ssl", "insecure", "skip_verify", "tls_skip_verify", "plaintext", "verify_certificate"} {
		rec := e.do("POST", "/api/v1/servers", newServerBody("x", `,"`+field+`":true`), withBearer(key))
		assert.Equal(t, http.StatusBadRequest, rec.Code, field)
		assert.Equal(t, "invalid_json", errorOf(t, rec), field)
	}
	main := e.createServer(key, newServerBody("main", ""))
	rec := e.do("PATCH", "/api/v1/servers/"+main.ID, `{"insecure":true}`, withBearer(key))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServerPasswordNeverAppearsInResponsesOrLogs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	var bodies []string
	add := func(rec interface{ String() string }) { bodies = append(bodies, rec.String()) }

	created := e.do("POST", "/api/v1/servers", newServerBody("main", ""), withBearer(key))
	var s serverJSON
	decode(t, created, &s)
	add(created.Body)
	add(e.do("GET", "/api/v1/servers", "", withBearer(key)).Body)
	add(e.do("GET", "/api/v1/servers/"+s.ID, "", withBearer(key)).Body)
	add(e.do("PATCH", "/api/v1/servers/"+s.ID, `{"password":"`+serverPassword+`-2","host":"x.example.com"}`, withBearer(key)).Body)
	add(e.do("POST", "/api/v1/servers/"+s.ID+"/test", "", withBearer(key)).Body)
	add(e.do("POST", "/api/v1/servers/test", newServerBody("t", ""), withBearer(key)).Body)
	// Invalid input must not echo the password either.
	add(e.do("POST", "/api/v1/servers", newServerBody("main", `,"port":0,"host":""`), withBearer(key)).Body)
	add(e.do("POST", "/api/v1/servers", `{"name":"x","host":"h","password":"`+serverPassword+`","bogus":1}`, withBearer(key)).Body)

	for _, b := range bodies {
		assert.NotContains(t, b, serverPassword)
	}
	assert.NotContains(t, e.log.String(), serverPassword)
}

func TestCreateServerRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	e.createServer(key, newServerBody("main", ""))
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"missing name":    {`{"host":"h.example.com"}`, 400, "invalid_name"},
		"missing host":    {`{"name":"x"}`, 400, "invalid_host"},
		"url host":        {`{"name":"x","host":"https://h.example.com"}`, 400, "invalid_host"},
		"port":            {`{"name":"x","host":"h","port":99999}`, 400, "invalid_port"},
		"connections":     {`{"name":"x","host":"h","max_connections":500}`, 400, "invalid_max_connections"},
		"retention":       {`{"name":"x","host":"h","retention_days":-5}`, 400, "invalid_retention_days"},
		"priority":        {`{"name":"x","host":"h","priority":-1}`, 400, "invalid_priority"},
		"duplicate name":  {`{"name":"main","host":"h"}`, 409, "name_taken"},
		"wrong json type": {`{"name":"x","host":"h","port":"563"}`, 400, "invalid_json"},
		"password type":   {`{"name":"x","host":"h","password":5}`, 400, "invalid_json"},
		"unknown field":   {`{"name":"x","host":"h","id":"abc"}`, 400, "invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("POST", "/api/v1/servers", tc.body, withBearer(key))

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
		})
	}
}

func TestListServersIsOrderedByPriorityThenNameAndWorks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	empty := e.do("GET", "/api/v1/servers", "", withBearer(key))
	e.createServer(key, newServerBody("zeta", `,"priority":0`))
	e.createServer(key, newServerBody("fill", `,"priority":1,"password":""`))
	e.createServer(key, newServerBody("alpha", `,"priority":0`))

	rec := e.do("GET", "/api/v1/servers", "", withBearer(key))

	assert.JSONEq(t, `{"items":[]}`, empty.Body.String())
	var out struct {
		Items []serverJSON `json:"items"`
	}
	decode(t, rec, &out)
	var names []string
	for _, s := range out.Items {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{"alpha", "zeta", "fill"}, names)
	assert.False(t, out.Items[2].HasPassword, "an empty password is no password")
	assert.Equal(t, http.StatusUnauthorized, e.do("GET", "/api/v1/servers", "").Code)
}

func TestGetServer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	s := e.createServer(key, newServerBody("main", ""))

	rec := e.do("GET", "/api/v1/servers/"+s.ID, "", withBearer(key))
	missing := e.do("GET", "/api/v1/servers/nope", "", withBearer(key))

	require.Equal(t, http.StatusOK, rec.Code)
	var got serverJSON
	decode(t, rec, &got)
	assert.Equal(t, s, got)
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

func TestPatchServerKeepsThePasswordUnlessOneIsGiven(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	s := e.createServer(key, newServerBody("main", ""))
	path := "/api/v1/servers/" + s.ID

	renamed := e.do("PATCH", path, `{"name":"renamed","port":443,"enabled":false,"priority":2,"max_connections":3,"retention_days":100}`, withBearer(key))
	stored, err := e.servers.Get(context.Background(), core.ID(s.ID))
	require.NoError(t, err)
	repl := e.do("PATCH", path, `{"password":"other"}`, withBearer(key))
	replaced, err := e.servers.Get(context.Background(), core.ID(s.ID))
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, renamed.Code, renamed.Body.String())
	var got serverJSON
	decode(t, renamed, &got)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, 443, got.Port)
	assert.False(t, got.Enabled)
	assert.Equal(t, 2, got.Priority)
	assert.Equal(t, 3, got.MaxConnections)
	assert.Equal(t, 100, got.RetentionDays)
	assert.True(t, got.HasPassword)
	assert.Equal(t, serverPassword, stored.Password.Reveal(), "a PATCH without a password keeps it")
	assert.Equal(t, http.StatusOK, repl.Code)
	assert.Equal(t, "other", replaced.Password.Reveal())
}

func TestPatchServerRejectsInvalidChanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	a := e.createServer(key, newServerBody("a", ""))
	e.createServer(key, newServerBody("b", ""))
	path := "/api/v1/servers/" + a.ID

	for name, tc := range map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"empty":     {path, `{}`, 400, "empty_patch"},
		"bad host":  {path, `{"host":"a b"}`, 400, "invalid_host"},
		"bad port":  {path, `{"port":0}`, 400, "invalid_port"},
		"taken":     {path, `{"name":"b"}`, 409, "name_taken"},
		"not found": {"/api/v1/servers/nope", `{"name":"x"}`, 404, "not_found"},
		"null":      {path, `{"name":null}`, 400, "empty_patch"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("PATCH", tc.path, tc.body, withBearer(key))

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
		})
	}
}

func TestDeleteServer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	s := e.createServer(key, newServerBody("main", ""))

	rec := e.do("DELETE", "/api/v1/servers/"+s.ID, "", withBearer(key))
	again := e.do("DELETE", "/api/v1/servers/"+s.ID, "", withBearer(key))

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, http.StatusNotFound, again.Code)
}

func TestServerChangesAreAnnouncedOnTheEventStream(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	events := e.subscribe()

	s := e.createServer(key, newServerBody("main", ""))
	e.do("PATCH", "/api/v1/servers/"+s.ID, `{"name":"x"}`, withBearer(key))
	e.do("DELETE", "/api/v1/servers/"+s.ID, "", withBearer(key))

	got := events()
	require.Len(t, got, 3)
	for _, ev := range got {
		assert.Equal(t, core.KindServersChanged, ev.Kind())
	}
}

func TestTestServerReportsTheProbeResult(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	s := e.createServer(key, newServerBody("main", ""))
	e.prober.result = app.ProbeResult{OK: true, Class: app.ProbeOK, Message: "connected and authenticated",
		ConnectTime: 123 * time.Millisecond, Latency: 45 * time.Millisecond}

	rec := e.do("POST", "/api/v1/servers/"+s.ID+"/test", "", withBearer(key))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"ok":true,"class":"ok","detail":"","message":"connected and authenticated","connect_ms":123,"latency_ms":45}`, rec.Body.String())
	require.Len(t, e.prober.probed, 1)
	assert.Equal(t, serverPassword, e.prober.probed[0].Password.Reveal())
}

func TestTestServerReportsAFailureClassWith200(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	s := e.createServer(key, newServerBody("main", ""))
	e.prober.result = app.ProbeResult{Class: app.ProbeTLS, Detail: "certificate_untrusted", Message: "the server's certificate is not trusted"}

	rec := e.do("POST", "/api/v1/servers/"+s.ID+"/test", "", withBearer(key))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"ok":false,"class":"tls","detail":"certificate_untrusted","message":"the server's certificate is not trusted","connect_ms":0,"latency_ms":0}`, rec.Body.String())
}

func TestTestUnknownServerIs404AndUnreadablePasswordIs409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	s := e.createServer(key, newServerBody("main", ""))
	// Another key, as after losing the key file.
	cfg := e.cfg
	cfg.Servers = app.NewServerService(store.NewServerRepo(e.db, store.GenerateKey()), e.bus, e.prober)
	restarted, err := api.New(cfg)
	require.NoError(t, err)
	other := *e
	other.server = restarted

	missing := e.do("POST", "/api/v1/servers/nope/test", "", withBearer(key))
	unreadable := other.do("POST", "/api/v1/servers/"+s.ID+"/test", "", withBearer(key))
	listed := other.do("GET", "/api/v1/servers", "", withBearer(key))

	assert.Equal(t, http.StatusNotFound, missing.Code)
	assert.Equal(t, http.StatusConflict, unreadable.Code)
	assert.Equal(t, "password_unavailable", errorOf(t, unreadable))
	var out struct {
		Items []serverJSON `json:"items"`
	}
	decode(t, listed, &out)
	require.Len(t, out.Items, 1)
	assert.True(t, out.Items[0].PasswordUnavailable)
	assert.True(t, out.Items[0].HasPassword)
}

func TestTestUnsavedServerValidatesAndSavesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	e.prober.result = app.ProbeResult{Class: app.ProbeAuth, Detail: "rejected", Message: "the server rejected the user name or password (481)"}

	rec := e.do("POST", "/api/v1/servers/test", `{"host":"news.example.com","username":"joe","password":"x"}`, withBearer(key))
	bad := e.do("POST", "/api/v1/servers/test", `{"host":"news.example.com/x"}`, withBearer(key))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		OK    bool   `json:"ok"`
		Class string `json:"class"`
	}
	decode(t, rec, &got)
	assert.False(t, got.OK)
	assert.Equal(t, "auth", got.Class)
	assert.Equal(t, http.StatusBadRequest, bad.Code)
	assert.Equal(t, "invalid_host", errorOf(t, bad))
	list, err := e.servers.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
	require.Len(t, e.prober.probed, 1)
	assert.Equal(t, 563, e.prober.probed[0].Port)
}

func TestServerWriteRoutesNeedTheKeyOrASessionWithCSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.createServer(e.apiKey(), newServerBody("main", ""))
	sess := e.login()
	routes := []struct{ method, path, body string }{
		{"POST", "/api/v1/servers", newServerBody("x", "")},
		{"PATCH", "/api/v1/servers/" + s.ID, `{"name":"y"}`},
		{"DELETE", "/api/v1/servers/" + s.ID, ""},
		{"POST", "/api/v1/servers/" + s.ID + "/test", ""},
		{"POST", "/api/v1/servers/test", `{"host":"h"}`},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			anonymous := e.do(rt.method, rt.path, rt.body)
			noToken := e.do(rt.method, rt.path, rt.body, withCookie(sess.cookie))

			assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
			require.Equal(t, http.StatusForbidden, noToken.Code)
			assert.Equal(t, "csrf_token_invalid", errorOf(t, noToken))
		})
	}
	// A session with the token may use them.
	rec := e.do("POST", "/api/v1/servers/test", `{"host":"h.example.com"}`, sess.mods()...)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, strings.Contains(rec.Body.String(), "error"))
}

func TestOversizedServerBodyIs413(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()

	rec := e.do("POST", "/api/v1/servers", `{"name":"`+strings.Repeat("x", 20<<10)+`"}`, withBearer(key))

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}
