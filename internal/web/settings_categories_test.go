package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func categoryNames(t *testing.T, e *env) []string {
	t.Helper()
	cats, err := e.cats.List(context.Background())
	require.NoError(t, err)
	var out []string
	for _, c := range cats {
		out = append(out, c.Name)
	}
	return out
}

// settingsParseRows parses a fragment of table rows, which html.Parse would drop
// outside a table.
func settingsParseRows(t *testing.T, body string) *html.Node {
	t.Helper()
	return parseHTML(t, "<table><tbody>"+body+"</tbody></table>")
}

func catRows(doc *html.Node) []*html.Node {
	return findAll(doc, func(n *html.Node) bool { return n.Data == "tr" && strings.HasPrefix(attr(n, "id"), "category-") })
}

func TestCategoriesCardShowsAnEmptyStateAndTheAddForm(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/paths").Body.String())

	card := byID(doc, "categories-card")
	require.NotNil(t, card)
	assert.Contains(t, text(card), "No categories yet")
	form := byID(doc, "category-form")
	assert.Equal(t, "/settings/categories", attr(form, "hx-post"))
	assert.Equal(t, "category-name", attr(find(form, func(n *html.Node) bool { return n.Data == "label" }), "for"))
	assert.Contains(t, attr(byID(doc, "category-name"), "aria-describedby"), "category-name-help")
	assert.NotNil(t, find(form, func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "_csrf" }))
	assert.Equal(t, "categoryForm", attr(form, "x-data"))
}

func TestCreatingACategory(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/categories", url.Values{"name": {"tv"}, "dir_override": {"/data/media/tv"}})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"tv"}, categoryNames(t, e))
	cats, _ := e.cats.List(context.Background())
	assert.Equal(t, "/data/media/tv", cats[0].DirOverride)
	doc := parseHTML(t, rec.Body.String())
	rows := catRows(doc)
	require.Len(t, rows, 1)
	assert.Contains(t, text(rows[0]), "tv")
	assert.Contains(t, text(rows[0]), "/data/media/tv")
	level, _ := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	assert.Empty(t, attr(byID(doc, "category-name"), "value"), "the add form is empty again")
}

func TestACategoryWithoutAFolderShowsTheDefault(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/categories", url.Values{"name": {"movies"}})

	row := catRows(parseHTML(t, rec.Body.String()))[0]
	assert.Contains(t, text(row), "Default")
}

func TestInvalidCategoryInputIsReportedNextToTheFields(t *testing.T) {
	long := strings.Repeat("a", 65)
	for _, tc := range []struct {
		name, dir string
		nameErr   bool
		dirErr    bool
	}{
		{"UPPER", "", true, false},
		{"with space", "", true, false},
		{"..", "", true, false},
		{".", "", true, false},
		{"", "", true, false},
		{long, "", true, false},
		{"<script>alert(1)</script>", "", true, false},
		{"tv/../x", "", true, false},
		{"tv", "relative/dir", false, true},
		{"BAD", "relative", true, true},
	} {
		t.Run(tc.name+"|"+tc.dir, func(t *testing.T) {
			e, b := settingsSignedIn(t)

			rec := b.settingsPost("/settings/categories", url.Values{"name": {tc.name}, "dir_override": {tc.dir}})

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			doc := parseHTML(t, rec.Body.String())
			assert.Equal(t, tc.nameErr, byID(doc, "category-name-error") != nil)
			assert.Equal(t, tc.dirErr, byID(doc, "category-dir-error") != nil)
			assert.Empty(t, categoryNames(t, e), "nothing is created")
			assert.Nil(t, find(doc, func(n *html.Node) bool { return n.Data == "script" && !hasAttr(n, "src") }), "hostile input is escaped")
			assert.NotContains(t, rec.Body.String(), "<script>alert")
			if tc.nameErr {
				assert.Equal(t, "true", attr(byID(doc, "category-name"), "aria-invalid"))
				assert.Contains(t, attr(byID(doc, "category-name"), "aria-describedby"), "category-name-error")
			}
		})
	}
}

func TestADuplicateCategoryNameIsReported(t *testing.T) {
	e, b := settingsSignedIn(t)
	_, err := e.cats.Create(context.Background(), "tv", "")
	require.NoError(t, err)

	rec := b.settingsPost("/settings/categories", url.Values{"name": {"tv"}})

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "category-name-error")), "already exists")
}

