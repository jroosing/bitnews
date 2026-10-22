package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthIsOKWithoutAuthentication(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("GET", "/api/v1/health", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

func TestEveryResponseCarriesSecurityHeaders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, path := range []string{"/api/v1/health", "/api/v1/downloads", "/nope"} {
		rec := e.do("GET", path, "")

		assert.Equal(t, "default-src 'self'; frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"), path)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"), path)
	}
}

func TestSecurityHeadersAlsoOnTheSetupGateResponse(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutAdmin())

	rec := e.do("GET", "/api/v1/downloads", "")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "setup_required", errorOf(t, rec))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
}

func TestRequestIDIsGeneratedWhenAbsent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	a := e.do("GET", "/api/v1/health", "").Header().Get("X-Request-ID")
	b := e.do("GET", "/api/v1/health", "").Header().Get("X-Request-ID")

	assert.Regexp(t, `^[0-9a-f]{32}$`, a)
	assert.NotEqual(t, a, b)
}

func TestRequestIDFromTheClientIsEchoedWhenItIsSafe(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("GET", "/api/v1/health", "", withHeader("X-Request-ID", "trace-123.A_b"))

	assert.Equal(t, "trace-123.A_b", rec.Header().Get("X-Request-ID"))
}

func TestRequestIDFromTheClientIsReplacedWhenTooLongOrOddlyEncoded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for name, id := range map[string]string{
		"too long": strings.Repeat("a", 65),
		"space":    "a b",
		"newline":  "a\\nb",
		"quote":    `a"b`,
		"unicode":  "é",
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("GET", "/api/v1/health", "", withHeader("X-Request-ID", id))

			assert.Regexp(t, `^[0-9a-f]{32}$`, rec.Header().Get("X-Request-ID"))
		})
	}
}

func TestUnknownPathIsAJSONNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("GET", "/api/v1/nope", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorOf(t, rec))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

func TestWrongMethodIsAJSONMethodNotAllowedWithAllowHeader(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("POST", "/api/v1/health", "")

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "method_not_allowed", errorOf(t, rec))
	assert.Contains(t, rec.Header().Get("Allow"), "GET")
}

func TestPanicInAHandlerBecomesAGenericJSON500AndIsLoggedWithItsStack(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.server.HandleForTest("GET /boom", func(http.ResponseWriter, *http.Request) { panic("secret detail") })

	rec := e.do("GET", "/boom", "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "internal", errorOf(t, rec))
	assert.NotContains(t, rec.Body.String(), "secret detail")
	assert.Contains(t, e.log.String(), "secret detail")
	assert.Contains(t, e.log.String(), "goroutine")
	assert.Contains(t, e.log.String(), rec.Header().Get("X-Request-ID"))
}

func TestAccessLogRecordsRequestWithRedactedURLAndRequestID(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("GET", "/api/v1/health?apikey=hunter2&x=1", "", withHeader("X-Request-ID", "req-1"))

	require.Equal(t, http.StatusOK, rec.Code)
	logs := e.log.String()
	assert.Contains(t, logs, "request_id=req-1")
	assert.Contains(t, logs, "status=200")
	assert.Contains(t, logs, "method=GET")
	assert.Contains(t, logs, "apikey=REDACTED")
	assert.NotContains(t, logs, "hunter2")
}

func TestAccessLogRecordsThePanickedRequestAs500(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.server.HandleForTest("GET /boom", func(http.ResponseWriter, *http.Request) { panic("x") })

	e.do("GET", "/boom", "")

	assert.Contains(t, e.log.String(), "status=500")
}
