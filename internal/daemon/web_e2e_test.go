package daemon_test

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webClient is a browser stand-in over real HTTP: it keeps cookies and does
// not follow redirects.
type webClient struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newWebClient(t *testing.T, base string) *webClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &webClient{t: t, base: base, c: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (w *webClient) do(method, path string, form url.Values) (*http.Response, string) {
	w.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, w.base+path, body)
	require.NoError(w.t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", w.base)
	}
	resp, err := w.c.Do(req)
	require.NoError(w.t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(w.t, err)
	_ = resp.Body.Close()
	return resp, string(b)
}

var csrfMeta = regexp.MustCompile(`<meta name="csrf-token" content="([0-9a-f]+)">`)

func (w *webClient) csrf() string {
	w.t.Helper()
	resp, body := w.do("GET", "/queue", nil)
	require.Equal(w.t, http.StatusOK, resp.StatusCode)
	m := csrfMeta.FindStringSubmatch(body)
	require.NotNil(w.t, m, "no csrf meta tag")
	return m[1]
}

// The whole first-run story over real HTTP: setup, sign out, sign in, a wrong
// password until the backoff says how long to wait.
func TestE2EWebFirstRunSetupLogoutLoginAndBackoff(t *testing.T) {
	e := newE2E(t, withoutAdmin())
	e.start()
	w := newWebClient(t, e.ts.URL)
	token := regexp.MustCompile(`setup_token=([A-Z0-9]+)`).FindStringSubmatch(e.logs.String())
	require.NotNil(t, token, "the setup token is logged")

	// First run: the front door leads to setup, everything else is gated.
	resp, _ := w.do("GET", "/", nil)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/setup", resp.Header.Get("Location"))
	resp, _ = w.do("GET", "/queue", nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp, body := w.do("GET", "/setup", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `id="setup-form"`)

	// Setup creates the admin and signs in.
	const password = "a long enough password"
	resp, body = w.do("POST", "/setup", url.Values{"token": {token[1]}, "username": {"owner"}, "password": {password}, "confirm": {password}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
	assert.Equal(t, "/queue", resp.Header.Get("Location"))
	resp, body = w.do("GET", "/queue", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "Queue")
	assert.Contains(t, body, "owner")
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "script-src 'self'")

	// Assets come hashed and cacheable; the API keeps its own looser CSP.
	css := regexp.MustCompile(`href="(/static/app\.[0-9a-f]{8}\.css)"`).FindStringSubmatch(body)
	require.NotNil(t, css)
	resp, _ = w.do("GET", css[1], nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Cache-Control"), "immutable")
	resp, _ = w.do("GET", "/api/v1/nope", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, "default-src 'self'; frame-ancestors 'none'", resp.Header.Get("Content-Security-Policy"))

	// Sign out: the session is gone.
	resp, _ = w.do("POST", "/logout", url.Values{"_csrf": {w.csrf()}})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))
	resp, _ = w.do("GET", "/queue", nil)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login?next=%2Fqueue", resp.Header.Get("Location"))

	// Sign in again.
	resp, body = w.do("POST", "/login", url.Values{"username": {"owner"}, "password": {password}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
	resp, _ = w.do("GET", "/queue", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp, _ = w.do("POST", "/logout", url.Values{"_csrf": {w.csrf()}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// Wrong passwords: a generic message, then the backoff.
	for range 5 {
		resp, body = w.do("POST", "/login", url.Values{"username": {"owner"}, "password": {"wrong wrong wrong"}})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
		assert.Contains(t, body, "Invalid username or password.")
	}
	resp, body = w.do("POST", "/login", url.Values{"username": {"owner"}, "password": {password}})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Regexp(t, `Try again in \d+ s\.`, body)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
}
