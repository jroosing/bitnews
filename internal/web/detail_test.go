package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/view"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

func TestDetailOfAReadyUsenetDownload(t *testing.T) {
	d := newDetailEnv(t)
	cat, err := d.catSvc.Create(t.Context(), "movies", "")
	require.NoError(t, err)
	dl := d.add(core.Download{Name: "Some.Movie.2024", CategoryID: cat.ID, TotalBytes: 3 << 30, DoneBytes: 3 << 30,
		OutputPath: "/media/complete/Some.Movie.2024", Priority: core.DownloadPriorityHigh})
	d.detail.reports[dl.ID] = web.FetchReport{MissingArticles: 3, MissingBytes: 2 << 20, DamagedArticles: 1, DamagedBytes: 1 << 20, MissingFiles: 2}

	rec := d.b.get("/downloads/" + string(dl.ID))

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.Equal(t, "Some.Movie.2024", text(find(doc, func(n *html.Node) bool { return n.Data == "h1" })))
	body := text(doc)
	for _, want := range []string{"Ready", "movies", "3.0 GB", "/media/complete/Some.Movie.2024", "Missing articles", "Damaged articles", "Missing files"} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "Share limit", "usenet downloads have no share limit")
	assert.Nil(t, find(doc, func(n *html.Node) bool { return attr(n, "id") == "tab-files" }), "no Files tab for NZBs")
	assert.NotNil(t, find(doc, func(n *html.Node) bool { return attr(n, "id") == "tab-details" }))
	copyBtn := find(byID(doc, "output-title").Parent.Parent, func(n *html.Node) bool { return hasAttr(n, "data-copy") })
	require.NotNil(t, copyBtn)
	assert.Equal(t, "/media/complete/Some.Movie.2024", attr(copyBtn, "data-copy"))
}

func TestDetailOfACleanFetchSaysSo(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})
	d.detail.reports[dl.ID] = web.FetchReport{}

	rec := d.b.get("/downloads/" + string(dl.ID))

	assert.Contains(t, text(parseHTML(t, rec.Body.String())), "Every article arrived intact")
}

func TestDetailOfAFailedDownloadShowsTheFailureCardForEveryReason(t *testing.T) {
	d := newDetailEnv(t)
	hints := map[core.FailureReason]string{
		core.FailureMissingArticles:    "grab another release",
		core.FailureRepairInsufficient: "Not enough recovery data to repair it. Sonarr and Radarr will usually grab another release.",
		core.FailureArchiveUnsupported: "Try another release",
		core.FailureArchivePassword:    "right archive password",
		core.FailureArchiveCorrupt:     "Try another release",
		core.FailureDiskFull:           "Free up space",
		core.FailurePathViolation:      "Do not use this release",
		core.FailureSourceInvalid:      "Get the NZB or torrent again",
		core.FailureInternal:           "server log",
	}
	for _, reason := range allFailureReasons {
		dl := d.add(core.Download{State: core.StateFailed, FailureReason: reason, FailureDetail: "detail for " + string(reason)})

		rec := d.b.get("/downloads/" + string(dl.ID))

		require.Equal(t, http.StatusOK, rec.Code, string(reason))
		doc := parseHTML(t, rec.Body.String())
		card := byID(doc, "failure-title")
		require.NotNil(t, card, string(reason))
		assert.Equal(t, "Failed: "+failureTitles[reason], text(card), string(reason))
		section := card
		for section != nil && attr(section, "role") != "alert" {
			section = section.Parent
		}
		require.NotNil(t, section)
		assert.Contains(t, text(section), hints[reason], string(reason))
		assert.Contains(t, text(section), "detail for "+string(reason))
		assert.Equal(t, "alert", attr(section, "role"))
		assert.Contains(t, text(doc), string(reason), "the raw code is in the Details tab")
	}
}

func TestDetailFailureDetailIsRedactedAndEscaped(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{State: core.StateFailed, FailureReason: core.FailureSourceInvalid,
		FailureDetail: `got <img src=x onerror=alert(1)> from https://tracker.example/announce?passkey=SECRETPASS999 ` + strings.Repeat("long ", 200)})

	rec := d.b.get("/downloads/" + string(dl.ID))

	body := rec.Body.String()
	assert.NotContains(t, body, "SECRETPASS999")
	assert.NotContains(t, body, "<img src=x")
	assert.Contains(t, body, "&lt;img")
	assert.Less(t, strings.Count(body, "long "), 100, "the detail is capped")
}

