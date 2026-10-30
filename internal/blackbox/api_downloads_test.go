package blackbox_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// mix is a set of Downloads in states that nothing in the daemon moves on
// its own: added, a queued one that is paused (so no Queue starts it), ready
// and failed ones.
type mix struct {
	tv, movies core.Category
	alpha      core.Download // nzb, added, tv, normal, position 0
	bravo      core.Download // nzb, added, high, position 1
	charlie    core.Download // nzb, queued and paused, movies, position 2
	delta      core.Download // torrent, added
	echo       core.Download // nzb, ready, tv
	foxtrot    core.Download // torrent, failed
}

func (m mix) all() []core.Download {
	return []core.Download{m.alpha, m.bravo, m.charlie, m.delta, m.echo, m.foxtrot}
}

// seedMix stores the mix in c. The Downloads are added one minute apart in
// the order alpha .. foxtrot.
func seedMix(t *testing.T, c configDir) mix {
	t.Helper()
	var m mix
	m.tv = addCategory(t, c, "series")
	m.movies = addCategory(t, c, "films")
	m.alpha = newDownload("alpha", at(0), position(0), inCategory(m.tv.ID))
	m.bravo = newDownload("bravo", at(1), position(1), withPriority(core.DownloadPriorityHigh))
	m.charlie = newDownload("charlie", at(2), position(2), inState(core.StateQueued), paused(), inCategory(m.movies.ID),
		func(d *core.Download) { d.TotalBytes = 1000; d.DoneBytes = 250 })
	m.delta = newDownload("delta", at(3), torrent())
	m.echo = newDownload("echo", at(4), inState(core.StateReady), inCategory(m.tv.ID), func(d *core.Download) {
		d.TotalBytes, d.DoneBytes = 5000, 5000
		d.FetchedAt = fixtureBase.Add(5 * time.Minute)
		d.ReadyAt = fixtureBase.Add(6*time.Minute + 123*time.Millisecond)
		d.OutputPath = "/data/usenet/complete/series/echo"
	})
	m.foxtrot = newDownload("foxtrot", at(5), torrent(), inState(core.StateFailed), func(d *core.Download) {
		d.FailureReason = core.FailureSourceInvalid
		d.FailureDetail = "not a torrent"
	})
	seedDownloads(t, c, m.all()...)
	return m
}

func dlPath(d core.Download) string { return "/api/v1/downloads/" + string(d.ID) }

func TestAPIListDownloadsOfNothingIsAnEmptyArrayAndANullCursor(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	r := d.apiGet("/api/v1/downloads")

	requireOK(t, r)
	assert.Equal(t, `{"items":[],"next":null}`+"\n", r.body)
}

func TestAPIListDownloadsAnswersEveryFieldInAddedOrder(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	r := d.apiGet("/api/v1/downloads")

	requireOK(t, r)
	items := itemsOf(t, r)
	assert.Equal(t, []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}, namesOfItems(items))
	assert.Nil(t, obj(t, r)["next"])
	// Field by field, with the exact JSON of each state shape.
	assert.JSONEq(t, `{"id":"`+string(m.alpha.ID)+`","name":"alpha","source_kind":"nzb","category_id":"`+string(m.tv.ID)+`","state":"added",`+
		`"priority":"normal","queue_position":0,"paused":false,"total_bytes":0,"done_bytes":0,`+
		`"added_at":"2026-03-01T12:00:00Z","fetched_at":null,"ready_at":null,"updated_at":"2026-03-01T12:00:00Z"}`, jsonOf(t, items[0]))
	assert.JSONEq(t, `{"id":"`+string(m.charlie.ID)+`","name":"charlie","source_kind":"nzb","category_id":"`+string(m.movies.ID)+`","state":"queued",`+
		`"priority":"normal","queue_position":2,"paused":true,"total_bytes":1000,"done_bytes":250,`+
		`"added_at":"2026-03-01T12:02:00Z","fetched_at":null,"ready_at":null,"updated_at":"2026-03-01T12:02:00Z"}`, jsonOf(t, items[2]))
	assert.JSONEq(t, `{"id":"`+string(m.echo.ID)+`","name":"echo","source_kind":"nzb","category_id":"`+string(m.tv.ID)+`","state":"ready",`+
		`"priority":"normal","queue_position":0,"paused":false,"total_bytes":5000,"done_bytes":5000,`+
		`"added_at":"2026-03-01T12:04:00Z","fetched_at":"2026-03-01T12:05:00Z","ready_at":"2026-03-01T12:06:00.123Z","updated_at":"2026-03-01T12:04:00Z",`+
		`"output_path":"/data/usenet/complete/series/echo"}`, jsonOf(t, items[4]))
	assert.JSONEq(t, `{"id":"`+string(m.foxtrot.ID)+`","name":"foxtrot","source_kind":"torrent","category_id":null,"state":"failed",`+
		`"priority":"normal","queue_position":0,"paused":false,"failure_reason":"source_invalid","failure_detail":"not a torrent",`+
		`"total_bytes":0,"done_bytes":0,"added_at":"2026-03-01T12:05:00Z","fetched_at":null,"ready_at":null,"updated_at":"2026-03-01T12:05:00Z"}`, jsonOf(t, items[5]))
	assert.NotContains(t, items[0], "failure_reason", "empty failure fields and output_path are left out")
	assert.NotContains(t, items[0], "output_path")
}

func TestAPIListDownloadsWritesTimestampsAsRFC3339WithoutTrailingZeros(t *testing.T) {
	t.Parallel()
	dl := newDownload("t", func(d *core.Download) {
		d.AddedAt = time.Date(2026, 5, 6, 7, 8, 9, 120_000_000, time.UTC)
		d.UpdatedAt = time.Date(2026, 5, 6, 7, 8, 9, 5_000_000, time.UTC)
	})
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, dl) })

	r := d.apiGet(dlPath(dl))

	requireOK(t, r)
	assert.Contains(t, r.body, `"added_at":"2026-05-06T07:08:09.12Z"`)
	assert.Contains(t, r.body, `"updated_at":"2026-05-06T07:08:09.005Z"`)
}

