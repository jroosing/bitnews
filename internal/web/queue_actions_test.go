package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/core"
)

// trigger parses the HX-Trigger header of a response.
func trigger(t *testing.T, h http.Header) map[string]json.RawMessage {
	t.Helper()
	raw := h.Get("HX-Trigger")
	if raw == "" {
		return nil
	}
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &m), raw)
	return m
}

func toastOf(t *testing.T, h http.Header) (level, message string) {
	t.Helper()
	raw, ok := trigger(t, h)["toast"]
	require.True(t, ok, "no toast in %q", h.Get("HX-Trigger"))
	var ts struct{ Level, Message string }
	require.NoError(t, json.Unmarshal(raw, &ts))
	return ts.Level, ts.Message
}

// parseRows parses a response made of table rows, which the document parser
// would drop outside a table.
func parseRows(t *testing.T, body string) *html.Node {
	t.Helper()
	return parseHTML(t, "<table><tbody>"+body+"</tbody></table>")
}

func TestPauseAndResumeReturnTheUpdatedRow(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetching})

	rec := q.act("/queue/"+string(d.ID)+"/pause", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, q.get(d.ID).Paused)
	doc := parseRows(t, rec.Body.String())
	row := rowFor(doc, d.ID)
	require.NotNil(t, row)
	assert.Equal(t, "Paused", text(badgeOf(row)))
	assert.NotContains(t, rec.Body.String(), "<html", "a fragment")
	assert.NotNil(t, find(row, func(n *html.Node) bool { return attr(n, "action") == "/queue/"+string(d.ID)+"/resume" }), "the button flips")

	rec = q.act("/queue/"+string(d.ID)+"/resume", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, q.get(d.ID).Paused)
	assert.Equal(t, "Downloading", text(badgeOf(rowFor(parseRows(t, rec.Body.String()), d.ID))))
}

func TestPriorityChangeReturnsTheRowWithTheIndicator(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateQueued})

	rec := q.act("/queue/"+string(d.ID)+"/priority", url.Values{"priority": {"force"}})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, core.DownloadPriorityForce, q.get(d.ID).Priority)
	assert.Contains(t, text(rowFor(parseRows(t, rec.Body.String()), d.ID)), "Force")
}

func TestInvalidPriorityIsAToastNotAServerError(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateQueued})

	rec := q.act("/queue/"+string(d.ID)+"/priority", url.Values{"priority": {"urgent"}})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	level, msg := toastOf(t, rec.Header())
	assert.Equal(t, "danger", level)
	assert.Contains(t, msg, "priority")
	assert.Empty(t, rec.Body.String(), "htmx does not swap errors")
	assert.Equal(t, core.DownloadPriorityNormal, q.get(d.ID).Priority)
}

func TestMoveReturnsTheListsInTheNewOrder(t *testing.T) {
	q := newQueueEnv(t)
	a := q.add(core.Download{State: core.StateQueued})
	b := q.add(core.Download{State: core.StateQueued})
	c := q.add(core.Download{State: core.StateQueued})
	order := func(rec interface{ String() string }) []string {
		var ids []string
		for _, tr := range findAll(parseHTML(t, rec.String()), func(n *html.Node) bool { return n.Data == "tr" && strings.HasPrefix(attr(n, "id"), "dl-") }) {
			ids = append(ids, strings.TrimPrefix(attr(tr, "id"), "dl-"))
		}
		return ids
	}

	for _, tc := range []struct {
		id   core.ID
		to   string
		want []core.ID
	}{
		{c.ID, "top", []core.ID{c.ID, a.ID, b.ID}},
		{c.ID, "down", []core.ID{a.ID, c.ID, b.ID}},
		{b.ID, "up", []core.ID{a.ID, b.ID, c.ID}},
		{a.ID, "bottom", []core.ID{b.ID, c.ID, a.ID}},
	} {
		rec := q.act("/queue/"+string(tc.id)+"/move", url.Values{"to": {tc.to}})

		require.Equal(t, http.StatusOK, rec.Code, tc.to)
		var want []string
		for _, id := range tc.want {
			want = append(want, string(id))
		}
		assert.Equal(t, want, order(rec.Body), tc.to)
		assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "queue-lists"), "the whole lists")
	}
}

func TestMoveOfADownloadThatLeftTheQueueIsAToast(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetched})

	rec := q.act("/queue/"+string(d.ID)+"/move", url.Values{"to": {"top"}})

	assert.Equal(t, http.StatusConflict, rec.Code)
	level, _ := toastOf(t, rec.Header())
	assert.Equal(t, "danger", level)
}

