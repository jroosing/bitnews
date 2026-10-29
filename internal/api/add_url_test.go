package api_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

const urlSecret = "SUPERSECRETKEY123"

// indexer is a fake NZB source that records the requests it gets.
type indexer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newIndexer(t *testing.T, h http.HandlerFunc) *indexer {
	t.Helper()
	ix := &indexer{}
	ix.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ix.mu.Lock()
		ix.reqs = append(ix.reqs, r)
		ix.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(ix.Close)
	return ix
}

func (ix *indexer) hits() int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.reqs)
}

func serveNZB(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		_, _ = io.WriteString(w, doc)
	}
}

func (e *env) addURL(key, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do("POST", "/api/v1/downloads", body, withBearer(key))
}

func jsonURL(u string, extra string) string {
	return `{"url":"` + u + `"` + extra + `}`
}

func TestAddByURLFetchesAndStoresTheNZB(t *testing.T) {
	t.Parallel()
	doc := nzbXML(metaName)
	ix := newIndexer(t, serveNZB(doc))
	e, root := newAddEnv(t)
	key := e.apiKey()
	events := e.subscribe()

	rec := e.addURL(key, jsonURL(ix.URL+"/getnzb?apikey="+urlSecret, ""))

	got := e.addedDownload(rec)
	assert.Equal(t, "Meta.Release.Name", got.Name)
	assert.Equal(t, "queued", got.State)
	assert.EqualValues(t, 175, got.TotalBytes)
	stored, err := os.ReadFile(root + "/" + got.ID + "/Meta.Release.Name.nzb")
	require.NoError(t, err)
	assert.Equal(t, doc, string(stored))
	assert.Len(t, events(), 1)
}

func TestAddByURLHonorsTheJSONFields(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, serveNZB(nzbXML("")))
	e, _ := newAddEnv(t)
	key := e.apiKey()
	cat := core.Category{ID: core.NewID(), Name: "movies"}
	require.NoError(t, e.cats.Create(context.Background(), cat))

	rec := e.addURL(key, jsonURL(ix.URL, `,"name":"Some Movie","category_id":"`+string(cat.ID)+`","priority":"low","password":"pw1","paused":true`))

	got := e.addedDownload(rec)
	assert.Equal(t, "Some Movie", got.Name)
	assert.Equal(t, "low", got.Priority)
	assert.True(t, got.Paused)
	require.NotNil(t, got.CategoryID)
	assert.Equal(t, string(cat.ID), *got.CategoryID)
	_, pw, err := e.sources.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, "pw1", pw.Reveal())
}

func TestAddByURLNeverPersistsTheURL(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, serveNZB(nzbXML("")))
	e, root := newAddEnv(t)
	key := e.apiKey()

	got := e.addedDownload(e.addURL(key, jsonURL(ix.URL+"/getnzb?apikey="+urlSecret, "")))

	for _, table := range []string{"downloads", "download_sources"} {
		rows, err := e.db.Reader().Query(`SELECT * FROM ` + table)
		require.NoError(t, err)
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			require.NoError(t, rows.Scan(ptrs...))
			assert.NotContains(t, fmt.Sprint(vals...), urlSecret, table)
			assert.NotContains(t, fmt.Sprintf("%s", vals), ix.URL, table)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()
	}
	nzbBytes, err := os.ReadFile(root + "/" + got.ID + "/" + "show.part1.rar.nzb")
	require.NoError(t, err)
	assert.NotContains(t, string(nzbBytes), urlSecret)
}

func TestAddByURLSendsNoAmbientCredentialsAndABitnewsUserAgent(t *testing.T) {
	t.Parallel()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc"})
			http.Redirect(w, r, "/nzb", http.StatusFound)
			return
		}
		serveNZB(nzbXML(""))(w, r)
	})
	e, _ := newAddEnv(t, withAddOptions(app.WithHTTPClient(&http.Client{Jar: jar})))
	key := e.apiKey()
	body := jsonURL(ix.URL+"/start", "")

	e.addedDownload(e.addURL(key, body))

	ix.mu.Lock()
	defer ix.mu.Unlock()
	require.Len(t, ix.reqs, 2)
	for _, r := range ix.reqs {
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"), "no cookies, not even ones the indexer set")
		assert.Contains(t, r.Header.Get("User-Agent"), "bitnews")
		assert.Equal(t, http.MethodGet, r.Method)
	}
}