func TestAPIListDownloadsFiltersByStateCategoryAndKind(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })
	get := func(query string) []string {
		return namesOfItems(itemsOf(t, d.apiGet("/api/v1/downloads?"+query)))
	}

	assert.Equal(t, []string{"alpha", "bravo", "delta"}, get("state=added"))
	assert.Equal(t, []string{"charlie"}, get("state=queued"))
	assert.Equal(t, []string{"echo"}, get("state=ready"))
	assert.Equal(t, []string{"foxtrot"}, get("state=failed"))
	assert.Empty(t, get("state=fetching"), "a valid state without Downloads is an empty list")
	assert.Empty(t, get("state=post_processing"))
	assert.Equal(t, []string{"alpha", "echo"}, get("category_id="+string(m.tv.ID)))
	assert.Equal(t, []string{"charlie"}, get("category_id="+string(m.movies.ID)))
	assert.Empty(t, get("category_id=nosuchcategory"), "an unknown category is not an error")
	assert.Equal(t, []string{"alpha", "bravo", "charlie", "echo"}, get("kind=nzb"))
	assert.Equal(t, []string{"delta", "foxtrot"}, get("kind=torrent"))
	assert.Equal(t, []string{"alpha"}, get("state=added&kind=nzb&category_id="+string(m.tv.ID)))
	assert.Equal(t, []string{"foxtrot"}, get("kind=torrent&state=failed"))
	assert.Empty(t, get("kind=torrent&state=ready"))
	assert.Equal(t, namesOfItems(itemsOf(t, d.apiGet("/api/v1/downloads"))), get("state=&kind=&category_id=&after=&limit="),
		"empty parameters filter nothing")
}

func TestAPIListDownloadsRejectsInvalidParameters(t *testing.T) {
	t.Parallel()
	d := startAPI(t, func(c configDir) { seedMix(t, c) })

	for name, tc := range map[string]struct{ query, code string }{
		"unknown state":        {"state=bogus", "invalid_state"},
		"state in upper case":  {"state=READY", "invalid_state"},
		"unknown kind":         {"kind=usb", "invalid_kind"},
		"limit zero":           {"limit=0", "invalid_limit"},
		"limit negative":       {"limit=-3", "invalid_limit"},
		"limit not a number":   {"limit=many", "invalid_limit"},
		"limit fractional":     {"limit=1.5", "invalid_limit"},
		"limit with a plus":    {"limit=%2B5x", "invalid_limit"},
		"unknown parameter":    {"stat=ready", "unknown_parameter"},
		"unknown next to ok":   {"state=ready&page=2", "unknown_parameter"},
		"cursor not base64":    {"after=%21%21%21", "invalid_cursor"},
		"cursor without colon": {"after=" + b64("nocolon"), "invalid_cursor"},
		"cursor without id":    {"after=" + b64("12345:"), "invalid_cursor"},
		"cursor not a number":  {"after=" + b64("abc:def"), "invalid_cursor"},
	} {
		t.Run(name, func(t *testing.T) {
			r := d.apiGet("/api/v1/downloads?" + tc.query)
			requireError(t, r, http.StatusBadRequest, tc.code)
		})
	}
}

func TestAPIListDownloadsErrorMessagesWrittenByTheApp(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	requireErrorMessage(t, d.apiGet("/api/v1/downloads?state=bogus"), 400, "invalid_state", `unknown state "bogus"`)
	requireErrorMessage(t, d.apiGet("/api/v1/downloads?kind=usb"), 400, "invalid_kind", `unknown kind "usb": use nzb or torrent`)
	requireErrorMessage(t, d.apiGet("/api/v1/downloads?limit=0"), 400, "invalid_limit", "limit must be a positive integer")
	requireErrorMessage(t, d.apiGet("/api/v1/downloads?stat=ready"), 400, "unknown_parameter", `unknown query parameter "stat"`)
	requireErrorMessage(t, d.apiGet("/api/v1/downloads?after=%21"), 400, "invalid_cursor", "the cursor is not valid")
}

func TestAPIListDownloadsChecksTheLimitBeforeTheStateAndTheStateBeforeTheKind(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	requireError(t, d.apiGet("/api/v1/downloads?state=bogus&kind=bogus"), 400, "invalid_state")
	requireError(t, d.apiGet("/api/v1/downloads?kind=bogus&limit=0"), 400, "invalid_limit")
	requireError(t, d.apiGet("/api/v1/downloads?state=bogus&bogus=1"), 400, "unknown_parameter")
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestAPIListDownloadsPagesWithAnOpaqueCursorThatEndsInNull(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	var seen []string
	query := "limit=4"
	pages := 0
	for {
		r := d.apiGet("/api/v1/downloads?" + query)
		requireOK(t, r)
		body := obj(t, r)
		seen = append(seen, namesOfItems(itemsOf(t, r))...)
		pages++
		next, _ := body["next"].(string)
		if next == "" {
			assert.Nil(t, body["next"], "the last page has a null cursor")
			break
		}
		assert.Len(t, itemsOf(t, r), 4, "every page but the last is full")
		query = "limit=4&after=" + url.QueryEscape(next)
	}

	assert.Equal(t, 2, pages)
	assert.Equal(t, []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}, seen)
	_ = m
}

func TestAPIListDownloadsPageOfExactlyTheRemainingRowsHasNoCursor(t *testing.T) {
	t.Parallel()
	d := startAPI(t, func(c configDir) { seedMix(t, c) })

	exact := obj(t, d.apiGet("/api/v1/downloads?limit=6"))
	more := obj(t, d.apiGet("/api/v1/downloads?limit=5"))

	assert.Nil(t, exact["next"], "six rows, limit six: nothing follows")
	assert.NotNil(t, more["next"])
}

