package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

type categoryJSON struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	DirOverride *string `json:"dir_override"`
}

func (e *env) createCategory(key, body string) categoryJSON {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/categories", body, withBearer(key))
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	var c categoryJSON
	decode(e.t, rec, &c)
	return c
}

func TestCreateCategoryReturns201WithTheStoredCategory(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()

	rec := e.do("POST", "/api/v1/categories", `{"name":"tv","dir_override":"/data/tv"}`, withBearer(key))

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var got categoryJSON
	decode(t, rec, &got)
	assert.NotEmpty(t, got.ID)
	assert.Equal(t, "tv", got.Name)
	require.NotNil(t, got.DirOverride)
	assert.Equal(t, "/data/tv", *got.DirOverride)
	stored, err := e.cats.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, "tv", stored.Name)
}

func TestListCategoriesShowsNullForMissingDirOverride(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	empty := e.do("GET", "/api/v1/categories", "", withBearer(key))
	c := e.createCategory(key, `{"name":"tv"}`)

	rec := e.do("GET", "/api/v1/categories", "", withBearer(key))

	assert.JSONEq(t, `{"items":[]}`, empty.Body.String())
	assert.JSONEq(t, `{"items":[{"id":"`+c.ID+`","name":"tv","dir_override":null}]}`, rec.Body.String())
	assert.Equal(t, http.StatusUnauthorized, e.do("GET", "/api/v1/categories", "").Code)
}

func TestCreateCategoryRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	e.createCategory(key, `{"name":"tv"}`)
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"bad name":        {`{"name":"TV Shows"}`, 400, "invalid_name"},
		"dot dot":         {`{"name":".."}`, 400, "invalid_name"},
		"missing name":    {`{}`, 400, "invalid_name"},
		"relative dir":    {`{"name":"x","dir_override":"rel/dir"}`, 400, "invalid_dir_override"},
		"duplicate":       {`{"name":"tv"}`, 409, "name_taken"},
		"unknown field":   {`{"name":"x","id":"abc"}`, 400, "invalid_json"},
		"wrong json type": {`{"name":5}`, 400, "invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("POST", "/api/v1/categories", tc.body, withBearer(key))

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
		})
	}
}

func TestPatchCategoryRenamesSetsAndClearsDirOverride(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	c := e.createCategory(key, `{"name":"tv"}`)
	path := "/api/v1/categories/" + c.ID

	renamed := e.do("PATCH", path, `{"name":"series"}`, withBearer(key))
	withDir := e.do("PATCH", path, `{"dir_override":"/data/series"}`, withBearer(key))
	cleared := e.do("PATCH", path, `{"dir_override":null}`, withBearer(key))

	assert.JSONEq(t, `{"id":"`+c.ID+`","name":"series","dir_override":null}`, renamed.Body.String())
	assert.JSONEq(t, `{"id":"`+c.ID+`","name":"series","dir_override":"/data/series"}`, withDir.Body.String())
	assert.JSONEq(t, `{"id":"`+c.ID+`","name":"series","dir_override":null}`, cleared.Body.String())
}

func TestPatchCategoryRejectsInvalidChanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	c := e.createCategory(key, `{"name":"tv"}`)
	e.createCategory(key, `{"name":"movies"}`)
	path := "/api/v1/categories/" + c.ID

	bad := e.do("PATCH", path, `{"name":"Bad"}`, withBearer(key))
	taken := e.do("PATCH", path, `{"name":"movies"}`, withBearer(key))
	empty := e.do("PATCH", path, `{}`, withBearer(key))
	missing := e.do("PATCH", "/api/v1/categories/nope", `{"name":"x"}`, withBearer(key))

	assert.Equal(t, 400, bad.Code)
	assert.Equal(t, "invalid_name", errorOf(t, bad))
	assert.Equal(t, 409, taken.Code)
	assert.Equal(t, 400, empty.Code)
	assert.Equal(t, "empty_patch", errorOf(t, empty))
	assert.Equal(t, 404, missing.Code)
}

func TestDeleteCategoryClearsItFromDownloadsAndAnnouncesThem(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	c := e.createCategory(key, `{"name":"tv"}`)
	d := e.addDownload("show", 0)
	d.CategoryID = core.ID(c.ID)
	require.NoError(t, e.downloads.Update(context.Background(), d))
	events := e.subscribe()

	rec := e.do("DELETE", "/api/v1/categories/"+c.ID, "", withBearer(key))
	missing := e.do("DELETE", "/api/v1/categories/"+c.ID, "", withBearer(key))

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, http.StatusNotFound, missing.Code)
	got, err := e.downloads.Get(context.Background(), d.ID)
	require.NoError(t, err)
	assert.Empty(t, got.CategoryID)
	require.Len(t, events(), 1)
}
