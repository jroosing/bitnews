package blackbox_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

const (
	catsPath = "/api/v1/categories"
	// The messages of the app layer for category validation.
	invalidNameMessage = "category name must use only a-z, 0-9, '.', '_' and '-', be 1 to 64 characters long, and not be '.' or '..'"
	invalidDirMessage  = "usenet_dir and torrent_dir must be absolute paths"
	nameTakenMessage   = "a category with this name already exists"
)

func catPath(id string) string { return catsPath + "/" + id }

// catJSON is the exact JSON of a Category; an empty folder is null.
func catJSON(id, name, usenet, torrent string) string {
	q := func(s string) string {
		if s == "" {
			return "null"
		}
		return `"` + s + `"`
	}
	return `{"id":"` + id + `","name":"` + name + `","usenet_dir":` + q(usenet) + `,"torrent_dir":` + q(torrent) + `}`
}

func createCat(t *testing.T, d *daemon, body string) map[string]any {
	t.Helper()
	r := d.api(http.MethodPost, catsPath, body)
	require.Equal(t, http.StatusCreated, r.status, r.body)
	requireAPIHeaders(t, r)
	return obj(t, r)
}

func TestAPICategoriesListTheStoredOneWithNullForAMissingFolder(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.apiGet(catsPath)

	requireOK(t, r)
	assert.Equal(t, `{"items":[`+catJSON(string(d.cfg.CategoryID), categoryName, categoryUsenetDir, "")+`]}`+"\n", r.body)
}

func TestAPICategoriesAreListedByName(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	for _, n := range []string{"zeta", "alpha", "a.b", "a-b", "a_b", "9lives", "mid"} {
		createCat(t, d, `{"name":"`+n+`"}`)
	}

	got := namesOfItems(itemsOf(t, d.apiGet(catsPath)))

	assert.Equal(t, []string{"9lives", "a-b", "a.b", "a_b", "alpha", "mid", "tv", "zeta"}, got, "ordered by name, byte-wise")
}

func TestAPICategoryCreateAnswers201WithTheStoredCategoryAndPersistsIt(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	r := d.api(http.MethodPost, catsPath, `{"name":"movies","usenet_dir":"/media/usenet/movies","torrent_dir":"/media/torrents/movies"}`)

	require.Equal(t, http.StatusCreated, r.status, r.body)
	requireAPIHeaders(t, r)
	id := obj(t, r)["id"].(string)
	assert.Regexp(t, `^[a-z0-9]{20,}$`, id)
	assert.Equal(t, catJSON(id, "movies", "/media/usenet/movies", "/media/torrents/movies")+"\n", r.body)
	d.stop()
	got := storedCategories(t, cfg)["movies"]
	assert.Equal(t, core.ID(id), got.ID)
	assert.Equal(t, "/media/usenet/movies", got.UsenetDir)
	assert.Equal(t, "/media/torrents/movies", got.TorrentDir)
}

func TestAPICategoryCreateTakesOnlyTheNameAndTreatsEmptyAndNullFoldersAsNone(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	a := createCat(t, d, `{"name":"a"}`)
	b := createCat(t, d, `{"name":"b","usenet_dir":"","torrent_dir":""}`)
	c := createCat(t, d, `{"name":"c","usenet_dir":null,"torrent_dir":null}`)
	e := createCat(t, d, `{"name":"e","torrent_dir":"/media/torrents/e"}`)

	for _, m := range []map[string]any{a, b, c} {
		assert.Nil(t, m["usenet_dir"])
		assert.Nil(t, m["torrent_dir"])
	}
	assert.Nil(t, e["usenet_dir"])
	assert.Equal(t, "/media/torrents/e", e["torrent_dir"])
}

func TestAPICategoryCreateAcceptsEveryAllowedCharacterAndLength(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for _, n := range []string{"a", "0", ".x", "..x", "x..", "a.b-c_d", strings.Repeat("a", 64), "-", "_", "..."} {
		r := d.api(http.MethodPost, catsPath, `{"name":"`+n+`"}`)
		assert.Equal(t, http.StatusCreated, r.status, "%q: %s", n, r.body)
	}
}

func TestAPICategoryCreateRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"upper case":            {`{"name":"TV"}`, 400, "invalid_name"},
		"space":                 {`{"name":"tv shows"}`, 400, "invalid_name"},
		"slash":                 {`{"name":"a/b"}`, 400, "invalid_name"},
		"backslash":             {`{"name":"a\\b"}`, 400, "invalid_name"},
		"dot":                   {`{"name":"."}`, 400, "invalid_name"},
		"dot dot":               {`{"name":".."}`, 400, "invalid_name"},
		"empty":                 {`{"name":""}`, 400, "invalid_name"},
		"missing":               {`{}`, 400, "invalid_name"},
		"null":                  {`{"name":null}`, 400, "invalid_name"},
		"65 characters":         {`{"name":"` + strings.Repeat("a", 65) + `"}`, 400, "invalid_name"},
		"non ascii":             {`{"name":"série"}`, 400, "invalid_name"},
		"newline":               {`{"name":"a\nb"}`, 400, "invalid_name"},
		"NUL":                   {`{"name":"a\u0000b"}`, 400, "invalid_name"},
		"relative usenet dir":   {`{"name":"x","usenet_dir":"rel/dir"}`, 400, "invalid_dir_override"},
		"relative torrent dir":  {`{"name":"x","torrent_dir":"rel/dir"}`, 400, "invalid_dir_override"},
		"dot dot in dir":        {`{"name":"x","usenet_dir":"/a/../b"}`, 400, "invalid_dir_override"},
		"dot in dir":            {`{"name":"x","usenet_dir":"/a/./b"}`, 400, "invalid_dir_override"},
		"trailing slash":        {`{"name":"x","usenet_dir":"/a/b/"}`, 400, "invalid_dir_override"},
		"doubled slash":         {`{"name":"x","torrent_dir":"/a//b"}`, 400, "invalid_dir_override"},
		"name checked first":    {`{"name":"X","usenet_dir":"rel"}`, 400, "invalid_name"},
		"old dir_override":      {`{"name":"x","dir_override":"/data/x"}`, 400, "invalid_json"},
		"unknown field":         {`{"name":"x","id":"abc"}`, 400, "invalid_json"},
		"name wrong type":       {`{"name":5}`, 400, "invalid_json"},
		"dir wrong type":        {`{"name":"x","usenet_dir":5}`, 400, "invalid_json"},
		"array":                 {`[]`, 400, "invalid_json"},
		"truncated":             {`{"name":"x"`, 400, "invalid_json"},
		"two objects":           {`{"name":"x"}{"name":"y"}`, 400, "invalid_json"},
		"garbage after":         {`{"name":"x"} x`, 400, "invalid_json"},
		"duplicate name":        {`{"name":"tv"}`, 409, "name_taken"},
		"duplicate name w dirs": {`{"name":"tv","usenet_dir":"/other"}`, 409, "name_taken"},
	} {
		t.Run(name, func(t *testing.T) {
			r := d.api(http.MethodPost, catsPath, tc.body)

			requireError(t, r, tc.status, tc.code)
		})
	}
	assert.Equal(t, []string{"tv"}, namesOfItems(itemsOf(t, d.apiGet(catsPath))), "nothing was created")
}

func TestAPICategoryErrorMessagesWrittenByTheApp(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	requireErrorMessage(t, d.api(http.MethodPost, catsPath, `{"name":"TV"}`), 400, "invalid_name", invalidNameMessage)
	requireErrorMessage(t, d.api(http.MethodPost, catsPath, `{"name":"x","usenet_dir":"rel"}`), 400, "invalid_dir_override", invalidDirMessage)
	requireErrorMessage(t, d.api(http.MethodPost, catsPath, `{"name":"tv"}`), 409, "name_taken", nameTakenMessage)
	requireErrorMessage(t, d.api(http.MethodPatch, catPath(string(d.cfg.CategoryID)), `{}`), 400, "empty_patch",
		"nothing to change: give name, usenet_dir or torrent_dir")
	requireErrorMessage(t, d.request(http.MethodPost, catsPath, d.apiHeaders(), ""), 400, "invalid_json", "the request body is empty")
}

func TestAPICategoryPatchRenamesSetsAndClearsFolders(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	id := string(cfg.CategoryID)
	patch := func(body string) response {
		r := d.api(http.MethodPatch, catPath(id), body)
		requireOK(t, r)
		return r
	}

	assert.Equal(t, catJSON(id, "series", categoryUsenetDir, "")+"\n", patch(`{"name":"series"}`).body)
	assert.Equal(t, catJSON(id, "series", categoryUsenetDir, "/data/series")+"\n", patch(`{"torrent_dir":"/data/series"}`).body)
	assert.Equal(t, catJSON(id, "series", "/data/usenet/series", "/data/series")+"\n", patch(`{"usenet_dir":"/data/usenet/series"}`).body)
	assert.Equal(t, catJSON(id, "shows", "/u", "/t")+"\n", patch(`{"name":"shows","usenet_dir":"/u","torrent_dir":"/t"}`).body)
	assert.Equal(t, catJSON(id, "shows", "", "/t")+"\n", patch(`{"usenet_dir":null}`).body, "null removes the override")
	assert.Equal(t, catJSON(id, "shows", "", "")+"\n", patch(`{"torrent_dir":""}`).body, "so does an empty string")
	assert.Equal(t, catJSON(id, "shows", "/again", "")+"\n", patch(`{"usenet_dir":"/again","torrent_dir":null}`).body)
	assert.Equal(t, catJSON(id, "shows", "/again", "")+"\n", patch(`{"name":"shows"}`).body, "the same name is no conflict")
	d.stop()
	got := storedCategories(t, cfg)["shows"]
	assert.Equal(t, core.ID(id), got.ID)
	assert.Equal(t, "/again", got.UsenetDir)
	assert.Empty(t, got.TorrentDir)
	assert.NotContains(t, storedCategories(t, cfg), categoryName)
}