func TestAPIListDownloadsCursorIsKeysetBasedAndSurvivesInsertionsBeforeIt(t *testing.T) {
	t.Parallel()
	var m mix
	cfg := prepareConfigDir(t)
	m = seedMix(t, cfg)
	d := startDaemon(t, cfg)
	first := obj(t, d.apiGet("/api/v1/downloads?limit=2"))
	next := first["next"].(string)
	// A Download older than everything is added after the first page was read.
	seedDownloads(t, cfg, newDownload("oldest", at(-60)))

	r := d.apiGet("/api/v1/downloads?limit=10&after=" + url.QueryEscape(next))

	assert.Equal(t, []string{"charlie", "delta", "echo", "foxtrot"}, namesOfItems(itemsOf(t, r)),
		"nothing is repeated or skipped, and the new row sits before the cursor")
	_ = m
}

func TestAPIListDownloadsCursorFormatIsTheStoredOne(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	next := obj(t, d.apiGet("/api/v1/downloads?limit=2"))["next"].(string)

	// The cursor is the unpadded URL-safe base64 of "<added_at ms>:<id>" of
	// the last row of the page, so a cursor survives a switch of binaries.
	raw, err := base64.RawURLEncoding.DecodeString(next)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(m.bravo.AddedAt.UnixMilli(), 10)+":"+string(m.bravo.ID), string(raw))
	hand := b64(strconv.FormatInt(m.charlie.AddedAt.UnixMilli(), 10) + ":" + string(m.charlie.ID))
	assert.Equal(t, []string{"delta", "echo", "foxtrot"}, namesOfItems(itemsOf(t, d.apiGet("/api/v1/downloads?after="+hand))))
}

func TestAPIListDownloadsOrdersEqualAddedTimesByID(t *testing.T) {
	t.Parallel()
	ids := []core.ID{"id-c", "id-a", "id-b"}
	d := startAPI(t, func(c configDir) {
		for _, id := range ids {
			seedDownloads(t, c, newDownload("same-"+string(id), func(d *core.Download) { d.ID = id }))
		}
	})

	items := itemsOf(t, d.apiGet("/api/v1/downloads"))

	assert.Equal(t, []string{"id-a", "id-b", "id-c"}, idsOfItems(items))
}

func TestAPIListDownloadsDefaultPageIs50AndTheLargestIs200(t *testing.T) {
	t.Parallel()
	d := startAPI(t, func(c configDir) {
		ds := make([]core.Download, 0, 205)
		for i := range 205 {
			ds = append(ds, newDownload(fmt.Sprintf("n%03d", i), at(i), inState(core.StateReady)))
		}
		seedDownloads(t, c, ds...)
	})

	def := d.apiGet("/api/v1/downloads")
	big := d.apiGet("/api/v1/downloads?limit=100000")
	small := d.apiGet("/api/v1/downloads?limit=1")

	assert.Len(t, itemsOf(t, def), 50)
	assert.NotNil(t, obj(t, def)["next"])
	assert.Len(t, itemsOf(t, big), 200)
	assert.NotNil(t, obj(t, big)["next"])
	assert.Len(t, itemsOf(t, small), 1)
}

func TestAPIGetDownloadAnswersTheStoredDownload(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	r := d.apiGet(dlPath(m.bravo))

	requireOK(t, r)
	assert.Equal(t, wantDownloadJSON(string(m.bravo.ID), "bravo", "nzb", "added", "high", 1, false, "2026-03-01T12:01:00Z")+"\n", r.body)
	failed := d.apiGet(dlPath(m.foxtrot))
	requireOK(t, failed)
	assert.Equal(t, "source_invalid", obj(t, failed)["failure_reason"])
}

func TestAPIGetDownloadOfAnUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	d := startAPI(t, func(c configDir) { seedMix(t, c) })

	for _, id := range []string{"nope", "0", "a%2Fb", "%20", "x.y", "a%00b", strings.Repeat("z", 300), "ID", "%E2%9C%93"} {
		t.Run(id, func(t *testing.T) {
			r := d.apiGet("/api/v1/downloads/" + id)
			assert.Equal(t, http.StatusNotFound, r.status, r.body)
			assert.Equal(t, errBody("not_found", "not found"), r.body)
			requireAPIHeaders(t, r)
		})
	}
}

func TestAPIGetDownloadIgnoresQueryParameters(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	r := d.apiGet(dlPath(m.alpha) + "?whatever=1")

	requireOK(t, r)
}

func TestAPIPauseAndResumeToggleTheFlagAndPersistIt(t *testing.T) {
	t.Parallel()
	var m mix
	cfg := prepareConfigDir(t)
	m = seedMix(t, cfg)
	d := startDaemon(t, cfg)
	before := time.Now().Add(-time.Second)

	p := d.api(http.MethodPost, dlPath(m.alpha)+"/pause", "")
	requireOK(t, p)
	pm := obj(t, p)
	assert.Equal(t, true, pm["paused"])
	assert.Equal(t, "added", pm["state"], "pausing is a flag, not a state")
	assert.True(t, parseTime(t, pm["updated_at"]).After(before), "updated_at is refreshed")
	assert.Equal(t, "2026-03-01T12:00:00Z", pm["added_at"])
	assert.Equal(t, true, obj(t, d.apiGet(dlPath(m.alpha)))["paused"])
	stored, err := storedDownload(t, cfg, m.alpha.ID)
	require.NoError(t, err)
	assert.True(t, stored.Paused, "the flag is in the database at once")

	r := d.api(http.MethodPost, dlPath(m.alpha)+"/resume", "")
	requireOK(t, r)
	assert.Equal(t, false, obj(t, r)["paused"])
	d.stop()
	stored, err = storedDownload(t, cfg, m.alpha.ID)
	require.NoError(t, err)
	assert.False(t, stored.Paused)
	assert.Equal(t, core.StateAdded, stored.State)
}