// redirectChain serves /hop/N by redirecting to /hop/N-1 down to /hop/0,
// which serves the NZB.
func redirectChain(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if err != nil || n == 0 {
			serveNZB(doc)(w, r)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d?apikey=%s", n-1, urlSecret), http.StatusFound)
	}
}

func TestAddByURLFollowsFiveRedirectsButNotSix(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, redirectChain(nzbXML("")))
	e, root := newAddEnv(t)
	key := e.apiKey()

	five := e.addURL(key, jsonURL(ix.URL+"/hop/5", ""))
	six := e.addURL(key, jsonURL(ix.URL+"/hop/6", ""))

	assert.Equal(t, http.StatusCreated, five.Code, five.Body.String())
	require.Equal(t, http.StatusBadGateway, six.Code, six.Body.String())
	assert.Equal(t, "fetch_failed", errorOf(t, six))
	assert.Contains(t, six.Body.String(), "redirects")
	assert.NotContains(t, six.Body.String(), urlSecret)
	assert.NotContains(t, e.log.String(), urlSecret)
	assert.Len(t, leftovers(t, root), 2, "only the successful add left files")
}

func TestAddByURLRefusesRedirectsToOtherSchemes(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"file:///etc/passwd", "ftp://example.com/x.nzb", "gopher://example.com/"} {
		ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusFound)
		})
		e, root := newAddEnv(t)
		key := e.apiKey()

		rec := e.addURL(key, jsonURL(ix.URL, ""))

		require.Equal(t, http.StatusBadGateway, rec.Code, target)
		assert.Equal(t, "fetch_failed", errorOf(t, rec))
		assert.Contains(t, rec.Body.String(), "only http and https")
		assert.Empty(t, leftovers(t, root))
	}
}

func TestAddByURLRejectsOtherSchemesAndBadURLsBeforeFetching(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.apiKey()
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/a.nzb", "javascript:alert(1)", "example.com/a.nzb", "http://", "//example.com/a"} {
		rec := e.addURL(key, jsonURL(u, ""))

		assert.Equal(t, http.StatusBadRequest, rec.Code, u)
		assert.Equal(t, "invalid_url", errorOf(t, rec), u)
	}
	empty := e.addURL(key, `{"name":"x"}`)
	assert.Equal(t, http.StatusBadRequest, empty.Code)
	assert.Equal(t, "missing_url", errorOf(t, empty))
	assert.Empty(t, leftovers(t, root))
}

func TestAddByURLChecksCategoryAndPriorityBeforeFetching(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, serveNZB(nzbXML("")))
	e, _ := newAddEnv(t)
	key := e.apiKey()

	cat := e.addURL(key, jsonURL(ix.URL, `,"category_id":"nope"`))
	prio := e.addURL(key, jsonURL(ix.URL, `,"priority":"urgent"`))
	unknown := e.addURL(key, jsonURL(ix.URL, `,"category":"tv"`))

	assert.Equal(t, 400, cat.Code)
	assert.Equal(t, "unknown_category", errorOf(t, cat))
	assert.Equal(t, "invalid_priority", errorOf(t, prio))
	assert.Equal(t, "invalid_json", errorOf(t, unknown))
	assert.Zero(t, ix.hits(), "nothing was fetched")
}

func TestAddByURLTimesOut(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	e, root := newAddEnv(t, withAddOptions(app.WithFetchTimeout(150*time.Millisecond)))
	key := e.apiKey()

	start := time.Now()
	rec := e.addURL(key, jsonURL(ix.URL+"/slow?apikey="+urlSecret, ""))

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Equal(t, "fetch_failed", errorOf(t, rec))
	assert.Contains(t, rec.Body.String(), "timed out")
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.NotContains(t, rec.Body.String(), urlSecret)
	assert.Empty(t, leftovers(t, root))
}

func TestAddByURLTimeoutAppliesToTheBodyToo(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<nzb>")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a body that never finishes
	})
	e, root := newAddEnv(t, withAddOptions(app.WithFetchTimeout(150*time.Millisecond)))
	key := e.apiKey()

	rec := e.addURL(key, jsonURL(ix.URL, ""))

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Equal(t, "fetch_failed", errorOf(t, rec))
	assert.Empty(t, leftovers(t, root))
}

