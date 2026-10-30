package blackbox_test

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

const (
	srvsPath         = "/api/v1/servers"
	serverNameTaken  = "a server with this name already exists"
	secondPassword   = "an-entirely-different-password"
	passwordUnavMsg  = "the stored password cannot be decrypted: enter the password again"
	serverEmptyPatch = "nothing to change: give at least one field"
)

func srvPath(id string) string { return srvsPath + "/" + id }

// serverFields are the keys of a Server answer, and the only ones.
var serverFields = []string{"created_at", "enabled", "has_password", "host", "id", "max_connections",
	"name", "password_unavailable", "port", "priority", "retention_days", "username"}

func requireServerShape(t *testing.T, m map[string]any) {
	t.Helper()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, serverFields, keys, "a Server answer has these fields; no password, no TLS switches")
}

func createSrv(t *testing.T, d *daemon, body string) map[string]any {
	t.Helper()
	r := d.api(http.MethodPost, srvsPath, body)
	require.Equal(t, http.StatusCreated, r.status, r.body)
	requireAPIHeaders(t, r)
	m := obj(t, r)
	requireServerShape(t, m)
	return m
}

func TestAPIServersListTheStoredOneWithoutItsPassword(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.apiGet(srvsPath)

	requireOK(t, r)
	items := itemsOf(t, r)
	require.Len(t, items, 1)
	requireServerShape(t, items[0])
	assert.NotContains(t, r.body, serverPassword)
	one := d.apiGet(srvPath(string(d.cfg.ServerID)))
	requireOK(t, one)
	assert.JSONEq(t, jsonOf(t, items[0]), one.body, "the list and the single answer show the same Server")
}

func TestAPIServerCreateAppliesTheDefaults(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	before := time.Now().Add(-5 * time.Second)

	m := createSrv(t, d, `{"name":"  spaced name  ","host":"  news.example.org  "}`)

	assert.Regexp(t, `^[a-z0-9]{20,}$`, m["id"])
	assert.Equal(t, "spaced name", m["name"], "the name is trimmed")
	assert.Equal(t, "news.example.org", m["host"], "so is the host")
	assert.EqualValues(t, 563, m["port"])
	assert.Equal(t, "", m["username"])
	assert.Equal(t, false, m["has_password"])
	assert.Equal(t, false, m["password_unavailable"])
	assert.EqualValues(t, 8, m["max_connections"])
	assert.EqualValues(t, 0, m["retention_days"])
	assert.EqualValues(t, 0, m["priority"])
	assert.Equal(t, true, m["enabled"])
	assert.True(t, parseTime(t, m["created_at"]).After(before))
	d.stop()
	key, err := store.LoadOrCreateKey(cfg.KeyFile, discard)
	require.NoError(t, err)
	db := openDB(t, cfg)
	defer func() { require.NoError(t, db.Close()) }()
	srv, err := store.NewServerRepo(db, key).Get(t.Context(), core.ID(m["id"].(string)))
	require.NoError(t, err)
	assert.Equal(t, "spaced name", srv.Name)
	assert.Equal(t, 563, srv.Port)
	assert.Empty(t, srv.Password.Reveal())
}

func TestAPIServerCreateTreatsZeroPortAndConnectionsAsTheDefaults(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	m := createSrv(t, d, `{"name":"zeros","host":"h.example","port":0,"max_connections":0,"enabled":null}`)

	assert.EqualValues(t, 563, m["port"])
	assert.EqualValues(t, 8, m["max_connections"])
	assert.Equal(t, true, m["enabled"])
}

func TestAPIServerCreateTakesEveryField(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	m := createSrv(t, d, `{"name":"full","host":"203.0.113.9","port":119,"username":"me","password":"pw","max_connections":100,`+
		`"retention_days":4000,"priority":7,"enabled":false}`)

	assert.Equal(t, "203.0.113.9", m["host"])
	assert.EqualValues(t, 119, m["port"])
	assert.Equal(t, "me", m["username"])
	assert.Equal(t, true, m["has_password"])
	assert.EqualValues(t, 100, m["max_connections"])
	assert.EqualValues(t, 4000, m["retention_days"])
	assert.EqualValues(t, 7, m["priority"])
	assert.Equal(t, false, m["enabled"])
	got := d.apiGet(srvPath(m["id"].(string)))
	requireOK(t, got)
	assert.Equal(t, false, obj(t, got)["enabled"])
}