func TestAPIPauseAndResumeThatChangeNothingKeepTheDownloadAsItWas(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	resumed := d.api(http.MethodPost, dlPath(m.alpha)+"/resume", "")
	again := d.api(http.MethodPost, dlPath(m.charlie)+"/pause", "")
	readyResume := d.api(http.MethodPost, dlPath(m.echo)+"/resume", "")

	requireOK(t, resumed)
	assert.Equal(t, "2026-03-01T12:00:00Z", obj(t, resumed)["updated_at"], "no change, no new updated_at")
	requireOK(t, again)
	assert.Equal(t, true, obj(t, again)["paused"])
	assert.Equal(t, "2026-03-01T12:02:00Z", obj(t, again)["updated_at"])
	requireOK(t, readyResume)
	assert.Equal(t, "ready", obj(t, readyResume)["state"], "a Download that is not paused resumes as a no-op in any state")
}

func TestAPIPauseRefusesDownloadsThatAreNotWaitingOrReceiving(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	ready := d.api(http.MethodPost, dlPath(m.echo)+"/pause", "")
	failed := d.api(http.MethodPost, dlPath(m.foxtrot)+"/pause", "")

	requireErrorMessage(t, ready, http.StatusConflict, "not_pausable", `a download in state "ready" cannot be paused`)
	requireErrorMessage(t, failed, http.StatusConflict, "not_pausable", `a download in state "failed" cannot be paused`)
	assert.Equal(t, false, obj(t, d.apiGet(dlPath(m.echo)))["paused"])
}

func TestAPIPauseRefusesPostProcessingAndFetchedDownloads(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	// Inserted while the daemon runs, because it would resume or claim them
	// at start-up. Post-processing starts Fetched Downloads on its next
	// poll, so the test is done long before.
	post := newDownload("post", inState(core.StatePostProcessing))
	seedDownloads(t, cfg, post)

	r := d.api(http.MethodPost, dlPath(post)+"/pause", "")

	requireErrorMessage(t, r, http.StatusConflict, "not_pausable", `a download in state "post_processing" cannot be paused`)
}

func TestAPIPauseAReadyTorrentIsAllowedBecauseItStopsSeeding(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	seeding := newDownload("seeding", torrent(), inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = "/data/torrents/seeding"
	})
	seedDownloads(t, cfg, seeding)

	p := d.api(http.MethodPost, dlPath(seeding)+"/pause", "")

	requireOK(t, p)
	assert.Equal(t, true, obj(t, p)["paused"])
	assert.Equal(t, "ready", obj(t, p)["state"])
	r := d.api(http.MethodPost, dlPath(seeding)+"/resume", "")
	requireOK(t, r)
	assert.Equal(t, false, obj(t, r)["paused"])
}

func TestAPIPauseAndResumeOfAnUnknownDownloadAreNotFound(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	for _, action := range []string{"pause", "resume"} {
		r := d.api(http.MethodPost, "/api/v1/downloads/nope/"+action, "")
		assert.Equal(t, http.StatusNotFound, r.status)
		assert.Equal(t, errBody("not_found", "not found"), r.body)
	}
}

func TestAPIPatchDownloadChangesThePriorityAndPersistsIt(t *testing.T) {
	t.Parallel()
	var m mix
	cfg := prepareConfigDir(t)
	m = seedMix(t, cfg)
	d := startDaemon(t, cfg)
	before := time.Now().Add(-time.Second)

	r := d.api(http.MethodPatch, dlPath(m.alpha), `{"priority":"force"}`)

	requireOK(t, r)
	got := obj(t, r)
	assert.Equal(t, "force", got["priority"])
	assert.Equal(t, string(m.alpha.ID), got["id"])
	assert.Equal(t, "alpha", got["name"])
	assert.True(t, parseTime(t, got["updated_at"]).After(before))
	d.stop()
	stored, err := storedDownload(t, cfg, m.alpha.ID)
	require.NoError(t, err)
	assert.Equal(t, core.DownloadPriorityForce, stored.Priority)
	assert.Equal(t, m.tv.ID, stored.CategoryID, "other fields are untouched")
	assert.Equal(t, 0, stored.QueuePosition)
}

func TestAPIPatchDownloadAcceptsEveryPriorityAndRefusesOthers(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	for _, p := range []string{"low", "normal", "high", "force"} {
		r := d.api(http.MethodPatch, dlPath(m.bravo), `{"priority":"`+p+`"}`)
		requireOK(t, r)
		assert.Equal(t, p, obj(t, r)["priority"])
	}
	for _, p := range []string{`"urgent"`, `"HIGH"`, `""`, `" high"`} {
		r := d.api(http.MethodPatch, dlPath(m.bravo), `{"priority":`+p+`}`)
		requireError(t, r, http.StatusBadRequest, "invalid_priority")
	}
	requireErrorMessage(t, d.api(http.MethodPatch, dlPath(m.bravo), `{"priority":"urgent"}`),
		400, "invalid_priority", `unknown priority "urgent": use force, high, normal or low`)
	assert.Equal(t, "force", obj(t, d.apiGet(dlPath(m.bravo)))["priority"])
}

func TestAPIPatchDownloadThatChangesNothingKeepsUpdatedAt(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	r := d.api(http.MethodPatch, dlPath(m.alpha), `{"priority":"normal","category_id":"`+string(m.tv.ID)+`"}`)

	requireOK(t, r)
	assert.Equal(t, "2026-03-01T12:00:00Z", obj(t, r)["updated_at"])
}

func TestAPIPatchDownloadSetsAndClearsTheCategory(t *testing.T) {
	t.Parallel()
	var m mix
	cfg := prepareConfigDir(t)
	m = seedMix(t, cfg)
	d := startDaemon(t, cfg)

	set := d.api(http.MethodPatch, dlPath(m.bravo), `{"category_id":"`+string(m.movies.ID)+`"}`)
	nulled := d.api(http.MethodPatch, dlPath(m.alpha), `{"category_id":null}`)
	emptied := d.api(http.MethodPatch, dlPath(m.echo), `{"category_id":""}`)

	requireOK(t, set)
	assert.Equal(t, string(m.movies.ID), obj(t, set)["category_id"])
	requireOK(t, nulled)
	assert.Nil(t, obj(t, nulled)["category_id"], "null removes the category")
	requireOK(t, emptied)
	assert.Nil(t, obj(t, emptied)["category_id"], "so does an empty string")
	d.stop()
	bravo, _ := storedDownload(t, cfg, m.bravo.ID)
	alpha, _ := storedDownload(t, cfg, m.alpha.ID)
	assert.Equal(t, m.movies.ID, bravo.CategoryID)
	assert.Empty(t, alpha.CategoryID)
}

