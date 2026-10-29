package web_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/core"
)

func TestHistoryListsReadyAndFailedNewestFirst(t *testing.T) {
	d := newDetailEnv(t)
	oldest := d.add(core.Download{Name: "Oldest.Release"})
	d.add(core.Download{Name: "Active.Not.Shown", State: core.StateFetching})
	failed := d.add(core.Download{Name: "Failed.Release", State: core.StateFailed, UpdatedAt: oldest.UpdatedAt.Add(time.Hour)})
	newest := d.add(core.Download{Name: "Newest.Release", ReadyAt: failed.UpdatedAt.Add(time.Hour), UpdatedAt: failed.UpdatedAt.Add(time.Hour)})

	rec := d.b.get("/history")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"Newest.Release", "Failed.Release", "Oldest.Release"}, historyRowNames(t, rec.Body.String()))
	assert.NotContains(t, rec.Body.String(), "Active.Not.Shown")
	assert.Contains(t, rec.Body.String(), "/downloads/"+string(newest.ID))
}

func TestHistoryShowsOutputPathOfReadyAndReasonOfFailed(t *testing.T) {
	d := newDetailEnv(t)
	ready := d.add(core.Download{Name: "Ready.One", OutputPath: "/media/complete/Ready.One"})
	d.add(core.Download{Name: "Failed.One", State: core.StateFailed, FailureReason: core.FailureRepairInsufficient,
		FailureDetail: "4 blocks needed, 2 available"})

	rec := d.b.get("/history")

	doc := parseHTML(t, rec.Body.String())
	readyRow := byID(doc, "download-"+string(ready.ID))
	require.NotNil(t, readyRow)
	assert.Contains(t, text(readyRow), "/media/complete/Ready.One")
	copyBtn := find(readyRow, func(n *html.Node) bool { return n.Data == "button" && hasAttr(n, "data-copy") })
	require.NotNil(t, copyBtn)
	assert.Equal(t, "/media/complete/Ready.One", attr(copyBtn, "data-copy"))

	body := text(doc)
	assert.Contains(t, body, "Not enough repair data")
	assert.Contains(t, body, "4 blocks needed, 2 available")
}

func TestHistoryFiltersByStateKindAndCategory(t *testing.T) {
	d := newDetailEnv(t)
	cat, err := d.catSvc.Create(t.Context(), "tv", "", "")
	require.NoError(t, err)
	d.add(core.Download{Name: "Usenet.Ready", CategoryID: cat.ID})
	d.add(core.Download{Name: "Usenet.Failed", State: core.StateFailed})
	d.add(core.Download{Name: "Torrent.Ready", SourceKind: core.SourceTorrent})

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"state=failed", []string{"Usenet.Failed"}},
		{"state=ready", []string{"Torrent.Ready", "Usenet.Ready"}},
		{"kind=torrent", []string{"Torrent.Ready"}},
		{"kind=nzb", []string{"Usenet.Failed", "Usenet.Ready"}},
		{"category=" + string(cat.ID), []string{"Usenet.Ready"}},
		{"kind=nzb&state=failed", []string{"Usenet.Failed"}},
		{"state=bogus&kind=bogus&category=bogus", []string{"Torrent.Ready", "Usenet.Failed", "Usenet.Ready"}},
	} {
		rec := d.b.get("/history?" + tc.query)

		require.Equal(t, http.StatusOK, rec.Code, tc.query)
		assert.Equal(t, tc.want, historyRowNames(t, rec.Body.String()), tc.query)
	}
}

func TestHistoryFilterFormWorksWithoutJavaScriptAndWithHTMX(t *testing.T) {
	d := newDetailEnv(t)

	doc := parseHTML(t, d.b.get("/history").Body.String())

	form := byID(doc, "history-filters")
	require.NotNil(t, form)
	assert.Equal(t, "get", attr(form, "method"))
	assert.Equal(t, "/history", attr(form, "action"))
	assert.Equal(t, "/history", attr(form, "hx-get"))
	assert.Equal(t, "true", attr(form, "hx-push-url"))
	assert.Equal(t, "#history-results", attr(form, "hx-target"))
	for _, name := range []string{"state", "kind", "category"} {
		sel := find(form, func(n *html.Node) bool { return n.Data == "select" && attr(n, "name") == name })
		require.NotNil(t, sel, name)
	}
	assert.NotNil(t, find(form, func(n *html.Node) bool { return n.Data == "noscript" }), "an Apply button for no-JS browsers")
}