func TestEditingACategoryFolderInPlace(t *testing.T) {
	e, b := settingsSignedIn(t)
	c, err := e.cats.Create(context.Background(), "tv", "/old")
	require.NoError(t, err)
	id := string(c.ID)

	edit := b.settingsDo("GET", "/settings/categories/"+id+"/edit", nil)

	require.Equal(t, http.StatusOK, edit.Code)
	doc := parseHTML(t, edit.Body.String())
	in := byID(doc, "category-dir-"+id)
	require.NotNil(t, in)
	assert.Equal(t, "/old", attr(in, "value"))
	assert.Equal(t, "Folder for tv (optional)", text(find(doc, func(n *html.Node) bool { return n.Data == "label" })))

	save := b.settingsPost("/settings/categories/"+id, url.Values{"dir_override": {"/new/tv"}})

	require.Equal(t, http.StatusOK, save.Code, save.Body.String())
	cats, _ := e.cats.List(context.Background())
	assert.Equal(t, "/new/tv", cats[0].DirOverride)
	row := catRows(settingsParseRows(t, save.Body.String()))
	require.Len(t, row, 1, "just the row comes back")
	assert.Contains(t, text(row[0]), "/new/tv")

	clear := b.settingsPost("/settings/categories/"+id, url.Values{"dir_override": {""}})
	require.Equal(t, http.StatusOK, clear.Code)
	cats, _ = e.cats.List(context.Background())
	assert.Empty(t, cats[0].DirOverride, "an empty folder removes the override")

	cancel := b.settingsDo("GET", "/settings/categories/"+id, nil)
	assert.Equal(t, http.StatusOK, cancel.Code)
	assert.Len(t, catRows(settingsParseRows(t, cancel.Body.String())), 1)
}

func TestEditingACategoryFolderRejectsARelativePath(t *testing.T) {
	e, b := settingsSignedIn(t)
	c, _ := e.cats.Create(context.Background(), "tv", "/old")

	rec := b.settingsPost("/settings/categories/"+string(c.ID), url.Values{"dir_override": {"rel"}})

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.Contains(t, text(byID(doc, "category-dir-"+string(c.ID)+"-error")), "absolute")
	assert.Equal(t, "rel", attr(byID(doc, "category-dir-"+string(c.ID)), "value"))
	cats, _ := e.cats.List(context.Background())
	assert.Equal(t, "/old", cats[0].DirOverride)
}

func TestDeletingACategoryAsksForConfirmationAndKeepsDownloads(t *testing.T) {
	e, b := settingsSignedIn(t)
	c, _ := e.cats.Create(context.Background(), "tv", "")
	keep, _ := e.cats.Create(context.Background(), "movies", "")
	doc := parseHTML(t, b.get("/settings/paths").Body.String())
	del := find(doc, func(n *html.Node) bool {
		return n.Data == "button" && attr(n, "hx-delete") == "/settings/categories/"+string(c.ID)
	})
	require.NotNil(t, del)
	assert.Equal(t, "Delete the category tv? Downloads keep their files; their category is cleared.", attr(del, "hx-confirm"))
	assert.Equal(t, "danger", attr(del, "data-confirm-tone"))
	assert.Equal(t, "Delete tv", attr(del, "aria-label"))

	rec := b.settingsDo("DELETE", "/settings/categories/"+string(c.ID), nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"movies"}, categoryNames(t, e))
	rows := catRows(parseHTML(t, rec.Body.String()))
	require.Len(t, rows, 1)
	assert.Equal(t, "category-"+string(keep.ID), attr(rows[0], "id"))
	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	assert.Contains(t, msg, "keep their files")
}

func TestCategoryRoutesAnswer404ForUnknownIDs(t *testing.T) {
	_, b := settingsSignedIn(t)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/settings/categories/nope"},
		{"GET", "/settings/categories/nope/edit"},
		{"POST", "/settings/categories/nope"},
		{"DELETE", "/settings/categories/nope"},
	} {
		rec := b.settingsDo(rt.method, rt.path, url.Values{"dir_override": {"/x"}})

		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", rt.method, rt.path)
	}
}

func TestHostileFolderOverridesAreEscaped(t *testing.T) {
	e, b := settingsSignedIn(t)
	hostile := `/x/<img src=x onerror=alert(1)>"'`
	_, err := e.cats.Create(context.Background(), "tv", hostile)
	require.NoError(t, err)

	rec := b.get("/settings/paths")

	doc := parseHTML(t, rec.Body.String())
	assert.Nil(t, find(doc, func(n *html.Node) bool { return n.Data == "img" }))
	assert.Contains(t, text(catRows(doc)[0]), hostile)

	edit := b.settingsDo("GET", "/settings/categories/"+string(mustCatID(t, e, "tv"))+"/edit", nil)
	assert.Equal(t, hostile, attr(byID(parseHTML(t, edit.Body.String()), "category-dir-"+string(mustCatID(t, e, "tv"))), "value"))
	assert.NotContains(t, edit.Body.String(), "<img src=x")
}

func mustCatID(t *testing.T, e *env, name string) core.ID {
	t.Helper()
	cats, err := e.cats.List(context.Background())
	require.NoError(t, err)
	for _, c := range cats {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("no category %q", name)
	return ""
}

func TestPlainCategoryFormPostRedirectsBackOrShowsTheError(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.post("/settings/categories", url.Values{"name": {"tv"}, "_csrf": {b.csrf()}})
	assert.Equal(t, http.StatusOK, rec.Code, "the whole page again")
	assert.NotNil(t, find(parseHTML(t, rec.Body.String()), func(n *html.Node) bool { return n.Data == "html" }))
	assert.Equal(t, []string{"tv"}, categoryNames(t, e))

	rec = b.post("/settings/categories", url.Values{"name": {"BAD"}, "_csrf": {b.csrf()}})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "category-name-error"))
}
