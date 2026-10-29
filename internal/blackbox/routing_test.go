package blackbox_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUnknownAPIPathIsAJSON404EvenWithoutAKey(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	// A path that matches no route is a 404 before authentication looks at it:
	// it never gives away which paths exist behind a key, and never a 401.
	for _, path := range []string{
		"/api/v1/nope",
		"/api/v1/",
		"/api/v1",
		"/api",
		"/api/",
		"/api/v3/health",
		"/api/v1/health/",
		"/api/v1/version/",
		"/api/v1/downloads/",
		"/sabnzbd",
	} {
		t.Run(path, func(t *testing.T) {
			r := d.get(path, nil)
			assert.Equal(t, http.StatusNotFound, r.status)
			assert.Equal(t, errNotFound, r.body)
			requireAPIHeaders(t, r)
		})
	}
}

func TestUnknownAPIPathWithAKeyIsTheSame404(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	r := d.get("/api/v1/nope", bearer(cfg.AppKey))

	assert.Equal(t, http.StatusNotFound, r.status)
	assert.Equal(t, errNotFound, r.body)
}

func TestWrongMethodIsAJSON405WithAnAllowHeader(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	// A GET route also answers HEAD, so Allow lists both. Like the 404, the
	// 405 comes before authentication, so no key is needed to see it.
	for _, path := range []string{"/api/v1/health", "/api/v1/version"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
			t.Run(method+" "+path, func(t *testing.T) {
				r := d.request(method, path, nil, "")
				assert.Equal(t, http.StatusMethodNotAllowed, r.status)
				assert.Equal(t, errMethod, r.body)
				assert.Equal(t, "GET, HEAD", r.header.Get("Allow"))
				requireAPIHeaders(t, r)
			})
		}
	}
}

func TestANonCanonicalPathIsRedirectedToTheCleanOne(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, prepareConfigDir(t))

	for _, path := range []string{"/api/v1//health", "/api/v1/./health", "/api/v1/x/../health"} {
		t.Run(path, func(t *testing.T) {
			r := d.raw("GET " + path + " HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
			status, _, _ := strings.Cut(r, "\r\n")
			assert.Equal(t, "HTTP/1.1 307 Temporary Redirect", status)
			assert.Contains(t, r, "\r\nLocation: /api/v1/health\r\n")
		})
	}
}

func TestARequestBodyAboveTheRouteLimitIsRefusedWith413(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	json := func(key string) map[string]string {
		h := map[string]string{"Content-Type": "application/json"}
		if key != "" {
			h["Authorization"] = "Bearer " + key
		}
		return h
	}

	t.Run("a small route stops at 16 KiB", func(t *testing.T) {
		body := `{"name":"` + strings.Repeat("a", 17<<10) + `"}`
		r := d.request(http.MethodPost, "/api/v1/categories", json(cfg.AppKey), body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, r.status)
		assert.Equal(t, errTooLarge, r.body)
		requireAPIHeaders(t, r)
	})
	t.Run("authentication comes before the body is read", func(t *testing.T) {
		body := `{"name":"` + strings.Repeat("a", 17<<10) + `"}`
		r := d.request(http.MethodPost, "/api/v1/categories", json(""), body)
		assert.Equal(t, http.StatusUnauthorized, r.status)
		assert.Equal(t, errUnauthorized, r.body)
	})
	t.Run("settings stop at 1 MiB", func(t *testing.T) {
		body := `{"x":"` + strings.Repeat("a", 1<<20) + `"}`
		r := d.request(http.MethodPut, "/api/v1/settings", json(cfg.AppKey), body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, r.status)
		assert.Equal(t, errTooLarge, r.body)
	})
}