func TestHistoryEmptyStatesPerFilter(t *testing.T) {
	d := newDetailEnv(t)
	for _, tc := range []struct{ query, title string }{
		{"", "Nothing in the history yet"},
		{"state=failed", "No failed downloads"},
		{"state=ready", "No ready downloads yet"},
		{"kind=torrent", "No downloads match these filters"},
	} {
		rec := d.b.get("/history?" + tc.query)

		assert.Contains(t, text(parseHTML(t, rec.Body.String())), tc.title, tc.query)
	}
}

func TestHistoryPagesWithACursorAndLoadsMore(t *testing.T) {
	d := newDetailEnv(t)
	for i := 1; i <= 30; i++ {
		d.add(core.Download{Name: fmt.Sprintf("Release.%02d", i)})
	}

	first := d.b.get("/history")

	names := historyRowNames(t, first.Body.String())
	require.Len(t, names, 25)
	assert.Equal(t, "Release.30", names[0])
	assert.Equal(t, "Release.06", names[24])
	doc := parseHTML(t, first.Body.String())
	more := byID(doc, "history-more")
	require.NotNil(t, more)
	assert.Contains(t, text(more), "Showing 1 to 25 of 30 downloads")
	next := find(more, func(n *html.Node) bool {
		return n.Data == "a" && attr(n, "rel") == "" && strings.HasPrefix(attr(n, "href"), "/history?before=")
	})
	require.NotNil(t, next, "a Show older link that also works without JavaScript")
	assert.Equal(t, attr(next, "href")+"&append=1", attr(next, "hx-get"))
	assert.Equal(t, "beforeend", attr(next, "hx-swap"))
	assert.Equal(t, "#history-rows > *", attr(next, "hx-select"))

	second := d.b.get(attr(next, "href"))

	assert.Equal(t, []string{"Release.05", "Release.04", "Release.03", "Release.02", "Release.01"}, historyRowNames(t, second.Body.String()))
	doc = parseHTML(t, second.Body.String())
	assert.Contains(t, text(byID(doc, "history-more")), "Showing 26 to 30 of 30 downloads")
	assert.Nil(t, find(byID(doc, "history-more"), func(n *html.Node) bool { return attr(n, "hx-get") != "" }), "no older page")
	assert.NotNil(t, find(byID(doc, "history-more"), func(n *html.Node) bool { return n.Data == "a" && attr(n, "href") == "/history" }), "back to newest")

	appended := d.b.get(attr(next, "hx-get"), htmx())
	assert.Contains(t, text(byID(parseHTML(t, appended.Body.String()), "history-more")), "Showing 1 to 30 of 30 downloads")
}

func TestHistoryPagingKeepsTheFilter(t *testing.T) {
	d := newDetailEnv(t)
	for range 27 {
		d.add(core.Download{State: core.StateFailed})
	}
	d.add(core.Download{Name: "Ready.Hidden"})

	rec := d.b.get("/history?state=failed")

	next := find(parseHTML(t, rec.Body.String()), func(n *html.Node) bool {
		return n.Data == "a" && strings.HasPrefix(attr(n, "href"), "/history?") && strings.Contains(attr(n, "href"), "before=")
	})
	require.NotNil(t, next)
	assert.Contains(t, attr(next, "href"), "state=failed")
	second := d.b.get(attr(next, "href"))
	assert.Len(t, historyRowNames(t, second.Body.String()), 2)
}

