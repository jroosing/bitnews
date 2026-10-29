package blackbox_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
)

func TestVersionWithoutAKeyIsUnauthorized(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	r := d.get("/api/v1/version", nil)

	assert.Equal(t, http.StatusUnauthorized, r.status)
	assert.Equal(t, errUnauthorized, r.body)
	assert.Equal(t, "Bearer", r.header.Get("WWW-Authenticate"))
	requireAPIHeaders(t, r)
}

func TestVersionWithAWrongKeyIsUnauthorizedWithTheSameAnswer(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	other := newAppKey(t)

	tests := []struct {
		name   string
		header map[string]string
		path   string
	}{
		{"another well-formed key as bearer", bearer(other), ""},
		{"another well-formed key as X-Api-Key", map[string]string{"X-Api-Key": other}, ""},
		{"empty bearer token", map[string]string{"Authorization": "Bearer "}, ""},
		{"garbage bearer token", bearer("not-a-key"), ""},
		{"the right key with a character appended", bearer(cfg.AppKey + "a"), ""},
		{"the right key with a character removed", bearer(cfg.AppKey[:len(cfg.AppKey)-1]), ""},
		{"the right key in upper case", bearer(strings.ToUpper(cfg.AppKey)), ""},
		{"the prefix alone", bearer(auth.KindAppKey.Prefix()), ""},
		{"a very long token", bearer(strings.Repeat("a", 8000)), ""},
		{"Basic scheme with the right key", map[string]string{"Authorization": "Basic " + cfg.AppKey}, ""},
		{"scheme only, no space", map[string]string{"Authorization": "Bearer" + cfg.AppKey}, ""},
		{"the right key only in the query string", nil, "/api/v1/version?apikey=" + cfg.AppKey},
		{"the right key in the query string next to a wrong bearer", bearer(other), "/api/v1/version?apikey=" + cfg.AppKey},
		{"a wrong bearer wins over the right X-Api-Key", map[string]string{"Authorization": "Bearer " + other, "X-Api-Key": cfg.AppKey}, ""},
		{"a wrong X-Api-Key and no bearer", map[string]string{"X-Api-Key": other}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path == "" {
				path = "/api/v1/version"
			}
			r := d.get(path, tt.header)
			assert.Equal(t, http.StatusUnauthorized, r.status)
			assert.Equal(t, errUnauthorized, r.body)
			assert.Equal(t, "Bearer", r.header.Get("WWW-Authenticate"))
		})
	}
}

func TestVersionAcceptsTheRightKeyInEveryForm(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	other := newAppKey(t)

	tests := []struct {
		name   string
		header map[string]string
	}{
		{"Authorization: Bearer", map[string]string{"Authorization": "Bearer " + cfg.AppKey}},
		{"the scheme is case-insensitive (lower)", map[string]string{"Authorization": "bearer " + cfg.AppKey}},
		{"the scheme is case-insensitive (upper)", map[string]string{"Authorization": "BEARER " + cfg.AppKey}},
		{"extra spaces after the scheme", map[string]string{"Authorization": "Bearer    " + cfg.AppKey}},
		{"X-Api-Key", map[string]string{"X-Api-Key": cfg.AppKey}},
		{"X-Api-Key with spaces around it", map[string]string{"X-Api-Key": "  " + cfg.AppKey + "  "}},
		{"both headers with the right key", map[string]string{"Authorization": "Bearer " + cfg.AppKey, "X-Api-Key": cfg.AppKey}},
		{"the right bearer wins over a wrong X-Api-Key", map[string]string{"Authorization": "Bearer " + cfg.AppKey, "X-Api-Key": other}},
		{"another scheme is skipped for the X-Api-Key", map[string]string{"Authorization": "Basic Zm9vOmJhcg==", "X-Api-Key": cfg.AppKey}},
		{"a header with no space is skipped for the X-Api-Key", map[string]string{"Authorization": "Bearer", "X-Api-Key": cfg.AppKey}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := d.get("/api/v1/version", tt.header)
			assert.Equal(t, http.StatusOK, r.status, r.body)
			assert.Empty(t, r.header.Get("WWW-Authenticate"))
			requireAPIHeaders(t, r)
		})
	}
}