func TestCategoryChangeAssignsAndClears(t *testing.T) {
	q := newQueueEnv(t)
	cat, err := q.categories.Create(t.Context(), "tv", "")
	require.NoError(t, err)
	d := q.add(core.Download{State: core.StateQueued})

	rec := q.act("/queue/"+string(d.ID)+"/category", url.Values{"category_id": {string(cat.ID)}})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, cat.ID, q.get(d.ID).CategoryID)
	assert.Contains(t, text(rowFor(parseRows(t, rec.Body.String()), d.ID)), "tv")

	rec = q.act("/queue/"+string(d.ID)+"/category", url.Values{"category_id": {"none"}})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, q.get(d.ID).CategoryID)

	rec = q.act("/queue/"+string(d.ID)+"/category", url.Values{"category_id": {"nosuchcategory"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRemoveDeletesTheDownloadAndReturnsTheLists(t *testing.T) {
	for _, deleteData := range []string{"false", "true"} {
		q := newQueueEnv(t)
		gone := q.add(core.Download{State: core.StateQueued})
		kept := q.add(core.Download{State: core.StateQueued})

		rec := q.act("/queue/"+string(gone.ID)+"/remove", url.Values{"delete_data": {deleteData}})

		require.Equal(t, http.StatusOK, rec.Code, deleteData)
		_, err := q.repo.Get(t.Context(), gone.ID)
		assert.ErrorIs(t, err, core.ErrNotFound)
		doc := parseHTML(t, rec.Body.String())
		assert.Nil(t, rowFor(doc, gone.ID))
		assert.NotNil(t, rowFor(doc, kept.ID))
		level, _ := toastOf(t, rec.Header())
		assert.Equal(t, "success", level)
	}
}

func TestUnknownDownloadIs404WithAToastAndARefresh(t *testing.T) {
	q := newQueueEnv(t)

	for _, action := range []string{"pause", "resume", "priority", "move", "category", "remove"} {
		rec := q.act("/queue/doesnotexist/"+action, url.Values{"priority": {"high"}, "to": {"top"}})

		assert.Equal(t, http.StatusNotFound, rec.Code, action)
		level, _ := toastOf(t, rec.Header())
		assert.Equal(t, "danger", level, action)
		_, refresh := trigger(t, rec.Header())["queue-refresh"]
		assert.True(t, refresh, "%s: the page refreshes its lists", action)
	}
}

func TestPausingAFetchedDownloadIsAToastNotA500(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetched})

	rec := q.act("/queue/"+string(d.ID)+"/pause", nil)

	assert.Equal(t, http.StatusConflict, rec.Code)
	_, msg := toastOf(t, rec.Header())
	assert.Contains(t, msg, "cannot be paused")
}

func TestRemovingADownloadBeingPostProcessedIsAToast(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StatePostProcessing})

	rec := q.act("/queue/"+string(d.ID)+"/remove", url.Values{"delete_data": {"true"}})

	assert.Equal(t, http.StatusConflict, rec.Code)
	_, msg := toastOf(t, rec.Header())
	assert.Contains(t, msg, "post-processed")
	_, err := q.repo.Get(t.Context(), d.ID)
	assert.NoError(t, err, "the download stays")
}

func TestActionOnADownloadThatFinishedMeanwhileRemovesTheRow(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateReady, OutputPath: "/x"})

	rec := q.act("/queue/"+string(d.ID)+"/priority", url.Values{"priority": {"low"}})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, strings.TrimSpace(rec.Body.String()), "an empty swap removes the stale row")
	_, refresh := trigger(t, rec.Header())["queue-refresh"]
	assert.True(t, refresh)
}

func TestActionsNeedASessionAndTheSameSite(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetching})
	path := "/queue/" + string(d.ID) + "/pause"

	rec := q.env.browser().post(path, url.Values{}, htmx())
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "/login", rec.Header().Get("HX-Redirect"))

	rec = q.b.post(path, url.Values{}, htmx(), header("Sec-Fetch-Site", "cross-site")) // signed in, another site
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = q.b.post(path, url.Values{}, htmx(), header("Origin", "https://evil.test"))
	assert.Equal(t, http.StatusForbidden, rec.Code)

	assert.False(t, q.get(d.ID).Paused, "nothing happened")
}

func TestPlainFormPostWorksWithoutJavaScript(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetching})

	rec := q.b.post("/queue/"+string(d.ID)+"/pause", url.Values{})

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/queue", location(rec))
	assert.True(t, q.get(d.ID).Paused)
}