func TestAPIPatchDownloadRefusesAnUnknownCategoryAndChangesNothing(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	r := d.api(http.MethodPatch, dlPath(m.alpha), `{"priority":"low","category_id":"missing"}`)

	requireErrorMessage(t, r, http.StatusBadRequest, "invalid_category", `category "missing" does not exist`)
	got := obj(t, d.apiGet(dlPath(m.alpha)))
	assert.Equal(t, "normal", got["priority"], "the whole patch is refused")
	assert.Equal(t, string(m.tv.ID), got["category_id"])
}

func TestAPIPatchDownloadValidatesTheWholePatchBeforeApplyingAnyOfIt(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	r := d.api(http.MethodPatch, dlPath(m.bravo), `{"category_id":"`+string(m.tv.ID)+`","queue_position":{"move":"sideways"}}`)

	requireErrorMessage(t, r, http.StatusBadRequest, "invalid_move", `unknown move "sideways": use top, bottom, up or down`)
	assert.Nil(t, obj(t, d.apiGet(dlPath(m.bravo)))["category_id"])
}

func TestAPIPatchDownloadMovesWithinTheQueueOfItsKind(t *testing.T) {
	t.Parallel()
	var m mix
	cfg := prepareConfigDir(t)
	m = seedMix(t, cfg)
	d := startDaemon(t, cfg)
	order := func() []string {
		var q []map[string]any
		for _, it := range itemsOf(t, d.apiGet("/api/v1/downloads?kind=nzb")) {
			if st := it["state"]; st == "added" || st == "queued" {
				q = append(q, it)
			}
		}
		sort.Slice(q, func(i, j int) bool { return q[i]["queue_position"].(float64) < q[j]["queue_position"].(float64) })
		var out []string
		for _, it := range q {
			out = append(out, it["name"].(string)+":"+strconv.Itoa(int(it["queue_position"].(float64))))
		}
		return out
	}
	move := func(dl core.Download, mv string) map[string]any {
		r := d.api(http.MethodPatch, dlPath(dl), `{"queue_position":{"move":"`+mv+`"}}`)
		requireOK(t, r)
		return obj(t, r)
	}
	// The queue is alpha 0, bravo 1, charlie 2 (queued and paused still counts).
	require.Equal(t, []string{"alpha:0", "bravo:1", "charlie:2"}, order())

	got := move(m.charlie, "top")
	assert.EqualValues(t, 0, got["queue_position"])
	assert.Equal(t, []string{"charlie:0", "alpha:1", "bravo:2"}, order())

	got = move(m.charlie, "down")
	assert.EqualValues(t, 1, got["queue_position"])
	assert.Equal(t, []string{"alpha:0", "charlie:1", "bravo:2"}, order())

	got = move(m.alpha, "bottom")
	assert.EqualValues(t, 2, got["queue_position"])
	assert.Equal(t, []string{"charlie:0", "bravo:1", "alpha:2"}, order())

	got = move(m.alpha, "up")
	assert.EqualValues(t, 1, got["queue_position"])
	assert.Equal(t, []string{"charlie:0", "alpha:1", "bravo:2"}, order())

	got = move(m.bravo, "up")
	assert.EqualValues(t, 1, got["queue_position"])
	d.stop()
	var positions []string
	for _, s := range storedDownloads(t, cfg) {
		if s.SourceKind == core.SourceNZB && s.State.InQueue() || s.State == core.StateQueued {
			positions = append(positions, s.Name+":"+strconv.Itoa(s.QueuePosition))
		}
	}
	assert.ElementsMatch(t, []string{"alpha:2", "bravo:1", "charlie:0"}, positions, "the new order is in the database")
	delta, _ := storedDownload(t, cfg, m.delta.ID)
	assert.Equal(t, 0, delta.QueuePosition, "the torrent queue is separate")
}

func TestAPIPatchDownloadMovePastTheEdgesStaysAndRenumbersGaps(t *testing.T) {
	t.Parallel()
	x := newDownload("x", at(0), position(10))
	y := newDownload("y", at(1), position(10))
	z := newDownload("z", at(2), position(20))
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, x, y, z) })

	top := d.api(http.MethodPatch, dlPath(x), `{"queue_position":{"move":"up"}}`)
	requireOK(t, top)
	assert.EqualValues(t, 0, obj(t, top)["queue_position"], "already first: positions become dense")

	moved := d.api(http.MethodPatch, dlPath(z), `{"queue_position":{"move":"up"}}`)
	requireOK(t, moved)
	assert.EqualValues(t, 1, obj(t, moved)["queue_position"])
	got := map[string]float64{}
	for _, it := range itemsOf(t, d.apiGet("/api/v1/downloads")) {
		got[it["name"].(string)] = it["queue_position"].(float64)
	}
	assert.Equal(t, map[string]float64{"x": 0, "z": 1, "y": 2}, got)
	bottom := d.api(http.MethodPatch, dlPath(y), `{"queue_position":{"move":"down"}}`)
	assert.EqualValues(t, 2, obj(t, bottom)["queue_position"], "already last")
}

func TestAPIPatchDownloadMoveOfADownloadOutsideTheQueueIsAConflict(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	r := d.api(http.MethodPatch, dlPath(m.echo), `{"queue_position":{"move":"top"}}`)

	requireErrorMessage(t, r, http.StatusConflict, "not_queued", `a download in state "ready" is not in a queue`)
	failed := d.api(http.MethodPatch, dlPath(m.foxtrot), `{"queue_position":{"move":"top"}}`)
	requireError(t, failed, http.StatusConflict, "not_queued")
}