func TestVersionAnswersTheCommonFields(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	r := d.get("/api/v1/version", bearer(cfg.AppKey))

	require.Equal(t, http.StatusOK, r.status)
	assert.True(t, strings.HasSuffix(r.body, "}\n"), "one JSON object followed by one newline: %q", r.body)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &got))
	// The Go daemon also reports go_version, the Zig one zig_version: only
	// the fields both have are compared.
	for _, field := range []string{"version", "commit"} {
		v, ok := got[field].(string)
		assert.True(t, ok, "%s must be a string: %v", field, got[field])
		assert.NotEmpty(t, v, field)
	}
	if binary.isGo {
		assert.Equal(t, builtVersion, got["version"])
		assert.Equal(t, builtCommit, got["commit"])
		assert.Contains(t, got, "go_version")
	}
}

func TestVersionAnswersHeadWithoutABody(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	r := d.request(http.MethodHead, "/api/v1/version", bearer(cfg.AppKey), "")

	assert.Equal(t, http.StatusOK, r.status)
	assert.Empty(t, r.body)
	assert.NotEmpty(t, r.header.Get("Content-Length"))
	assert.Equal(t, http.StatusUnauthorized, d.request(http.MethodHead, "/api/v1/version", nil, "").status)
}

func TestTheAppKeyStoredInTheDatabaseIsTheOneThatWorks(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	assert.Equal(t, http.StatusOK, d.get("/api/v1/version", bearer(cfg.AppKey)).status)
	assert.Equal(t, http.StatusUnauthorized, d.get("/api/v1/version", bearer(newAppKey(t))).status)
	d.stop()
	assert.Equal(t, cfg.AppKey, storedAppKey(t, cfg), "a well-formed stored key is kept as it is")
}

func TestChangingTheAppKeyInTheDatabaseBetweenTwoRunsChangesTheResult(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	first := startDaemon(t, cfg)
	require.Equal(t, http.StatusOK, first.get("/api/v1/version", bearer(cfg.AppKey)).status)
	first.stop()

	next := newAppKey(t)
	setAppKey(t, cfg, next)
	second := startDaemon(t, cfg)

	assert.Equal(t, http.StatusUnauthorized, second.get("/api/v1/version", bearer(cfg.AppKey)).status, "the old key no longer works")
	assert.Equal(t, http.StatusOK, second.get("/api/v1/version", bearer(next)).status, "the new key works")
	assert.Equal(t, http.StatusOK, second.get("/api/v1/version", map[string]string{"X-Api-Key": next}).status)
}

func TestAMalformedStoredAppKeyIsReplacedWithAWellFormedOne(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	setAppKey(t, cfg, "edited-by-hand")
	d := startDaemon(t, cfg)

	assert.Equal(t, http.StatusUnauthorized, d.get("/api/v1/version", bearer("edited-by-hand")).status)
	r := d.stop()
	assert.Contains(t, r.stderr, "the stored API key is malformed; making a new one")

	fresh := storedAppKey(t, cfg)
	kind, ok := auth.KindOf(fresh)
	assert.True(t, ok && kind == auth.KindAppKey, "the new key is shaped like an app key: %q", fresh)
	assert.NotContains(t, r.stderr, fresh, "the key is never logged")

	again := startDaemon(t, cfg)
	assert.Equal(t, http.StatusOK, again.get("/api/v1/version", bearer(fresh)).status)
}

func TestTheAppKeyIsNeverLogged(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg, withEnv("BITNEWS_LOG_LEVEL", "debug"))
	other := newAppKey(t)

	d.get("/api/v1/version", bearer(cfg.AppKey))
	d.get("/api/v1/version", map[string]string{"X-Api-Key": cfg.AppKey})
	d.get("/api/v1/version", bearer(other))
	d.get("/api/v1/version?apikey="+cfg.AppKey+"&token="+cfg.AppKey+"&key="+cfg.AppKey, nil)
	d.get("/api/v1/health?apikey="+cfg.AppKey, nil)
	out := d.stop().stderr

	assert.NotContains(t, out, cfg.AppKey)
	assert.NotContains(t, out, other)
	assert.Contains(t, out, "url=/api/v1/version", "the request itself is logged")
}
