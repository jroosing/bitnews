package blackbox_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiRoute is one route of the own API.
type apiRoute struct {
	method, path string
	// json is true when the route reads a JSON body.
	json bool
	// body is a valid body for it, when it has one.
	body string
}

// The routes of the Z2 scope. {id} stands for an ID that does not exist.
var apiRoutes = []apiRoute{
	{http.MethodGet, "/api/v1/downloads", false, ""},
	{http.MethodGet, "/api/v1/downloads/nope", false, ""},
	{http.MethodPost, "/api/v1/downloads/nope/pause", false, ""},
	{http.MethodPost, "/api/v1/downloads/nope/resume", false, ""},
	{http.MethodPatch, "/api/v1/downloads/nope", true, `{"priority":"low"}`},
	{http.MethodDelete, "/api/v1/downloads/nope", false, ""},
	{http.MethodGet, "/api/v1/categories", false, ""},
	{http.MethodPost, "/api/v1/categories", true, `{"name":"x"}`},
	{http.MethodPatch, "/api/v1/categories/nope", true, `{"name":"x"}`},
	{http.MethodDelete, "/api/v1/categories/nope", false, ""},
	{http.MethodGet, "/api/v1/servers", false, ""},
	{http.MethodPost, "/api/v1/servers", true, `{"name":"x","host":"h"}`},
	{http.MethodPost, "/api/v1/servers/test", true, `{"host":"h"}`},
	{http.MethodGet, "/api/v1/servers/nope", false, ""},
	{http.MethodPatch, "/api/v1/servers/nope", true, `{"name":"x"}`},
	{http.MethodDelete, "/api/v1/servers/nope", false, ""},
	{http.MethodPost, "/api/v1/servers/nope/test", false, ""},
	{http.MethodGet, "/api/v1/settings", false, ""},
	{http.MethodPut, "/api/v1/settings", true, `{}`},
	{http.MethodGet, "/api/v1/events", false, ""},
}

func TestAPIEveryRouteNeedsTheAppKeyBeforeItLooksAtTheRequest(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for _, rt := range apiRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			none := d.request(rt.method, rt.path, map[string]string{"Content-Type": "application/json"}, rt.body)
			wrong := d.request(rt.method, rt.path, map[string]string{"Authorization": "Bearer " + newAppKey(t), "Content-Type": "text/plain"}, rt.body)

			for _, r := range []response{none, wrong} {
				assert.Equal(t, http.StatusUnauthorized, r.status, r.body)
				assert.Equal(t, errUnauthorized, r.body)
				assert.Equal(t, "Bearer", r.header.Get("WWW-Authenticate"))
				requireAPIHeaders(t, r)
			}
		})
	}
}

func TestAPIRoutesAnswer405WithTheAllowedMethodsBeforeAuthentication(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodPut, "/api/v1/downloads", "GET, HEAD, POST"},
		{http.MethodDelete, "/api/v1/downloads", "GET, HEAD, POST"},
		{http.MethodPost, "/api/v1/downloads/x", "DELETE, GET, HEAD, PATCH"},
		{http.MethodPut, "/api/v1/downloads/x", "DELETE, GET, HEAD, PATCH"},
		{http.MethodGet, "/api/v1/downloads/x/pause", "POST"},
		{http.MethodDelete, "/api/v1/downloads/x/resume", "POST"},
		{http.MethodPut, "/api/v1/categories", "GET, HEAD, POST"},
		{http.MethodGet, "/api/v1/categories/x", "DELETE, PATCH"},
		{http.MethodPut, "/api/v1/categories/x", "DELETE, PATCH"},
		{http.MethodPut, "/api/v1/servers", "GET, HEAD, POST"},
		{http.MethodPut, "/api/v1/servers/x", "DELETE, GET, HEAD, PATCH"},
		{http.MethodGet, "/api/v1/servers/x/test", "POST"},
		{http.MethodPost, "/api/v1/settings", "GET, HEAD, PUT"},
		{http.MethodPost, "/api/v1/events", "GET, HEAD"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := d.request(tc.method, tc.path, nil, "")

			assert.Equal(t, http.StatusMethodNotAllowed, r.status)
			assert.Equal(t, errMethod, r.body)
			assert.Equal(t, tc.allow, r.header.Get("Allow"))
			requireAPIHeaders(t, r)
		})
	}
}