func TestAddByURLCapsAStreamingResponse(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<nzb>")
		chunk := []byte(strings.Repeat(" ", 1024))
		for { // no Content-Length: chunked until the client gives up
			if _, err := w.Write(chunk); err != nil || r.Context().Err() != nil {
				return
			}
		}
	})
	e, root := newAddEnv(t, withAddOptions(app.WithMaxNZBBytes(8192)))
	key := e.apiKey()

	rec := e.addURL(key, jsonURL(ix.URL, ""))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Equal(t, "nzb_too_large", errorOf(t, rec))
	assert.Empty(t, leftovers(t, root))
}

func TestAddByURLRefusesAnAnnouncedOversizeResponseWithoutReadingIt(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<nzb>")
	})
	e, root := newAddEnv(t, withAddOptions(app.WithMaxNZBBytes(8192)))
	key := e.apiKey()

	rec := e.addURL(key, jsonURL(ix.URL, ""))

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "nzb_too_large", errorOf(t, rec))
	assert.Empty(t, leftovers(t, root))
}

func TestAddByURLReportsANon200AsFetchFailedWithoutTheURL(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusForbidden} {
		ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope "+r.URL.String(), status)
		})
		e, root := newAddEnv(t)
		key := e.apiKey()

		rec := e.addURL(key, jsonURL(ix.URL+"/getnzb?apikey="+urlSecret, ""))

		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		assert.Equal(t, "fetch_failed", errorOf(t, rec))
		assert.Contains(t, rec.Body.String(), strconv.Itoa(status))
		assert.NotContains(t, rec.Body.String(), urlSecret)
		assert.NotContains(t, rec.Body.String(), "nope", "the indexer's body is not echoed")
		assert.NotContains(t, e.log.String(), urlSecret)
		assert.Empty(t, leftovers(t, root))
	}
}

func TestAddByURLRedactsTheAPIKeyInEveryLog(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, serveNZB(nzbXML("")))
	closed := newIndexer(t, nil)
	closedURL := closed.URL
	closed.Close()
	e, _ := newAddEnv(t)
	key := e.apiKey()

	ok := e.addURL(key, jsonURL(ix.URL+"/getnzb?apikey="+urlSecret+"&t=get", ""))
	refused := e.addURL(key, jsonURL(closedURL+"/getnzb?apikey="+urlSecret, ""))
	// A client that puts a key in the query of the API request itself.
	viaQuery := e.do("POST", "/api/v1/downloads?apikey="+urlSecret, jsonURL(ix.URL, ""), withBearer(key))

	assert.Equal(t, http.StatusCreated, ok.Code)
	require.Equal(t, http.StatusBadGateway, refused.Code, refused.Body.String())
	assert.Equal(t, "fetch_failed", errorOf(t, refused))
	assert.NotContains(t, refused.Body.String(), urlSecret)
	_ = viaQuery
	logs := e.log.String()
	assert.NotContains(t, logs, urlSecret)
	assert.Contains(t, logs, "nzb fetched")
	assert.Contains(t, logs, "fetch failed")
	assert.Contains(t, logs, "apikey=")
}

func TestAddByURLOfANonNZBBodyIs400AndLeavesNothing(t *testing.T) {
	t.Parallel()
	ix := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html><body>Please log in</body></html>")
	})
	e, root := newAddEnv(t)
	key := e.apiKey()

	rec := e.addURL(key, jsonURL(ix.URL, ""))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid_nzb", errorOf(t, rec))
	assert.Empty(t, leftovers(t, root))
}

func TestAddByURLAllowsLoopbackAndPrivateTargets(t *testing.T) {
	// httptest listens on 127.0.0.1: the LAN-tool decision is that private
	// addresses are fine, so a loopback indexer works (see the happy path).
	t.Parallel()
	ix := newIndexer(t, serveNZB(nzbXML("")))
	assert.True(t, strings.HasPrefix(ix.URL, "http://127.0.0.1:"))
	e, _ := newAddEnv(t)

	rec := e.addURL(e.apiKey(), jsonURL(ix.URL, ""))

	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestAddByURLJSONBodyIsStrictAndSmall(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.apiKey()

	extra := e.addURL(key, `{"url":"http://x","tls":false}`)
	huge := e.addURL(key, `{"url":"http://x/`+strings.Repeat("a", 20<<10)+`"}`)

	assert.Equal(t, "invalid_json", errorOf(t, extra))
	assert.Equal(t, http.StatusRequestEntityTooLarge, huge.Code)
}