func TestAPICategoryPatchRejectsInvalidChanges(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	createCat(t, d, `{"name":"movies"}`)
	path := catPath(string(d.cfg.CategoryID))

	for name, tc := range map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"empty object":        {path, `{}`, 400, "empty_patch"},
		"null name":           {path, `{"name":null}`, 400, "empty_patch"},
		"bad name":            {path, `{"name":"Bad"}`, 400, "invalid_name"},
		"empty name":          {path, `{"name":""}`, 400, "invalid_name"},
		"taken name":          {path, `{"name":"movies"}`, 409, "name_taken"},
		"relative dir":        {path, `{"usenet_dir":"rel"}`, 400, "invalid_dir_override"},
		"unclean dir":         {path, `{"torrent_dir":"/a/../b"}`, 400, "invalid_dir_override"},
		"old dir_override":    {path, `{"dir_override":"/x"}`, 400, "invalid_json"},
		"unknown field":       {path, `{"id":"x"}`, 400, "invalid_json"},
		"wrong type":          {path, `{"name":1}`, 400, "invalid_json"},
		"dir wrong type":      {path, `{"usenet_dir":[]}`, 400, "invalid_json"},
		"unknown id":          {catPath("nope"), `{"name":"x"}`, 404, "not_found"},
		"unknown id empty":    {catPath("nope"), `{}`, 400, "empty_patch"},
		"unknown id bad name": {catPath("nope"), `{"name":"X"}`, 404, "not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			r := d.api(http.MethodPatch, tc.path, tc.body)

			requireError(t, r, tc.status, tc.code)
		})
	}
	got := namesOfItems(itemsOf(t, d.apiGet(catsPath)))
	assert.Equal(t, []string{"movies", "tv"}, got, "no refused patch changed anything")
	assert.Equal(t, errBody("not_found", "not found"), d.api(http.MethodPatch, catPath("nope"), `{"name":"x"}`).body)
}

func TestAPICategoryDeleteRemovesItAndClearsItFromItsDownloads(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	tagged := newDownload("tagged", inCategory(cfg.CategoryID))
	untagged := newDownload("untagged", at(1))
	seedDownloads(t, cfg, tagged, untagged)
	d := startDaemon(t, cfg)

	r := d.api(http.MethodDelete, catPath(string(cfg.CategoryID)), "")

	requireNoContent(t, r)
	assert.JSONEq(t, `{"items":[]}`, d.apiGet(catsPath).body)
	assert.Nil(t, obj(t, d.apiGet(dlPath(tagged)))["category_id"], "the Download lost its Category")
	assert.Equal(t, http.StatusOK, d.apiGet(dlPath(tagged)).status, "but it is still there")
	again := d.api(http.MethodDelete, catPath(string(cfg.CategoryID)), "")
	assert.Equal(t, errBody("not_found", "not found"), again.body)
	assert.Equal(t, http.StatusNotFound, again.status)
	d.stop()
	got, err := storedDownload(t, cfg, tagged.ID)
	require.NoError(t, err)
	assert.Empty(t, got.CategoryID)
	assert.Empty(t, storedCategories(t, cfg))
}

func TestAPICategoryDeleteOfAnUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.api(http.MethodDelete, catPath("nope"), "")

	assert.Equal(t, http.StatusNotFound, r.status)
	assert.Equal(t, errBody("not_found", "not found"), r.body)
}

func TestAPICategoryNameCanBeReusedAfterDelete(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	first := createCat(t, d, `{"name":"temp"}`)
	requireNoContent(t, d.api(http.MethodDelete, catPath(first["id"].(string)), ""))

	second := createCat(t, d, `{"name":"temp"}`)

	assert.NotEqual(t, first["id"], second["id"])
}

func TestAPICategoryRoutesRefuseOtherMethodsWithAllow(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	put := d.api(http.MethodPut, catsPath, `{}`)
	get := d.apiGet(catPath("x"))

	assert.Equal(t, http.StatusMethodNotAllowed, put.status)
	assert.Equal(t, errMethod, put.body)
	assert.Equal(t, "GET, HEAD, POST", put.header.Get("Allow"))
	assert.Equal(t, http.StatusMethodNotAllowed, get.status)
	assert.Equal(t, "DELETE, PATCH", get.header.Get("Allow"))
}
