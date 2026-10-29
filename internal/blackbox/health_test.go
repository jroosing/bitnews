package blackbox_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiCSP is the Content-Security-Policy of every API answer.
const apiCSP = "default-src 'self'; frame-ancestors 'none'"

// requireAPIHeaders asserts the headers every JSON answer of the API carries:
// the security headers of the outer middleware, the JSON type, no caching
// and a request ID.
func requireAPIHeaders(t *testing.T, r response) {
	t.Helper()
	assert.Equal(t, "application/json", r.header.Get("Content-Type"))
	assert.Equal(t, "no-store", r.header.Get("Cache-Control"))
	requireSecurityHeaders(t, r)
}

// requireSecurityHeaders asserts the headers the API middleware sets on
// every answer, successful or not.
func requireSecurityHeaders(t *testing.T, r response) {
	t.Helper()
	assert.Equal(t, apiCSP, r.header.Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", r.header.Get("X-Content-Type-Options"))
	assert.Equal(t, "same-origin", r.header.Get("Referrer-Policy"))
	assert.NotEmpty(t, r.header.Get("X-Request-Id"))
}

func TestHealthAnswersTheExactBodyAndHeaders(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	r := d.get("/api/v1/health", nil)

	assert.Equal(t, http.StatusOK, r.status)
	assert.Equal(t, healthBody, r.body, "compact JSON followed by one newline")
	assert.Equal(t, "16", r.header.Get("Content-Length"))
	requireAPIHeaders(t, r)
	assert.Empty(t, r.header.Get("WWW-Authenticate"), "health needs no key")
}

func TestHealthAnswersHeadWithTheHeadersAndNoBody(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	r := d.request(http.MethodHead, "/api/v1/health", nil, "")

	assert.Equal(t, http.StatusOK, r.status)
	assert.Empty(t, r.body)
	assert.Equal(t, "16", r.header.Get("Content-Length"))
	requireAPIHeaders(t, r)
}

func TestHealthDoesNotReadARequestBodyAboveTheRouteLimit(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	// The route's limit is 16 KiB, but a handler that never reads the body
	// never trips it.
	r := d.request(http.MethodGet, "/api/v1/health", nil, strings.Repeat("x", 30_000))

	assert.Equal(t, http.StatusOK, r.status)
	assert.Equal(t, healthBody, r.body)
}

func TestRequestIDIsEchoedWhenValidAndReplacedWhenNot(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))
	generated := regexp.MustCompile(`^[0-9a-f]{32}$`)

	tests := []struct {
		name   string
		header string
		// want is the expected ID; empty means a generated one.
		want string
	}{
		{"none sent", "", ""},
		{"plain", "abc-123", "abc-123"},
		{"all allowed characters", "Az09._-", "Az09._-"},
		{"64 characters", strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{"65 characters", strings.Repeat("a", 65), ""},
		{"space inside", "has space", ""},
		{"slash", "a/b", ""},
		{"non ASCII", "café", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tt.header != "" {
				hdr["X-Request-ID"] = tt.header
			}
			r := d.get("/api/v1/health", hdr)
			require.Equal(t, http.StatusOK, r.status)
			got := r.header.Get("X-Request-Id")
			if tt.want != "" {
				assert.Equal(t, tt.want, got)
				return
			}
			assert.Regexp(t, generated, got, "a generated ID is 32 lowercase hex digits")
			assert.NotEqual(t, tt.header, got)
		})
	}
}

func TestSecurityHeadersAreOnEveryAnswerOfTheAPI(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	for name, r := range map[string]response{
		"health":             d.get("/api/v1/health", nil),
		"unauthorized":       d.get("/api/v1/version", nil),
		"version":            d.get("/api/v1/version", bearer(cfg.AppKey)),
		"not found":          d.get("/api/v1/nope", nil),
		"method not allowed": d.request(http.MethodPost, "/api/v1/health", nil, ""),
	} {
		t.Run(name, func(t *testing.T) { requireAPIHeaders(t, r) })
	}
}