func TestDetailOfATorrentShowsFilesPeersRatioAndShareLimit(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{SourceKind: core.SourceTorrent, State: core.StateReady, Name: "Linux.ISO", TotalBytes: 6 << 30})
	two := 2.0
	d.live.set(dl.ID, view.Live{UpRate: 2 << 20, Torrent: &view.TorrentLive{
		Hash: "0123456789abcdef0123456789abcdef01234567", Registered: true, Private: true,
		Size: 6 << 30, DoneBytes: 6 << 30, Peers: 7, Seeds: 3, Uploaded: 12 << 30, SeedingTime: 49 * time.Hour,
		OwnRatio: &two,
	}})
	d.torrents.files[dl.ID] = []web.TorrentFile{{Path: "linux/disc1.iso", Size: 4 << 30}, {Path: "linux/<b>x</b>.txt", Size: 12}}

	rec := d.b.get("/downloads/" + string(dl.ID))

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	body := text(doc)
	for _, want := range []string{"7 peers", "3 seeds", "ratio 2.00", "Private", "2 d 1 h", "Own limit: ratio 2", "linux/disc1.iso", "4.0 GB", "0123456789abcdef0123456789abcdef01234567", "2 files"} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, rec.Body.String(), "<b>x</b>")
	assert.NotNil(t, byID(doc, "tab-files"))
	form := byID(doc, "share-card")
	require.NotNil(t, form)
	custom := byID(form, "share-mode-custom")
	assert.True(t, hasAttr(custom, "checked"))
	assert.Equal(t, "2", attr(byID(form, "share-ratio"), "value"))
	assert.Equal(t, "", attr(byID(form, "share-minutes"), "value"))
}

func TestDetailOfATorrentWithoutFactsStillRenders(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{SourceKind: core.SourceTorrent})

	rec := d.b.get("/downloads/" + string(dl.ID))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, text(parseHTML(t, rec.Body.String())), "No torrent details")
}

func TestDetailWithoutAnySourceStillRenders(t *testing.T) {
	d := newDetailEnv(t, withoutDetailSource(), withoutTorrentSource())
	nzb := d.add(core.Download{})
	tor := d.add(core.Download{SourceKind: core.SourceTorrent})

	for _, id := range []core.ID{nzb.ID, tor.ID} {
		assert.Equal(t, http.StatusOK, d.b.get("/downloads/"+string(id)).Code)
	}
}

func TestDetailPollsOnlyWhileTheDownloadIsNotTerminal(t *testing.T) {
	d := newDetailEnv(t)
	active := d.add(core.Download{State: core.StateFetching, TotalBytes: 1000, DoneBytes: 250})
	ready := d.add(core.Download{})
	failed := d.add(core.Download{State: core.StateFailed})

	for id, polls := range map[core.ID]bool{active.ID: true, ready.ID: false, failed.ID: false} {
		for _, target := range []string{"/downloads/" + string(id), "/downloads/" + string(id) + "/live"} {
			var opts []reqOpt
			if strings.HasSuffix(target, "/live") {
				opts = append(opts, htmx())
			}
			rec := d.b.get(target, opts...)

			require.Equal(t, http.StatusOK, rec.Code, target)
			live := byID(parseHTML(t, rec.Body.String()), "detail-live")
			require.NotNil(t, live, target)
			if polls {
				assert.Equal(t, "every 3s", attr(live, "hx-trigger"), target)
				assert.Equal(t, "/downloads/"+string(id)+"/live", attr(live, "hx-get"), target)
				assert.Equal(t, "outerHTML", attr(live, "hx-swap"), target)
			} else {
				assert.False(t, hasAttr(live, "hx-trigger"), target)
			}
		}
	}
}

func TestDetailLiveFragmentIsAFragmentWithProgress(t *testing.T) {
	d := newDetailEnv(t)
	active := d.add(core.Download{State: core.StateFetching, TotalBytes: 1000, DoneBytes: 250})

	rec := d.b.get("/downloads/"+string(active.ID)+"/live", htmx())

	assert.NotContains(t, rec.Body.String(), "<html")
	doc := parseHTML(t, rec.Body.String())
	progress := find(doc, func(n *html.Node) bool { return n.Data == "progress" })
	require.NotNil(t, progress)
	assert.Equal(t, "25", attr(progress, "value"))
}