func TestAPIJSONRoutesRefuseAnyOtherContentTypeWith415(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	const msg = "send the body as Content-Type: application/json"

	for _, rt := range apiRoutes {
		if !rt.json {
			continue
		}
		for name, ct := range map[string]string{
			"none":        "",
			"text":        "text/plain",
			"form":        "application/x-www-form-urlencoded",
			"multipart":   "multipart/form-data; boundary=x",
			"json suffix": "application/jsonx",
			"vendor json": "application/vnd.api+json",
			"wrong":       "json",
		} {
			t.Run(rt.method+" "+rt.path+" "+name, func(t *testing.T) {
				h := map[string]string{"Authorization": "Bearer " + d.cfg.AppKey}
				if ct != "" {
					h["Content-Type"] = ct
				}
				r := d.request(rt.method, rt.path, h, rt.body)

				requireErrorMessage(t, r, http.StatusUnsupportedMediaType, "unsupported_media_type", msg)
			})
		}
	}
}

func TestAPIContentTypeIsCheckedBeforeTheBodyAndBeforeTheLookup(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	h := map[string]string{"Authorization": "Bearer " + d.cfg.AppKey, "Content-Type": "text/plain"}

	broken := d.request(http.MethodPatch, "/api/v1/downloads/nope", h, `{`)
	empty := d.request(http.MethodPost, catsPath, h, "")

	requireError(t, broken, http.StatusUnsupportedMediaType, "unsupported_media_type")
	requireError(t, empty, http.StatusUnsupportedMediaType, "unsupported_media_type")
}

func TestAPIJSONRoutesAcceptParametersAndAnyCaseInTheContentType(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	n := 0

	for _, ct := range []string{"application/json; charset=utf-8", "APPLICATION/JSON", "Application/Json;charset=UTF-8", "application/json ; x=y"} {
		n++
		h := map[string]string{"Authorization": "Bearer " + d.cfg.AppKey, "Content-Type": ct}
		r := d.request(http.MethodPost, catsPath, h, `{"name":"ct`+strings.Repeat("a", n)+`"}`)

		assert.Equal(t, http.StatusCreated, r.status, "%q: %s", ct, r.body)
	}
}

func TestAPIRoutesWithoutABodyIgnoreItsContentTypeAndContent(t *testing.T) {
	t.Parallel()
	dl := newDownload("movie")
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, dl) })
	h := map[string]string{"Authorization": "Bearer " + d.cfg.AppKey, "Content-Type": "text/plain"}

	pause := d.request(http.MethodPost, dlPath(dl)+"/pause", h, "anything at all")
	get := d.request(http.MethodGet, "/api/v1/downloads", h, strings.Repeat("x", 20<<10))
	del := d.request(http.MethodDelete, dlPath(dl), h, "x")

	requireOK(t, pause)
	assert.Equal(t, true, obj(t, pause)["paused"])
	requireOK(t, get)
	requireNoContent(t, del)
}

func TestAPIJSONRoutesRefuseMalformedBodiesWithInvalidJSON(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for _, rt := range apiRoutes {
		if !rt.json {
			continue
		}
		for name, body := range map[string]string{
			"empty":     "",
			"spaces":    "   \n",
			"truncated": `{"x":`,
			"garbage":   `not json`,
			"trailing":  `{} {}`,
			"comma":     `{},`,
			"single":    `{'a':1}`,
			"nan":       `{"a":NaN}`,
		} {
			t.Run(rt.method+" "+rt.path+" "+name, func(t *testing.T) {
				r := d.request(rt.method, rt.path, d.apiHeaders(), body)

				requireError(t, r, http.StatusBadRequest, "invalid_json")
			})
		}
	}
}

func TestAPIInvalidJSONIsCheckedBeforeTheIDIsLookedUp(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for _, p := range []string{"/api/v1/downloads/nope", "/api/v1/categories/nope", "/api/v1/servers/nope"} {
		r := d.request(http.MethodPatch, p, d.apiHeaders(), `{`)

		requireError(t, r, http.StatusBadRequest, "invalid_json")
	}
}

func TestAPIAnEmptyBodyIsAnInvalidJSONErrorWithTheApiMessage(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.request(http.MethodPost, srvsPath, d.apiHeaders(), "")

	requireErrorMessage(t, r, http.StatusBadRequest, "invalid_json", "the request body is empty")
}