func TestAPIServerHostsAcceptNamesAndAddresses(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	for i, h := range []string{"news", "a.b.c", "my_service", "1.2.3.4", "::1", "2001:db8::1", "A-b.Example", strings.Repeat("a", 63) + ".example"} {
		r := d.api(http.MethodPost, srvsPath, `{"name":"h`+string(rune('a'+i))+`","host":"`+h+`"}`)
		assert.Equal(t, http.StatusCreated, r.status, "%q: %s", h, r.body)
	}
}

func TestAPIServerCreateRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	long := strings.Repeat("a", 65)

	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"missing name":          {`{"host":"h.example"}`, 400, "invalid_name"},
		"blank name":            {`{"name":"   ","host":"h.example"}`, 400, "invalid_name"},
		"long name":             {`{"name":"` + long + `","host":"h.example"}`, 400, "invalid_name"},
		"control in name":       {`{"name":"a\tb","host":"h.example"}`, 400, "invalid_name"},
		"missing host":          {`{"name":"x"}`, 400, "invalid_host"},
		"url host":              {`{"name":"x","host":"https://h.example"}`, 400, "invalid_host"},
		"host with port":        {`{"name":"x","host":"h.example:563"}`, 400, "invalid_host"},
		"host with path":        {`{"name":"x","host":"h.example/x"}`, 400, "invalid_host"},
		"host with space":       {`{"name":"x","host":"a b"}`, 400, "invalid_host"},
		"host leading dash":     {`{"name":"x","host":"-a.example"}`, 400, "invalid_host"},
		"host trailing dash":    {`{"name":"x","host":"a-.example"}`, 400, "invalid_host"},
		"host empty label":      {`{"name":"x","host":"a..example"}`, 400, "invalid_host"},
		"host trailing dot":     {`{"name":"x","host":"a.example."}`, 400, "invalid_host"},
		"host non ascii":        {`{"name":"x","host":"bücher.example"}`, 400, "invalid_host"},
		"host long label":       {`{"name":"x","host":"` + strings.Repeat("a", 64) + `.example"}`, 400, "invalid_host"},
		"host too long":         {`{"name":"x","host":"` + strings.Repeat("a.", 127) + `a"}`, 400, "invalid_host"},
		"bracketed ipv6":        {`{"name":"x","host":"[::1]"}`, 400, "invalid_host"},
		"ipv6 with zone":        {`{"name":"x","host":"fe80::1%eth0"}`, 400, "invalid_host"},
		"port too high":         {`{"name":"x","host":"h","port":65536}`, 400, "invalid_port"},
		"port negative":         {`{"name":"x","host":"h","port":-1}`, 400, "invalid_port"},
		"too many connections":  {`{"name":"x","host":"h","max_connections":101}`, 400, "invalid_max_connections"},
		"negative connections":  {`{"name":"x","host":"h","max_connections":-1}`, 400, "invalid_max_connections"},
		"negative retention":    {`{"name":"x","host":"h","retention_days":-5}`, 400, "invalid_retention_days"},
		"negative priority":     {`{"name":"x","host":"h","priority":-1}`, 400, "invalid_priority"},
		"long username":         {`{"name":"x","host":"h","username":"` + strings.Repeat("u", 257) + `"}`, 400, "invalid_credentials"},
		"control in username":   {`{"name":"x","host":"h","username":"a\nb"}`, 400, "invalid_credentials"},
		"long password":         {`{"name":"x","host":"h","password":"` + strings.Repeat("p", 257) + `"}`, 400, "invalid_credentials"},
		"control in password":   {`{"name":"x","host":"h","password":"a\u0001b"}`, 400, "invalid_credentials"},
		"name before host":      {`{"name":"","host":"","port":0}`, 400, "invalid_name"},
		"host before port":      {`{"name":"x","host":"","port":-1}`, 400, "invalid_host"},
		"port before conns":     {`{"name":"x","host":"h","port":-1,"max_connections":-1}`, 400, "invalid_port"},
		"conns before priority": {`{"name":"x","host":"h","max_connections":-1,"priority":-1}`, 400, "invalid_max_connections"},
		"priority before creds": {`{"name":"x","host":"h","priority":-1,"username":"\n"}`, 400, "invalid_priority"},
		"duplicate name":        {`{"name":"` + serverName + `","host":"h.example"}`, 409, "name_taken"},
		"duplicate trimmed":     {`{"name":"  ` + serverName + `  ","host":"h.example"}`, 409, "name_taken"},
		"port as string":        {`{"name":"x","host":"h","port":"563"}`, 400, "invalid_json"},
		"port fractional":       {`{"name":"x","host":"h","port":563.5}`, 400, "invalid_json"},
		"password as number":    {`{"name":"x","host":"h","password":5}`, 400, "invalid_json"},
		"enabled as string":     {`{"name":"x","host":"h","enabled":"yes"}`, 400, "invalid_json"},
		"id is not a field":     {`{"name":"x","host":"h","id":"abc"}`, 400, "invalid_json"},
		"created_at not field":  {`{"name":"x","host":"h","created_at":"2020-01-01T00:00:00Z"}`, 400, "invalid_json"},
		"has_password no field": {`{"name":"x","host":"h","has_password":true}`, 400, "invalid_json"},
		"array":                 {`[]`, 400, "invalid_json"},
		"null":                  {`null`, 400, "invalid_name"},
	} {
		t.Run(name, func(t *testing.T) {
			r := d.api(http.MethodPost, srvsPath, tc.body)

			requireError(t, r, tc.status, tc.code)
		})
	}
	assert.Len(t, itemsOf(t, d.apiGet(srvsPath)), 1, "nothing was created")
}