func TestAPIPatchDownloadCombinesPriorityCategoryAndMoveInOneEvent(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })
	s := d.openStreamAfterHello()

	r := d.api(http.MethodPatch, dlPath(m.bravo), `{"priority":"low","category_id":"`+string(m.movies.ID)+`","queue_position":{"move":"top"}}`)

	requireOK(t, r)
	got := obj(t, r)
	assert.Equal(t, "low", got["priority"])
	assert.Equal(t, string(m.movies.ID), got["category_id"])
	assert.EqualValues(t, 0, got["queue_position"])
	fs := s.take(2)
	assert.Equal(t, []string{"download_updated", "download_updated"}, names(fs))
	assert.Equal(t, string(m.bravo.ID), fs[0].json(t)["id"], "the patched Download first, then those it displaced")
	assert.JSONEq(t, r.body, fs[0].data)
	s.requireQuiet()
}

func TestAPIPatchDownloadRejectsBadPatches(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	for name, tc := range map[string]struct{ body, code string }{
		"empty object":          {`{}`, "empty_patch"},
		"null priority":         {`{"priority":null}`, "empty_patch"},
		"null queue position":   {`{"queue_position":null}`, "empty_patch"},
		"queue position empty":  {`{"queue_position":{}}`, "invalid_move"},
		"empty move":            {`{"queue_position":{"move":""}}`, "invalid_move"},
		"unknown move":          {`{"queue_position":{"move":"middle"}}`, "invalid_move"},
		"move in upper case":    {`{"queue_position":{"move":"TOP"}}`, "invalid_move"},
		"unknown field":         {`{"name":"renamed"}`, "invalid_json"},
		"state is not a field":  {`{"state":"ready"}`, "invalid_json"},
		"paused is not a field": {`{"paused":true}`, "invalid_json"},
		"unknown nested field":  {`{"queue_position":{"move":"top","by":2}}`, "invalid_json"},
		"priority wrong type":   {`{"priority":1}`, "invalid_json"},
		"category wrong type":   {`{"category_id":7}`, "invalid_json"},
		"queue position type":   {`{"queue_position":"top"}`, "invalid_json"},
		"not an object":         {`[]`, "invalid_json"},
		"a string":              {`"high"`, "invalid_json"},
		"truncated":             {`{"priority":`, "invalid_json"},
		"trailing data":         {`{"priority":"low"} {}`, "invalid_json"},
		"trailing garbage":      {`{"priority":"low"}x`, "invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			r := d.api(http.MethodPatch, dlPath(m.alpha), tc.body)
			requireError(t, r, http.StatusBadRequest, tc.code)
		})
	}
	got := obj(t, d.apiGet(dlPath(m.alpha)))
	assert.Equal(t, "normal", got["priority"])
	assert.Equal(t, "2026-03-01T12:00:00Z", got["updated_at"], "no bad patch touched the Download")
}

func TestAPIPatchDownloadErrorMessagesWrittenByTheApp(t *testing.T) {
	t.Parallel()
	var m mix
	d := startAPI(t, func(c configDir) { m = seedMix(t, c) })

	requireErrorMessage(t, d.api(http.MethodPatch, dlPath(m.alpha), `{}`), 400, "empty_patch",
		"nothing to change: give priority, category_id or queue_position")
	requireErrorMessage(t, d.api(http.MethodPatch, dlPath(m.alpha), `{"queue_position":{}}`), 400, "invalid_move",
		`queue_position needs a "move": top, bottom, up or down`)
	requireErrorMessage(t, d.request(http.MethodPatch, dlPath(m.alpha), d.apiHeaders(), ""), 400, "invalid_json", "the request body is empty")
}

func TestAPIPatchDownloadOfAnUnknownIDIsNotFoundButAnEmptyPatchIsCheckedFirst(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	notFound := d.api(http.MethodPatch, "/api/v1/downloads/nope", `{"priority":"low"}`)
	empty := d.api(http.MethodPatch, "/api/v1/downloads/nope", `{}`)
	badPriority := d.api(http.MethodPatch, "/api/v1/downloads/nope", `{"priority":"urgent"}`)
	badJSON := d.api(http.MethodPatch, "/api/v1/downloads/nope", `{`)

	assert.Equal(t, errBody("not_found", "not found"), notFound.body)
	assert.Equal(t, http.StatusNotFound, notFound.status)
	requireError(t, empty, http.StatusBadRequest, "empty_patch")
	requireError(t, badPriority, http.StatusBadRequest, "invalid_priority")
	requireError(t, badJSON, http.StatusBadRequest, "invalid_json")
}

func TestAPIDeleteDownloadRemovesItAndAnnouncesIt(t *testing.T) {
	t.Parallel()
	var m mix
	cfg := prepareConfigDir(t)
	m = seedMix(t, cfg)
	d := startDaemon(t, cfg)
	s := d.openStreamAfterHello()

	r := d.api(http.MethodDelete, dlPath(m.bravo), "")

	requireNoContent(t, r)
	f := s.next()
	assert.Equal(t, "removed", f.name)
	assert.JSONEq(t, `{"id":"`+string(m.bravo.ID)+`"}`, f.data)
	s.requireQuiet()
	assert.Equal(t, http.StatusNotFound, d.apiGet(dlPath(m.bravo)).status)
	assert.Equal(t, []string{"alpha", "charlie", "delta", "echo", "foxtrot"}, namesOfItems(itemsOf(t, d.apiGet("/api/v1/downloads"))))
	again := d.api(http.MethodDelete, dlPath(m.bravo), "")
	assert.Equal(t, errBody("not_found", "not found"), again.body)
	d.stop()
	_, err := storedDownload(t, cfg, m.bravo.ID)
	assert.Error(t, err, "the row is gone")
	assert.Len(t, storedDownloads(t, cfg), 5)
}