func TestDetailLiveOfARemovedDownloadEndsThePolling(t *testing.T) {
	d := newDetailEnv(t)
	gone := core.NewID()

	rec := d.b.get("/downloads/"+string(gone)+"/live", htmx())

	assert.Equal(t, http.StatusOK, rec.Code)
	live := byID(parseHTML(t, rec.Body.String()), "detail-live")
	require.NotNil(t, live)
	assert.False(t, hasAttr(live, "hx-trigger"))
	assert.Contains(t, text(live), "was removed")
}

func TestDetailShowsThePostProcessingStage(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{State: core.StatePostProcessing, FetchedAt: epoch})
	d.live.set(dl.ID, view.Live{Stage: core.StageRepairing, HaveStage: true})

	rec := d.b.get("/downloads/" + string(dl.ID))

	assert.Contains(t, text(parseHTML(t, rec.Body.String())), "Processing: Repairing")
}

func TestDetailUnknownOrMalformedIDsAre404(t *testing.T) {
	d := newDetailEnv(t)

	for _, id := range []string{string(core.NewID()), "nope", "UPPERCASE", strings.Repeat("a", 27), "%27%20OR%201%3D1"} {
		rec := d.b.get("/downloads/" + id)

		assert.Equal(t, http.StatusNotFound, rec.Code, id)
		assert.Contains(t, rec.Body.String(), "Download not found", id)
	}
	rec := d.b.get("/downloads/"+string(core.NewID()), htmx())
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "toast")
}

func TestDetailEscapesHostileNames(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{Name: `<script>alert(1)</script>"><svg onload=alert(2)>`, OutputPath: `/p"><b>x</b>`})

	rec := d.b.get("/downloads/" + string(dl.ID))

	body := rec.Body.String()
	assert.NotContains(t, body, "<script>alert")
	assert.NotContains(t, body, "<svg onload")
	assert.NotContains(t, body, "<b>x</b>")
}

func TestDetailRequiresASession(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})

	rec := d.env.browser().get("/downloads/" + string(dl.ID))

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/login?next="+url.QueryEscape("/downloads/"+string(dl.ID)), location(rec))
}

// ---- remove ---------------------------------------------------------------

func TestRemoveKeepsTheFilesByDefault(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})

	rec := d.act("/downloads/"+string(dl.ID)+"/delete", url.Values{"delete_data": {"0"}, "from": {"history"}})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String(), "an empty body removes the row")
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "Removed from the history")
	_, err := d.get(dl.ID)
	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.Empty(t, d.remover.ids())
}

func TestRemoveWithDeleteDataAsksTheDataRemover(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})

	rec := d.act("/downloads/"+string(dl.ID)+"/delete", url.Values{"delete_data": {"1"}, "from": {"history"}})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []core.ID{dl.ID}, d.remover.ids())
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "files were deleted")
}

func TestRemoveFromTheDetailPageRedirectsToTheHistory(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})

	rec := d.act("/downloads/"+string(dl.ID)+"/delete", url.Values{"from": {"detail"}})

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "/history", rec.Header().Get("HX-Redirect"))
}

func TestRemoveWorksAsAPlainFormPost(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})

	rec := d.b.post("/downloads/"+string(dl.ID)+"/delete", url.Values{"_csrf": {d.tok}, "delete_data": {"0"}})

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/history", location(rec))
	_, err := d.get(dl.ID)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestRemoveFailureKeepsTheDownloadAndSaysSo(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})
	d.remover.err = assertErr("disk on fire")

	rec := d.act("/downloads/"+string(dl.ID)+"/delete", url.Values{"delete_data": {"1"}})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "Could not remove it")
	assert.NotContains(t, rec.Header().Get("HX-Trigger"), "disk on fire")
	_, err := d.get(dl.ID)
	assert.NoError(t, err)
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func TestRemoveUnknownAndMalformedIDs(t *testing.T) {
	d := newDetailEnv(t)

	for _, id := range []string{string(core.NewID()), "bad-id"} {
		rec := d.act("/downloads/"+id+"/delete", nil)

		assert.Equal(t, http.StatusNotFound, rec.Code, id)
	}
}

func TestRemoveNeedsTheCSRFToken(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})

	rec := d.b.post("/downloads/"+string(dl.ID)+"/delete", url.Values{}, htmx())

	assert.Equal(t, http.StatusForbidden, rec.Code)
	_, err := d.get(dl.ID)
	assert.NoError(t, err)
	rec = d.b.post("/downloads/"+string(dl.ID)+"/delete", url.Values{}, htmx(), header("X-CSRF-Token", d.tok), header("Origin", "https://evil.test"))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestRemoveRequiresASession(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})

	rec := d.env.browser().post("/downloads/"+string(dl.ID)+"/delete", url.Values{})

	assert.NotEqual(t, http.StatusOK, rec.Code)
	_, err := d.get(dl.ID)
	assert.NoError(t, err)
}