func TestAPIServerCreateRefusesEveryFieldThatWouldWeakenTLS(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for _, f := range []string{"tls", "ssl", "insecure", "skip_verify", "tls_skip_verify", "plaintext", "verify_certificate"} {
		r := d.api(http.MethodPost, srvsPath, `{"name":"x","host":"h.example","`+f+`":true}`)
		requireError(t, r, http.StatusBadRequest, "invalid_json")
		p := d.api(http.MethodPatch, srvPath(string(d.cfg.ServerID)), `{"`+f+`":true}`)
		requireError(t, p, http.StatusBadRequest, "invalid_json")
	}
}

func TestAPIServerErrorMessagesWrittenByTheApp(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	requireErrorMessage(t, d.api(http.MethodPost, srvsPath, `{"name":"`+serverName+`","host":"h"}`), 409, "name_taken", serverNameTaken)
	requireErrorMessage(t, d.api(http.MethodPost, srvsPath, `{"name":"x","host":"h","port":99999}`), 400, "invalid_port",
		"server port must be 1 to 65535: 99999")
	requireErrorMessage(t, d.api(http.MethodPost, srvsPath, `{"name":"x","host":"h","max_connections":500}`), 400, "invalid_max_connections",
		"server max connections must be 1 to 100: 500")
	requireErrorMessage(t, d.api(http.MethodPost, srvsPath, `{"name":"x","host":"h","retention_days":-1,"priority":-1}`), 400, "invalid_retention_days",
		"server retention days must not be negative; server priority must not be negative")
	requireErrorMessage(t, d.api(http.MethodPatch, srvPath(string(d.cfg.ServerID)), `{}`), 400, "empty_patch", serverEmptyPatch)
	requireErrorMessage(t, d.request(http.MethodPost, srvsPath, d.apiHeaders(), ""), 400, "invalid_json", "the request body is empty")
}

func TestAPIServersAreListedByPriorityThenName(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	createSrv(t, d, `{"name":"zeta","host":"h","priority":0}`)
	createSrv(t, d, `{"name":"fill","host":"h","priority":2}`)
	createSrv(t, d, `{"name":"alpha","host":"h","priority":0}`)
	createSrv(t, d, `{"name":"Beta","host":"h","priority":1}`)

	got := namesOfItems(itemsOf(t, d.apiGet(srvsPath)))

	assert.Equal(t, []string{"alpha", "zeta", "Beta", serverName, "fill"}, got)
}

func TestAPIServerGetOfAnUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.apiGet(srvPath("nope"))

	assert.Equal(t, http.StatusNotFound, r.status)
	assert.Equal(t, errBody("not_found", "not found"), r.body)
}

func TestAPIServerPasswordNeverAppearsInAnAnswerOrTheLog(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	var bodies []string
	add := func(r response) { bodies = append(bodies, r.body) }

	created := d.api(http.MethodPost, srvsPath, `{"name":"main","host":"h.example","password":"`+secondPassword+`"}`)
	require.Equal(t, http.StatusCreated, created.status)
	id := obj(t, created)["id"].(string)
	add(created)
	add(d.apiGet(srvsPath))
	add(d.apiGet(srvPath(id)))
	add(d.api(http.MethodPatch, srvPath(id), `{"password":"`+secondPassword+`-2","host":"x.example.com"}`))
	add(d.api(http.MethodPost, srvPath(id)+"/test", ""))
	add(d.api(http.MethodPost, srvsPath+"/test", `{"name":"t","host":"h","port":99999,"password":"`+secondPassword+`"}`))
	add(d.api(http.MethodPost, srvsPath, `{"name":"main","host":"h","password":"`+secondPassword+`"}`))
	add(d.api(http.MethodPost, srvsPath, `{"name":"y","host":"","password":"`+secondPassword+`"}`))
	add(d.api(http.MethodPost, srvsPath, `{"name":"z","host":"h","password":"`+secondPassword+`","bogus":1}`))
	add(d.api(http.MethodPatch, srvPath(id), `{"password":"`+secondPassword+`","port":0}`))
	for _, b := range bodies {
		assert.NotContains(t, b, secondPassword)
		assert.NotContains(t, b, serverPassword)
	}

	res := d.stop()

	assert.NotContains(t, res.stderr, secondPassword)
	assert.NotContains(t, res.stderr, serverPassword)
	assert.NotContains(t, res.stdout, secondPassword)
}

func TestAPIServerPatchKeepsThePasswordUnlessOneIsGiven(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	path := srvPath(string(cfg.ServerID))

	r := d.api(http.MethodPatch, path, `{"name":"renamed","port":443,"enabled":false,"priority":2,"max_connections":3,"retention_days":100,"username":"other","host":" new.example "}`)

	requireOK(t, r)
	m := obj(t, r)
	requireServerShape(t, m)
	assert.Equal(t, "renamed", m["name"])
	assert.EqualValues(t, 443, m["port"])
	assert.Equal(t, false, m["enabled"])
	assert.EqualValues(t, 2, m["priority"])
	assert.EqualValues(t, 3, m["max_connections"])
	assert.EqualValues(t, 100, m["retention_days"])
	assert.Equal(t, "other", m["username"])
	assert.Equal(t, "new.example", m["host"])
	assert.Equal(t, true, m["has_password"])
	assert.Equal(t, string(cfg.ServerID), m["id"])
	assert.Equal(t, serverPassword, readServerPassword(t, cfg), "a PATCH without a password keeps it")

	repl := d.api(http.MethodPatch, path, `{"password":"`+secondPassword+`"}`)
	requireOK(t, repl)
	assert.Equal(t, true, obj(t, repl)["has_password"])
	assert.Equal(t, secondPassword, readServerPassword(t, cfg))

	cleared := d.api(http.MethodPatch, path, `{"password":""}`)
	requireOK(t, cleared)
	assert.Equal(t, false, obj(t, cleared)["has_password"], "an empty password removes it")
	assert.Equal(t, "", readServerPassword(t, cfg))

	again := d.api(http.MethodPatch, path, `{"password":null,"priority":5}`)
	requireOK(t, again)
	assert.EqualValues(t, 5, obj(t, again)["priority"])
}

