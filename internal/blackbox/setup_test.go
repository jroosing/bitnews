package blackbox_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// While no admin exists the instance is in first-run setup mode: the gate
// answers 503 setup_required for everything except the health route, the
// static assets and the setup pages of the web UI, even for a valid key.

func TestWhileSetupIsOpenOnlyHealthIsServed(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t, withoutAdmin())
	d := startDaemon(t, cfg)

	assert.Equal(t, healthBody, d.get("/api/v1/health", nil).body)

	// The gate lets only GET through on the health route: HEAD is gated too.
	head := d.request(http.MethodHead, "/api/v1/health", nil, "")
	assert.Equal(t, http.StatusServiceUnavailable, head.status)
	assert.Empty(t, head.body)

	for name, r := range map[string]response{
		"version without a key": d.get("/api/v1/version", nil),
		"version with the key":  d.get("/api/v1/version", bearer(cfg.AppKey)),
		"unknown API path":      d.get("/api/v1/nope", nil),
		"wrong method":          d.request(http.MethodPost, "/api/v1/health", nil, ""),
		"servers":               d.get("/api/v1/servers", bearer(cfg.AppKey)),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, http.StatusServiceUnavailable, r.status)
			assert.Equal(t, errSetup, r.body)
			requireAPIHeaders(t, r)
		})
	}
}

func TestWhileSetupIsOpenTheSetupTokenIsLoggedOnceAtWarnLevel(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t, withoutAdmin()), withEnv("BITNEWS_LOG_FORMAT", "json"))

	lines := jsonLines(t, d.stop().stderr)

	var found []map[string]any
	for _, l := range lines {
		if _, ok := l["setup_token"]; ok {
			found = append(found, l)
		}
	}
	require.Len(t, found, 1)
	assert.Equal(t, "WARN", found[0]["level"])
	assert.Contains(t, found[0]["msg"], "first-run setup required: no admin exists yet.")
	assert.Regexp(t, regexp.MustCompile(`^[A-Z2-7]{52}$`), found[0]["setup_token"], "32 random bytes in upper-case base32 without padding")
}

func TestOnceAnAdminExistsSetupIsClosedAndNoTokenIsLogged(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	assert.Equal(t, http.StatusOK, d.get("/api/v1/version", bearer(cfg.AppKey)).status)
	out := d.stop().stderr

	assert.NotContains(t, out, "setup_token")
	assert.NotContains(t, out, "first-run setup required")
}

// The web UI is judged by its own part of the suite (M15-07). These two
// checks only pin what the skeleton's routing decides.

func TestWebRootRedirectsToSetupWhileSetupIsOpen(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t, withoutAdmin()))

	r := d.get("/", nil)

	assert.Equal(t, http.StatusSeeOther, r.status)
	assert.Equal(t, "/setup", r.header.Get("Location"))
}

func TestWebUnknownPathIsAnHTMLPageNotTheAPIError(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	r := d.get("/no/such/page", nil)

	assert.Equal(t, http.StatusNotFound, r.status)
	assert.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"))
	assert.NotEqual(t, errNotFound, r.body, "only /api and /sabnzbd paths get the JSON error")
}

// jsonLines parses stderr of a daemon run with BITNEWS_LOG_FORMAT=json: one
// JSON object per non-empty line.
func jsonLines(t *testing.T, stderr string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range splitLines(stderr) {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "not a JSON log line: %q", line)
		out = append(out, m)
	}
	return out
}