// ---- share limit ------------------------------------------------------------

func TestShareLimitCustomValuesAreSet(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{SourceKind: core.SourceTorrent})
	d.live.set(dl.ID, view.Live{Torrent: &view.TorrentLive{Hash: "ab"}})

	rec := d.act("/downloads/"+string(dl.ID)+"/share-limit", url.Values{"mode": {"custom"}, "ratio": {"1.5"}, "minutes": {"1440"}})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []web.ShareLimitChange{{Ratio: 1.5, Minutes: 1440}}, d.torrents.recorded())
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "Share limit saved")
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "share-card"))
}

func TestShareLimitBlankFieldsMeanNoLimit(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{SourceKind: core.SourceTorrent})
	d.live.set(dl.ID, view.Live{Torrent: &view.TorrentLive{}})

	d.act("/downloads/"+string(dl.ID)+"/share-limit", url.Values{"mode": {"custom"}, "ratio": {""}, "minutes": {"30"}})
	d.act("/downloads/"+string(dl.ID)+"/share-limit", url.Values{"mode": {"custom"}, "ratio": {"3"}, "minutes": {" "}})
	d.act("/downloads/"+string(dl.ID)+"/share-limit", url.Values{"mode": {"default"}, "ratio": {"9"}})

	assert.Equal(t, []web.ShareLimitChange{{Ratio: -1, Minutes: 30}, {Ratio: 3, Minutes: -1}, {UseDefault: true}}, d.torrents.recorded())
}

func TestShareLimitInvalidValuesAreRejectedWithFieldErrors(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{SourceKind: core.SourceTorrent})
	d.live.set(dl.ID, view.Live{Torrent: &view.TorrentLive{}})

	for _, tc := range []struct{ ratio, minutes, field string }{
		{"abc", "", "share-ratio-error"}, {"-1", "", "share-ratio-error"}, {"NaN", "", "share-ratio-error"}, {"Inf", "", "share-ratio-error"},
		{"1e99", "", "share-ratio-error"},
		{"", "1.5", "share-minutes-error"}, {"", "-5", "share-minutes-error"}, {"", "99999999999", "share-minutes-error"},
	} {
		rec := d.act("/downloads/"+string(dl.ID)+"/share-limit", url.Values{"mode": {"custom"}, "ratio": {tc.ratio}, "minutes": {tc.minutes}})

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, tc.ratio+"/"+tc.minutes)
		doc := parseHTML(t, rec.Body.String())
		assert.NotNil(t, byID(doc, tc.field), tc.ratio+"/"+tc.minutes)
		input := byID(doc, strings.TrimSuffix(tc.field, "-error"))
		assert.Equal(t, "true", attr(input, "aria-invalid"))
		assert.Equal(t, tc.field, attr(input, "aria-describedby"))
	}
	assert.Empty(t, d.torrents.recorded())
}

func TestShareLimitOnAnNZBIsRefused(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{})

	rec := d.act("/downloads/"+string(dl.ID)+"/share-limit", url.Values{"mode": {"default"}})

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Empty(t, d.torrents.recorded())
}

func TestShareLimitPlainPostRedirectsBackToTheDetailPage(t *testing.T) {
	d := newDetailEnv(t)
	dl := d.add(core.Download{SourceKind: core.SourceTorrent})
	d.live.set(dl.ID, view.Live{Torrent: &view.TorrentLive{}})

	rec := d.b.post("/downloads/"+string(dl.ID)+"/share-limit", url.Values{"_csrf": {d.tok}, "mode": {"default"}})

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/downloads/"+string(dl.ID), location(rec))
}

func TestShareLimitSummaryNamesTheGlobalDefault(t *testing.T) {
	d := newDetailEnv(t)
	s := d.settings.Current()
	s.Seeding.RatioLimit = 2
	s.Seeding.SeedingTimeMinutes = 120
	require.NoError(t, d.settings.Update(t.Context(), s))
	dl := d.add(core.Download{SourceKind: core.SourceTorrent})
	d.live.set(dl.ID, view.Live{Torrent: &view.TorrentLive{}})

	rec := d.b.get("/downloads/" + string(dl.ID))

	assert.Contains(t, text(parseHTML(t, rec.Body.String())), "Global default: ratio 2 or seed for 2 h 0 min")
}