func TestAPIJSONBodiesDecodeEscapesAndTheLastDuplicateKeyWins(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	escaped := createCat(t, d, `{"name":"tv2"}`)
	dup := createCat(t, d, `{"name":"first","name":"second"}`)
	srv := createSrv(t, d, `{"name":"Provïder 😀 \"q\" \\ \/","host":"h"}`)

	assert.Equal(t, "tv2", escaped["name"])
	assert.Equal(t, "second", dup["name"])
	assert.Equal(t, "Provïder 😀 \"q\" \\ /", srv["name"], "escapes are decoded; answers carry UTF-8")
}

func TestAPIAnswersEscapeHTMLCharactersAsGoDoes(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.api(http.MethodPost, srvsPath, `{"name":"a&b<c>d\u2028e","host":"h"}`)

	require.Equal(t, http.StatusCreated, r.status, r.body)
	// Go's encoder writes & < > and the line separators U+2028 and U+2029 as
	// \u escapes.
	assert.Contains(t, r.body, `"name":"a\u0026b\u003cc\u003ed\u2028e"`)
	var m map[string]any
	require.NoError(t, jsonUnmarshal(r.body, &m))
	assert.Equal(t, "a&b<c>d\u2028e", m["name"])
}

func TestAPIRoutesStopReadingAtTheirBodyLimit(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	pad := func(prefix, suffix string, total int) string {
		return prefix + strings.Repeat("a", total-len(prefix)-len(suffix)) + suffix
	}

	t.Run("categories accept exactly 16 KiB", func(t *testing.T) {
		body := pad(`{"name":"big","usenet_dir":"/`, `"}`, 16<<10)
		require.Len(t, body, 16<<10)
		r := d.api(http.MethodPost, catsPath, body)
		assert.Equal(t, http.StatusCreated, r.status, "%.200s", r.body)
	})
	t.Run("categories refuse one byte more", func(t *testing.T) {
		body := pad(`{"name":"bigger","usenet_dir":"/`, `"}`, 16<<10+1)
		r := d.api(http.MethodPost, catsPath, body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, r.status)
		assert.Equal(t, errTooLarge, r.body)
		requireAPIHeaders(t, r)
		assert.NotContains(t, namesOfItems(itemsOf(t, d.apiGet(catsPath))), "bigger")
	})
	t.Run("category patch", func(t *testing.T) {
		r := d.api(http.MethodPatch, catPath(string(d.cfg.CategoryID)), pad(`{"usenet_dir":"/`, `"}`, 17<<10))
		assert.Equal(t, errTooLarge, r.body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, r.status)
	})
	t.Run("servers accept exactly 16 KiB", func(t *testing.T) {
		body := pad(`{"name":"x","host":"h","username":"`, `"}`, 16<<10)
		r := d.api(http.MethodPost, srvsPath, body)
		requireError(t, r, http.StatusBadRequest, "invalid_credentials")
	})
	t.Run("servers refuse one byte more", func(t *testing.T) {
		r := d.api(http.MethodPost, srvsPath, pad(`{"name":"x","host":"h","username":"`, `"}`, 16<<10+1))
		assert.Equal(t, errTooLarge, r.body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, r.status)
	})
	t.Run("server test", func(t *testing.T) {
		r := d.api(http.MethodPost, srvsPath+"/test", pad(`{"host":"h","username":"`, `"}`, 17<<10))
		assert.Equal(t, errTooLarge, r.body)
	})
	t.Run("download patch", func(t *testing.T) {
		r := d.api(http.MethodPatch, "/api/v1/downloads/nope", pad(`{"category_id":"`, `"}`, 17<<10))
		assert.Equal(t, errTooLarge, r.body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, r.status)
	})
	t.Run("settings accept exactly 1 MiB", func(t *testing.T) {
		body := pad(`{"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/`, `"}}}`, 1<<20)
		require.Len(t, body, 1<<20)
		r := d.api(http.MethodPut, settingsPath, body)
		assert.Equal(t, http.StatusOK, r.status, "%.200s", r.body)
	})
	t.Run("settings refuse one byte more", func(t *testing.T) {
		body := pad(`{"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/`, `"}}}`, 1<<20+1)
		r := d.api(http.MethodPut, settingsPath, body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, r.status)
		assert.Equal(t, errTooLarge, r.body)
	})
	t.Run("the daemon is fine afterwards", func(t *testing.T) {
		requireOK(t, d.apiGet(catsPath))
	})
}