func TestAPIDeleteDownloadAcceptsAndRefusesDeleteDataValues(t *testing.T) {
	t.Parallel()
	d := startAPI(t, func(c configDir) {
		var ds []core.Download
		for i := range 12 {
			ds = append(ds, newDownload(fmt.Sprintf("d%02d", i), at(i)))
		}
		seedDownloads(t, c, ds...)
	})
	all := itemsOf(t, d.apiGet("/api/v1/downloads"))
	require.Len(t, all, 12)
	i := 0
	for _, v := range []string{"", "true", "false", "1", "0", "t", "f", "T", "F", "TRUE", "FALSE", "True"} {
		path := "/api/v1/downloads/" + all[i]["id"].(string) + "?delete_data=" + v
		requireNoContent(t, d.api(http.MethodDelete, path, ""))
		i++
	}
	assert.Empty(t, itemsOf(t, d.apiGet("/api/v1/downloads")))

	d2 := startAPI(t, func(c configDir) { seedDownloads(t, c, newDownload("keep")) })
	keep := itemsOf(t, d2.apiGet("/api/v1/downloads"))[0]["id"].(string)
	for _, v := range []string{"yes", "no", "on", "2", "tRUE", "true%20"} {
		r := d2.api(http.MethodDelete, "/api/v1/downloads/"+keep+"?delete_data="+v, "")
		requireError(t, r, http.StatusBadRequest, "invalid_delete_data")
	}
	requireErrorMessage(t, d2.api(http.MethodDelete, "/api/v1/downloads/"+keep+"?delete_data=maybe", ""), 400, "invalid_delete_data",
		"delete_data must be true or false")
	requireErrorMessage(t, d2.api(http.MethodDelete, "/api/v1/downloads/"+keep+"?force=1", ""), 400, "unknown_parameter",
		`unknown query parameter "force"`)
	requireError(t, d2.api(http.MethodDelete, "/api/v1/downloads/nope?force=1", ""), 400, "unknown_parameter")
	requireError(t, d2.api(http.MethodDelete, "/api/v1/downloads/nope?delete_data=maybe", ""), 400, "invalid_delete_data")
	assert.Len(t, itemsOf(t, d2.apiGet("/api/v1/downloads")), 1, "nothing was deleted by a refused request")
}

func TestAPIDeleteDownloadBeingPostProcessedIsRefusedWithAndWithoutData(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	d := startDaemon(t, cfg)
	busy := newDownload("busy", inState(core.StatePostProcessing), func(d *core.Download) {
		d.OutputPath = filepath.Join(roots.complete, "busy")
	})
	require.NoError(t, os.MkdirAll(busy.OutputPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(busy.OutputPath, "file"), []byte("x"), 0o600))
	seedDownloads(t, cfg, busy)
	const msg = "This download is being post-processed (repaired or unpacked) and its files are in use. Remove it once it is ready or has failed."

	plain := d.api(http.MethodDelete, dlPath(busy), "")
	withData := d.api(http.MethodDelete, dlPath(busy)+"?delete_data=true", "")

	requireErrorMessage(t, plain, http.StatusConflict, "download_busy", msg)
	requireErrorMessage(t, withData, http.StatusConflict, "download_busy", msg)
	assert.FileExists(t, filepath.Join(busy.OutputPath, "file"))
	assert.Equal(t, http.StatusOK, d.apiGet(dlPath(busy)).status)
}

// dataRoots are the four directories the data remover works inside.
type dataRoots struct {
	usenetIncomplete, complete, torrentIncomplete, torrentComplete string
	outside                                                        string
}

// useDataRoots stores Settings whose paths are fresh temp directories, and
// returns them (with symlinks in the temp path resolved). outside is a
// directory that no setting or Category names.
func useDataRoots(t *testing.T, cfg configDir) dataRoots {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	r := dataRoots{
		usenetIncomplete:  filepath.Join(base, "usenet-incomplete"),
		complete:          filepath.Join(base, "usenet-complete"),
		torrentIncomplete: filepath.Join(base, "torrent-incomplete"),
		torrentComplete:   filepath.Join(base, "torrent-complete"),
		outside:           filepath.Join(base, "outside"),
	}
	for _, p := range []string{r.usenetIncomplete, r.complete, r.torrentIncomplete, r.torrentComplete, r.outside} {
		require.NoError(t, os.MkdirAll(p, 0o755))
	}
	s := core.DefaultSettings()
	s.Paths.Usenet = core.Roots{Incomplete: r.usenetIncomplete, Complete: r.complete}
	s.Paths.Torrent = core.Roots{Incomplete: r.torrentIncomplete, Complete: r.torrentComplete}
	saveSettings(t, cfg, s)
	return r
}

func writeTree(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	f := filepath.Join(dir, "sub", "file.bin")
	require.NoError(t, os.WriteFile(f, []byte("data"), 0o600))
	return f
}

func TestAPIDeleteDownloadWithDataRemovesTheWorkDirAndTheOutput(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	dl := newDownload("done", inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = filepath.Join(roots.complete, "series", "done")
	})
	work := filepath.Join(roots.usenetIncomplete, string(dl.ID))
	workFile := writeTree(t, work)
	outFile := writeTree(t, dl.OutputPath)
	sibling := writeTree(t, filepath.Join(roots.complete, "series", "sibling"))
	seedDownloads(t, cfg, dl)
	d := startDaemon(t, cfg)
	s := d.openStreamAfterHello()

	r := d.api(http.MethodDelete, dlPath(dl)+"?delete_data=true", "")

	requireNoContent(t, r)
	assert.NoFileExists(t, workFile)
	assert.NoDirExists(t, work)
	assert.NoFileExists(t, outFile)
	assert.NoDirExists(t, dl.OutputPath)
	assert.FileExists(t, sibling, "only the Download's own directories go")
	assert.DirExists(t, roots.complete, "never the root")
	assert.DirExists(t, roots.usenetIncomplete)
	assert.Equal(t, "removed", s.next().name)
	assert.Equal(t, http.StatusNotFound, d.apiGet(dlPath(dl)).status)
}