// readServerPassword reads the stored password of the fixture Server with the
// Go store. A daemon may be running: SQLite in WAL mode lets a second process
// read.
func readServerPassword(t *testing.T, cfg configDir) string {
	t.Helper()
	key, err := store.LoadOrCreateKey(cfg.KeyFile, discard)
	require.NoError(t, err)
	db := openDB(t, cfg)
	defer func() { require.NoError(t, db.Close()) }()
	srv, err := store.NewServerRepo(db, key).Get(t.Context(), cfg.ServerID)
	require.NoError(t, err)
	require.False(t, srv.PasswordUnavailable, "the stored password must open with the key file")
	return srv.Password.Reveal()
}

func TestAPIServerPatchRejectsInvalidChanges(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	createSrv(t, d, `{"name":"b","host":"h"}`)
	path := srvPath(string(d.cfg.ServerID))

	for name, tc := range map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"empty":               {path, `{}`, 400, "empty_patch"},
		"only nulls":          {path, `{"name":null,"port":null,"password":null}`, 400, "empty_patch"},
		"bad host":            {path, `{"host":"a b"}`, 400, "invalid_host"},
		"empty host":          {path, `{"host":""}`, 400, "invalid_host"},
		"port zero":           {path, `{"port":0}`, 400, "invalid_port"},
		"port too high":       {path, `{"port":70000}`, 400, "invalid_port"},
		"connections zero":    {path, `{"max_connections":0}`, 400, "invalid_max_connections"},
		"negative retention":  {path, `{"retention_days":-1}`, 400, "invalid_retention_days"},
		"negative priority":   {path, `{"priority":-1}`, 400, "invalid_priority"},
		"blank name":          {path, `{"name":" "}`, 400, "invalid_name"},
		"bad username":        {path, `{"username":"a\nb"}`, 400, "invalid_credentials"},
		"bad password":        {path, `{"password":"a\u0000b"}`, 400, "invalid_credentials"},
		"taken name":          {path, `{"name":"b"}`, 409, "name_taken"},
		"wrong type":          {path, `{"port":"1"}`, 400, "invalid_json"},
		"id is not a field":   {path, `{"id":"x"}`, 400, "invalid_json"},
		"unknown id":          {srvPath("nope"), `{"name":"x"}`, 404, "not_found"},
		"unknown id empty":    {srvPath("nope"), `{}`, 400, "empty_patch"},
		"unknown id bad port": {srvPath("nope"), `{"port":0}`, 404, "not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			r := d.api(http.MethodPatch, tc.path, tc.body)

			requireError(t, r, tc.status, tc.code)
		})
	}
	got := d.apiGet(path)
	assert.Equal(t, serverName, obj(t, got)["name"], "no refused patch changed anything")
	assert.EqualValues(t, 563, obj(t, got)["port"])
}

func TestAPIServerPatchMayKeepItsOwnName(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.api(http.MethodPatch, srvPath(string(d.cfg.ServerID)), `{"name":"`+serverName+`"}`)

	requireOK(t, r)
}

func TestAPIServerDeleteRemovesItAndAnnouncesNothingElse(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	other := createSrv(t, d, `{"name":"other","host":"h"}`)

	r := d.api(http.MethodDelete, srvPath(string(cfg.ServerID)), "")
	again := d.api(http.MethodDelete, srvPath(string(cfg.ServerID)), "")

	requireNoContent(t, r)
	assert.Equal(t, http.StatusNotFound, again.status)
	assert.Equal(t, errBody("not_found", "not found"), again.body)
	assert.Equal(t, []string{"other"}, namesOfItems(itemsOf(t, d.apiGet(srvsPath))))
	assert.Equal(t, http.StatusNotFound, d.apiGet(srvPath(string(cfg.ServerID))).status)
	d.stop()
	db := openDB(t, cfg)
	defer func() { require.NoError(t, db.Close()) }()
	key, err := store.LoadOrCreateKey(cfg.KeyFile, discard)
	require.NoError(t, err)
	servers, err := store.NewServerRepo(db, key).List(t.Context())
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, core.ID(other["id"].(string)), servers[0].ID)
}