func TestHistoryIgnoresAMalformedCursor(t *testing.T) {
	d := newDetailEnv(t)
	d.add(core.Download{Name: "Only.One"})

	for _, before := range []string{"garbage", "1-", "-abc", "99999999999999999999-x", "' OR 1=1"} {
		rec := d.b.get("/history?before=" + url.QueryEscape(before))

		require.Equal(t, http.StatusOK, rec.Code, before)
		assert.Equal(t, []string{"Only.One"}, historyRowNames(t, rec.Body.String()), before)
	}
}

func TestHistoryEscapesHostileNamesAndDetails(t *testing.T) {
	d := newDetailEnv(t)
	hostile := `<script>alert(1)</script>"><img src=x onerror=alert(2)>` + "\u202e" + `evil.exe`
	d.add(core.Download{Name: hostile, OutputPath: `/x"><b>path</b>`})
	d.add(core.Download{Name: "Failed", State: core.StateFailed, FailureReason: core.FailureInternal,
		FailureDetail: "fetched https://idx.example/get?apikey=SECRETKEY123&x=1 <b>bold</b>\n\x00"})

	rec := d.b.get("/history")

	body := rec.Body.String()
	assert.NotContains(t, body, "<script>alert")
	assert.NotContains(t, body, "<img src=x")
	assert.NotContains(t, body, "<b>path</b>")
	assert.NotContains(t, body, "<b>bold</b>")
	assert.NotContains(t, body, "\u202e")
	assert.NotContains(t, body, "SECRETKEY123")
	assert.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;")
}

func TestHistoryRowsOfEveryFailureReasonReadAsText(t *testing.T) {
	d := newDetailEnv(t)
	for i, reason := range allFailureReasons {
		d.add(core.Download{Name: fmt.Sprintf("Failed.%d", i), State: core.StateFailed, FailureReason: reason})
	}

	rec := d.b.get("/history?state=failed")

	body := text(parseHTML(t, rec.Body.String()))
	for _, reason := range allFailureReasons {
		assert.Contains(t, body, failureTitles[reason], string(reason))
	}
	assert.NotContains(t, body, "repair_insufficient")
	assert.NotContains(t, body, "missing_articles")
}

var allFailureReasons = []core.FailureReason{
	core.FailureMissingArticles, core.FailureRepairInsufficient, core.FailureArchiveUnsupported,
	core.FailureArchivePassword, core.FailureArchiveCorrupt, core.FailureDiskFull,
	core.FailurePathViolation, core.FailureSourceInvalid, core.FailureInternal,
}

var failureTitles = map[core.FailureReason]string{
	core.FailureMissingArticles:    "Articles are missing",
	core.FailureRepairInsufficient: "Not enough repair data",
	core.FailureArchiveUnsupported: "Unsupported archive format",
	core.FailureArchivePassword:    "Archive needs a password",
	core.FailureArchiveCorrupt:     "Archive is damaged",
	core.FailureDiskFull:           "Not enough disk space",
	core.FailurePathViolation:      "Unsafe file path",
	core.FailureSourceInvalid:      "The source cannot be used",
	core.FailureInternal:           "Internal error",
}

func TestHistoryRowMenuOffersRemoveButOnlyReadyOffersDeleteFiles(t *testing.T) {
	d := newDetailEnv(t)
	ready := d.add(core.Download{Name: "R"})
	failed := d.add(core.Download{Name: "F", State: core.StateFailed})

	doc := parseHTML(t, d.b.get("/history").Body.String())

	for id, wantDelete := range map[core.ID]bool{ready.ID: true, failed.ID: false} {
		row := byID(doc, "download-"+string(id))
		posts := findAll(row, func(n *html.Node) bool { return attr(n, "hx-post") == "/downloads/"+string(id)+"/delete" })
		require.Len(t, posts, map[bool]int{true: 2, false: 1}[wantDelete])
		for _, p := range posts {
			assert.NotEmpty(t, attr(p, "hx-confirm"))
			assert.Contains(t, attr(p, "hx-vals"), `"from":"history"`)
		}
		assert.NotContains(t, text(row), "Retry")
	}
}

func TestHistoryRequiresASession(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.env.browser().get("/history?state=failed")

	assert.Equal(t, http.StatusSeeOther, rec.Code)
}