func TestAPIDeleteDownloadWithoutDataKeepsTheFiles(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	dl := newDownload("done", inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = filepath.Join(roots.complete, "done")
	})
	outFile := writeTree(t, dl.OutputPath)
	workFile := writeTree(t, filepath.Join(roots.usenetIncomplete, string(dl.ID)))
	seedDownloads(t, cfg, dl)
	d := startDaemon(t, cfg)

	plain := d.api(http.MethodDelete, dlPath(dl), "")
	again := d.api(http.MethodDelete, dlPath(dl)+"?delete_data=false", "")

	requireNoContent(t, plain)
	assert.Equal(t, http.StatusNotFound, again.status)
	assert.FileExists(t, outFile)
	assert.FileExists(t, workFile)
}

func TestAPIDeleteDownloadWithDataLeavesAnythingOutsideTheConfiguredRoots(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	outsideFile := writeTree(t, filepath.Join(roots.outside, "precious"))
	rootsOwnFile := filepath.Join(roots.complete, "keep.txt")
	require.NoError(t, os.WriteFile(rootsOwnFile, []byte("x"), 0o600))
	elsewhere := newDownload("elsewhere", inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = filepath.Join(roots.outside, "precious")
	})
	atRoot := newDownload("atroot", at(1), inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = roots.complete
	})
	relative := newDownload("relative", at(2), inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = "precious"
	})
	climbing := newDownload("climbing", at(3), inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = filepath.Join(roots.complete, "..", "outside", "precious")
	})
	seedDownloads(t, cfg, elsewhere, atRoot, relative, climbing)
	d := startDaemon(t, cfg)

	for _, dl := range []core.Download{elsewhere, atRoot, relative, climbing} {
		r := d.api(http.MethodDelete, dlPath(dl)+"?delete_data=true", "")
		requireNoContent(t, r)
	}

	assert.FileExists(t, outsideFile, "a path outside the roots is left alone")
	assert.FileExists(t, rootsOwnFile, "a root itself is left alone")
	assert.Empty(t, itemsOf(t, d.apiGet("/api/v1/downloads")), "the Downloads are removed all the same")
}

func TestAPIDeleteDownloadWithDataDeletesInsideACategoryDirOverride(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	override := filepath.Join(roots.outside, "series-override")
	require.NoError(t, os.MkdirAll(override, 0o755))
	db := openDB(t, cfg)
	cat := core.Category{ID: core.NewID(), Name: "overridden", UsenetDir: override}
	require.NoError(t, newCategoryRepo(db).Create(t.Context(), cat))
	require.NoError(t, db.Close())
	dl := newDownload("done", inState(core.StateReady), inCategory(cat.ID), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = filepath.Join(override, "done")
	})
	outFile := writeTree(t, dl.OutputPath)
	seedDownloads(t, cfg, dl)
	d := startDaemon(t, cfg)

	requireNoContent(t, d.api(http.MethodDelete, dlPath(dl)+"?delete_data=true", ""))

	assert.NoFileExists(t, outFile)
	assert.DirExists(t, override, "the override itself stays")
}

func TestAPIDeleteDownloadWithDataUnlinksASymlinkAndNeverFollowsIt(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	target := filepath.Join(roots.outside, "target")
	targetFile := writeTree(t, target)
	link := filepath.Join(roots.complete, "link")
	require.NoError(t, os.Symlink(target, link))
	dl := newDownload("linked", inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = link
	})
	seedDownloads(t, cfg, dl)
	d := startDaemon(t, cfg)

	requireNoContent(t, d.api(http.MethodDelete, dlPath(dl)+"?delete_data=true", ""))

	_, err := os.Lstat(link)
	assert.True(t, os.IsNotExist(err), "the link itself is removed")
	assert.FileExists(t, targetFile, "what it pointed at is not")
}

func TestAPIDeleteDownloadWithDataRefusesAPathThatClimbsOutThroughASymlink(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	targetFile := writeTree(t, filepath.Join(roots.outside, "dir"))
	require.NoError(t, os.Symlink(roots.outside, filepath.Join(roots.complete, "escape")))
	dl := newDownload("escaping", inState(core.StateReady), func(d *core.Download) {
		d.ReadyAt = fixtureBase
		d.OutputPath = filepath.Join(roots.complete, "escape", "dir")
	})
	seedDownloads(t, cfg, dl)
	d := startDaemon(t, cfg)

	r := d.api(http.MethodDelete, dlPath(dl)+"?delete_data=true", "")

	requireError(t, r, http.StatusInternalServerError, "internal")
	assert.FileExists(t, targetFile)
	assert.Equal(t, http.StatusOK, d.apiGet(dlPath(dl)).status, "the Download stays so the user can retry")
}

func TestAPIDeleteDownloadWithDataOfAFailedDownloadRemovesItsWorkDir(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	dl := newDownload("broken", inState(core.StateFailed), func(d *core.Download) {
		d.FailureReason = core.FailureMissingArticles
		d.FailureDetail = "12 articles"
	})
	work := filepath.Join(roots.usenetIncomplete, string(dl.ID))
	workFile := writeTree(t, work)
	seedDownloads(t, cfg, dl)
	d := startDaemon(t, cfg)

	requireNoContent(t, d.api(http.MethodDelete, dlPath(dl)+"?delete_data=true", ""))

	assert.NoFileExists(t, workFile)
	assert.NoDirExists(t, work)
}

func TestAPIDeleteDownloadWithDataDoesNotRemoveWhatIsOutsideTheIncompleteRootWorkDirName(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	roots := useDataRoots(t, cfg)
	dl := newDownload("added", at(0))
	other := newDownload("other", at(1))
	otherFile := writeTree(t, filepath.Join(roots.usenetIncomplete, string(other.ID)))
	ownFile := writeTree(t, filepath.Join(roots.usenetIncomplete, string(dl.ID)))
	seedDownloads(t, cfg, dl, other)
	d := startDaemon(t, cfg)

	requireNoContent(t, d.api(http.MethodDelete, dlPath(dl)+"?delete_data=true", ""))

	assert.NoFileExists(t, ownFile)
	assert.FileExists(t, otherFile)
}