func TestAPIAddWithoutASourceExplainsWhatIsNeeded(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	const url = "/api/v1/downloads"

	post := func(ct, body string) response {
		h := map[string]string{"Authorization": "Bearer " + d.cfg.AppKey}
		if ct != "" {
			h["Content-Type"] = ct
		}
		return d.request(http.MethodPost, url, h, body)
	}
	const unsupported = "send multipart/form-data with an nzb or torrent part, or application/json with a url or magnet"
	requireErrorMessage(t, post("", `{}`), 415, "unsupported_media_type", unsupported)
	requireErrorMessage(t, post("text/plain", `{"url":"http://x"}`), 415, "unsupported_media_type", unsupported)
	requireErrorMessage(t, post("application/x-www-form-urlencoded", `url=x`), 415, "unsupported_media_type", unsupported)
	requireErrorMessage(t, post("application/json", `{}`), 400, "missing_url", "give the url of an NZB or a torrent, or a magnet link")
	requireErrorMessage(t, post("application/json", `{"url":"","magnet":""}`), 400, "missing_url", "give the url of an NZB or a torrent, or a magnet link")
	requireErrorMessage(t, post("application/json", `{"name":"x","paused":true}`), 400, "missing_url", "give the url of an NZB or a torrent, or a magnet link")
	requireErrorMessage(t, post("application/json", `{"url":"http://x.example/a.nzb","magnet":"magnet:?xt=urn:btih:abc"}`), 400, "ambiguous_source",
		"give either a url or a magnet, not both")
	requireErrorMessage(t, post("application/json", `{"url":"magnet:?xt=urn:btih:abc"}`), 400, "invalid_url",
		"url must be an absolute http or https URL; send a magnet link as magnet")
	requireErrorMessage(t, post("application/json", `{"url":"  MAGNET:?xt=urn:btih:abc"}`), 400, "invalid_url",
		"url must be an absolute http or https URL; send a magnet link as magnet")
	requireErrorMessage(t, post("application/json", `{"magnet":"http://x.example/a.torrent"}`), 400, "invalid_magnet", "not a usable magnet link")
	requireErrorMessage(t, post("application/json", ""), 400, "invalid_json", "the request body is empty")
	requireError(t, post("application/json", `{"link":"http://x"}`), 400, "invalid_json")
	requireError(t, post("application/json", `{"url":5}`), 400, "invalid_json")
	requireError(t, post("application/json", `{"url":"http://x"} {}`), 400, "invalid_json")
	big := post("application/json", `{"url":"http://x.example/`+strings.Repeat("a", 17<<10)+`"}`)
	assert.Equal(t, http.StatusRequestEntityTooLarge, big.status)
	assert.Equal(t, errTooLarge, big.body)
	assert.Empty(t, itemsOf(t, d.apiGet(url)), "nothing was added")
}

func TestAPIAddMultipartWithoutAFilePartIsRefused(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	const ct = "multipart/form-data; boundary=BB"
	part := func(name, value string) string {
		return "--BB\r\nContent-Disposition: form-data; name=\"" + name + "\"\r\n\r\n" + value + "\r\n"
	}
	post := func(ct, body string) response {
		return d.request(http.MethodPost, "/api/v1/downloads", map[string]string{"Authorization": "Bearer " + d.cfg.AppKey, "Content-Type": ct}, body)
	}

	requireErrorMessage(t, post(ct, "--BB--\r\n"), 400, "missing_nzb", `the form needs an "nzb" or "torrent" file part`)
	requireErrorMessage(t, post(ct, part("name", "x")+"--BB--\r\n"), 400, "missing_nzb", `the form needs an "nzb" or "torrent" file part`)
	requireErrorMessage(t, post(ct, part("bogus", "x")+"--BB--\r\n"), 400, "unknown_field", `unknown form field "bogus"`)
	requireErrorMessage(t, post(ct, part("name", "a")+part("name", "b")+"--BB--\r\n"), 400, "duplicate_field", `the form field "name" is given twice`)
	requireErrorMessage(t, post(ct, part("name", strings.Repeat("a", 4097))+"--BB--\r\n"), 400, "field_too_long", `the form field "name" is too long`)
	requireErrorMessage(t, post("multipart/form-data", "--BB--\r\n"), 400, "invalid_multipart", "the multipart body is not valid")
	assert.Empty(t, itemsOf(t, d.apiGet("/api/v1/downloads")))
}