// useAnotherKey replaces the key file of c with a different valid key, and
// returns the bytes of the original, so the password the Go run sealed cannot
// be opened by the daemon any more.
func useAnotherKey(t *testing.T, c configDir) (original []byte) {
	t.Helper()
	original = readFile(t, c.KeyFile)
	otherKeyFile := t.TempDir() + "/other.key"
	_, err := store.LoadOrCreateKey(otherKeyFile, discard)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(c.KeyFile, readFile(t, otherKeyFile), 0o600))
	return original
}

func TestAPIServerWithAPasswordThatCannotBeOpenedIsShownAsUnavailableEverywhere(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	useAnotherKey(t, cfg)
	d := startDaemon(t, cfg)
	path := srvPath(string(cfg.ServerID))

	one := d.apiGet(path)
	requireOK(t, one)
	assert.Equal(t, true, obj(t, one)["has_password"])
	assert.Equal(t, true, obj(t, one)["password_unavailable"])
	list := itemsOf(t, d.apiGet(srvsPath))
	require.Len(t, list, 1)
	assert.Equal(t, true, list[0]["password_unavailable"])

	requireErrorMessage(t, d.api(http.MethodPost, path+"/test", ""), http.StatusConflict, "password_unavailable", passwordUnavMsg)
}

func TestAPIServerPatchWithoutAPasswordLeavesAnUnreadablePasswordUntouched(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	original := useAnotherKey(t, cfg)
	d := startDaemon(t, cfg)
	path := srvPath(string(cfg.ServerID))

	r := d.api(http.MethodPatch, path, `{"name":"renamed","priority":3}`)

	requireOK(t, r)
	m := obj(t, r)
	assert.Equal(t, "renamed", m["name"])
	assert.Equal(t, true, m["has_password"])
	assert.Equal(t, true, m["password_unavailable"], "still sealed with a key this run does not have")
	d.stop()
	require.NoError(t, os.WriteFile(cfg.KeyFile, original, 0o600))
	assert.Equal(t, serverPassword, readServerPassword(t, cfg), "the original key still opens the stored password")
}

// readServerPassword reads the password of the fixture Server with
// the key file of cfg.
func TestAPIServerPatchWithANewPasswordMakesAnUnreadableOneReadable(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	useAnotherKey(t, cfg)
	d := startDaemon(t, cfg)
	path := srvPath(string(cfg.ServerID))

	r := d.api(http.MethodPatch, path, `{"password":"`+secondPassword+`"}`)

	requireOK(t, r)
	assert.Equal(t, true, obj(t, r)["has_password"])
	assert.Equal(t, false, obj(t, r)["password_unavailable"])
	assert.Equal(t, false, obj(t, d.apiGet(path))["password_unavailable"])
	d.stop()
	assert.Equal(t, secondPassword, readServerPassword(t, cfg), "sealed with the key the daemon runs with")
}

func TestAPIServerTestOfAnUnknownServerIsNotFound(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.api(http.MethodPost, srvPath("nope")+"/test", "")

	assert.Equal(t, http.StatusNotFound, r.status)
	assert.Equal(t, errBody("not_found", "not found"), r.body)
}

func TestAPIServerTestOfAnUnsavedSpecValidatesItBeforeAnyConnection(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for name, tc := range map[string]struct{ body, code string }{
		"no host":       {`{"name":"x"}`, "invalid_host"},
		"url host":      {`{"host":"https://h.example"}`, "invalid_host"},
		"bad port":      {`{"host":"h.example","port":99999}`, "invalid_port"},
		"bad conns":     {`{"host":"h.example","max_connections":101}`, "invalid_max_connections"},
		"unknown field": {`{"host":"h.example","tls":false}`, "invalid_json"},
		"bad name":      {`{"name":"a\tb","host":"h.example"}`, "invalid_name"},
	} {
		t.Run(name, func(t *testing.T) {
			r := d.api(http.MethodPost, srvsPath+"/test", tc.body)

			requireError(t, r, http.StatusBadRequest, tc.code)
		})
	}
	assert.Len(t, itemsOf(t, d.apiGet(srvsPath)), 1, "testing saves nothing")
}