func TestPlainFormErrorShowsAnErrorPage(t *testing.T) {
	q := newQueueEnv(t)

	rec := q.b.post("/queue/nope/pause", url.Values{})

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "no longer exists")
}

func TestRowActionsPostToServerValidatedIDsOnly(t *testing.T) {
	q := newQueueEnv(t)
	cat, err := q.categories.Create(t.Context(), "movies", "")
	require.NoError(t, err)
	for _, name := range hostile {
		q.add(core.Download{Name: name, State: core.StateQueued, CategoryID: cat.ID})
	}

	doc := queueDoc(t, q)

	posts := findAll(doc, func(n *html.Node) bool { return hasAttr(n, "hx-post") })
	require.NotEmpty(t, posts)
	for _, n := range posts {
		assert.Regexp(t, `^/queues?/[a-z0-9]+(/[a-z]+)?$`, attr(n, "hx-post"))
		if hasAttr(n, "hx-vals") {
			assert.Regexp(t, `^\{("[a-z_]+":"[a-z0-9]+"(,)?)+\}$`, attr(n, "hx-vals"), "hx-vals hold ids and enums, never names")
		}
	}
}

func TestRowMenuIsKeyboardOperableAndLabelled(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{Name: "Some.Release", State: core.StateQueued})
	q.add(core.Download{State: core.StateQueued})

	row := rowFor(queueDoc(t, q), d.ID)

	menu := find(row, func(n *html.Node) bool { return attr(n, "x-data") == "menu" })
	require.NotNil(t, menu, "the Alpine menu component of app.js")
	button := find(menu, func(n *html.Node) bool { return n.Data == "button" && attr(n, "aria-haspopup") == "menu" })
	require.NotNil(t, button)
	assert.Contains(t, attr(button, "aria-label"), "Some.Release")
	assert.Equal(t, "open", attr(button, ":aria-expanded"))
	panel := find(menu, func(n *html.Node) bool { return attr(n, "role") == "menu" })
	require.NotNil(t, panel)
	assert.Equal(t, "onPanelKey", attr(panel, "@keydown"))
	items := findAll(panel, func(n *html.Node) bool { return strings.HasPrefix(attr(n, "role"), "menuitem") })
	assert.GreaterOrEqual(t, len(items), 8)
	for _, it := range items {
		assert.Equal(t, "button", it.Data)
		assert.NotEmpty(t, text(it), "every item has a name")
	}
	checked := findAll(panel, func(n *html.Node) bool { return attr(n, "aria-checked") == "true" })
	assert.Len(t, checked, 1, "the current priority; there are no categories to choose")
}

func TestDestructiveItemsAskForConfirmation(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{Name: `<b>"bold"</b>`, State: core.StateQueued})

	row := rowFor(queueDoc(t, q), d.ID)

	removes := findAll(row, func(n *html.Node) bool { return strings.HasSuffix(attr(n, "hx-post"), "/remove") })
	require.Len(t, removes, 2)
	for _, r := range removes {
		assert.NotEmpty(t, attr(r, "hx-confirm"))
		assert.Equal(t, "danger", attr(r, "data-confirm-tone"))
		assert.Contains(t, attr(r, "hx-confirm"), `<b>"bold"</b>`, "the name round-trips as attribute text")
	}
	assert.Contains(t, attr(removes[1], "hx-confirm"), "delete its files")
	assert.Contains(t, attr(removes[1], "hx-vals"), "true")
	assert.Contains(t, attr(removes[0], "hx-vals"), "false")
}

func TestMenuOffersOnlyMovesThatDoSomething(t *testing.T) {
	q := newQueueEnv(t)
	first := q.add(core.Download{State: core.StateQueued})
	middle := q.add(core.Download{State: core.StateQueued})
	last := q.add(core.Download{State: core.StateQueued})
	fetched := q.add(core.Download{State: core.StateFetched})
	moves := func(doc *html.Node, id core.ID) []string {
		var out []string
		for _, n := range findAll(rowFor(doc, id), func(n *html.Node) bool { return strings.HasSuffix(attr(n, "hx-post"), "/move") }) {
			out = append(out, text(n))
		}
		return out
	}

	doc := queueDoc(t, q)

	assert.Equal(t, []string{"Move down", "Move to bottom"}, moves(doc, first.ID))
	assert.Equal(t, []string{"Move to top", "Move up", "Move down", "Move to bottom"}, moves(doc, middle.ID))
	assert.Equal(t, []string{"Move to top", "Move up"}, moves(doc, last.ID))
	assert.Empty(t, moves(doc, fetched.ID), "a fetched download is not in the queue")
}
