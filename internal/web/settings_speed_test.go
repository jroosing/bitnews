package web_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/web"
)

func speedLimitsForm(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestSpeedPageShowsTheSavedLimitsInLabelledGroups(t *testing.T) {
	e, b := settingsSignedIn(t)
	cur := e.settings.Current()
	cur.Speed = core.SpeedSettings{
		Global:  core.SpeedLimit{DownKBps: 10000, UpKBps: 2000},
		Usenet:  core.SpeedLimit{DownKBps: 8000},
		Torrent: core.SpeedLimit{DownKBps: 5000, UpKBps: 500},
	}
	require.NoError(t, e.settings.Update(t.Context(), cur))

	doc := parseHTML(t, b.get("/settings/speed").Body.String())

	form := byID(doc, "speed")
	require.NotNil(t, form)
	assert.Equal(t, "post", attr(form, "method"))
	assert.Equal(t, "/settings/speed", attr(form, "action"))
	assert.Equal(t, "/settings/speed", attr(form, "hx-post"))
	assert.NotNil(t, find(form, func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "_csrf" }), "works without JavaScript")
	legends := findAll(form, func(n *html.Node) bool { return n.Data == "legend" })
	require.Len(t, legends, 3)
	assert.Equal(t, "Overall", text(legends[0]))
	assert.Equal(t, "Usenet", text(legends[1]))
	assert.Equal(t, "Torrents", text(legends[2]))
	for id, want := range map[string]string{
		"global-down": "10000", "global-up": "2000", "usenet-down": "8000", "torrent-down": "5000", "torrent-up": "500",
	} {
		in := byID(doc, id)
		require.NotNil(t, in, id)
		assert.Equal(t, want, attr(in, "value"), id)
	}
	assert.Contains(t, text(byID(doc, "speed-card")), "KB/s")
	assert.Contains(t, text(byID(doc, "speed-card")), "no limit")
}

func TestSpeedPageShowsEmptyFieldsWhenThereIsNoLimit(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/speed").Body.String())

	for _, id := range []string{"global-down", "global-up", "usenet-down", "torrent-down", "torrent-up"} {
		assert.Empty(t, attr(byID(doc, id), "value"), id)
	}
}

func TestSavingSpeedLimitsChangesOnlyTheSpeedOfTheSettings(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	before.Auth.AllowedHosts = []string{"example.com"}
	before.Network.Usenet.Interface = "lo0"
	before.Seeding.RatioLimit = 2.5
	before.Queues.Usenet = 3
	before.Torrent.ListenPort = 6881
	require.NoError(t, e.settings.Update(t.Context(), before))

	rec := b.settingsPost("/settings/speed", speedLimitsForm(
		"global_down_kbps", "10000", "global_up_kbps", "", "usenet_down_kbps", " 8000 ", "torrent_down_kbps", "5000", "torrent_up_kbps", "500"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := e.settings.Current()
	assert.Equal(t, core.SpeedSettings{
		Global:  core.SpeedLimit{DownKBps: 10000},
		Usenet:  core.SpeedLimit{DownKBps: 8000},
		Torrent: core.SpeedLimit{DownKBps: 5000, UpKBps: 500},
	}, after.Speed, "an empty field is no limit")
	before.Speed = after.Speed
	assert.Equal(t, before, after, "everything else is untouched")
	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	assert.Contains(t, msg, "Speed limits saved")
	assert.NotContains(t, rec.Body.String(), "<html", "the card only")
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "speed-card"))
}

func TestSavingEmptySpeedLimitsRemovesThem(t *testing.T) {
	e, b := settingsSignedIn(t)
	cur := e.settings.Current()
	cur.Speed.Torrent.DownKBps = 100
	require.NoError(t, e.settings.Update(t.Context(), cur))

	rec := b.settingsPost("/settings/speed", url.Values{})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, core.SpeedSettings{}, e.settings.Current().Speed)
}

func TestBadSpeedLimitsAreRejectedNextToTheirFields(t *testing.T) {
	for name, bad := range map[string]string{
		"letters":     "fast",
		"negative":    "-5",
		"decimal":     "1.5",
		"exponent":    "1e3",
		"hex":         "0x10",
		"too big":     "99999999999",
		"huge digits": "99999999999999999999999999",
		"injection":   `"><script>alert(1)</script>`,
		"unicode":     "١٢٣",
	} {
		t.Run(name, func(t *testing.T) {
			e, b := settingsSignedIn(t)
			before := e.settings.Current()

			rec := b.settingsPost("/settings/speed", speedLimitsForm("global_down_kbps", "100", "torrent_up_kbps", bad))

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, before, e.settings.Current(), "nothing is saved")
			assert.NotContains(t, rec.Body.String(), "<script>alert", "escaped")
			doc := parseHTML(t, rec.Body.String())
			in := byID(doc, "torrent-up")
			assert.Equal(t, bad, attr(in, "value"), "what was typed stays")
			assert.Equal(t, "true", attr(in, "aria-invalid"))
			assert.Contains(t, text(byID(doc, "torrent-up-error")), "whole number")
			assert.Equal(t, "100", attr(byID(doc, "global-down"), "value"))
			assert.Nil(t, byID(doc, "global-down-error"), "the good fields have no error")
			assert.Equal(t, "alert", attr(byID(doc, "speed-error"), "role"))
			assert.Equal(t, "true", attr(byID(doc, "speed"), "data-dirty"), "Save stays enabled")
		})
	}
}

func TestUsenetHasNoUploadLimitField(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/speed").Body.String())

	assert.Nil(t, byID(doc, "usenet-up"))
	assert.Contains(t, text(byID(doc, "speed-card")), "only downloads")
}

func TestSpeedPlainFormPostRendersThePageAgain(t *testing.T) {
	e, b := settingsSignedIn(t)
	form := speedLimitsForm("usenet_down_kbps", "300")
	form.Set("_csrf", b.csrf())

	rec := b.post("/settings/speed", form)

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, find(doc, func(n *html.Node) bool { return n.Data == "html" }), "a whole page")
	assert.Contains(t, text(byID(doc, "speed-card")), "Your changes were saved")
	assert.Equal(t, 300, e.settings.Current().Speed.Usenet.DownKBps)
	assert.NotNil(t, byID(doc, "queue-card"), "the other cards are on the page too")

	form.Set("usenet_down_kbps", "x")
	rec = b.post("/settings/speed", form)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "usenet-down-error"))
}

// fakeConfig is the daemon's view of the running configuration.
type fakeConfig struct {
	started      core.Settings
	bindings     map[core.SourceKind]web.BindingStatus
	portOverride int
	// port is the live listen port status; nil means torrents are off.
	port *core.ListenPortStatus
}

func (f *fakeConfig) ListenPort() (core.ListenPortStatus, bool) {
	if f.port == nil {
		return core.ListenPortStatus{}, false
	}
	return *f.port, true
}

func (f *fakeConfig) Started() core.Settings   { return f.started }
func (f *fakeConfig) TorrentPortOverride() int { return f.portOverride }
func (f *fakeConfig) Binding(k core.SourceKind) web.BindingStatus {
	return f.bindings[k]
}

func queueForm(usenet, torrent, postproc string) url.Values {
	return url.Values{"usenet_queue": {usenet}, "torrent_queue": {torrent}, "postproc_concurrency": {postproc}}
}

func TestSpeedPageShowsTheQueueSizesAndPostProcessingConcurrency(t *testing.T) {
	e, b := settingsSignedIn(t)
	cur := e.settings.Current()
	cur.Queues = core.QueueLimits{Usenet: 2, Torrent: 7, PostprocConcurrency: 3, MinFree: 1 << 30}
	require.NoError(t, e.settings.Update(t.Context(), cur))

	doc := parseHTML(t, b.get("/settings/speed").Body.String())

	form := byID(doc, "queue")
	require.NotNil(t, form)
	assert.Equal(t, "/settings/speed/queue", attr(form, "hx-post"))
	assert.Equal(t, "2", attr(byID(doc, "usenet-queue"), "value"))
	assert.Equal(t, "7", attr(byID(doc, "torrent-queue"), "value"))
	assert.Equal(t, "3", attr(byID(doc, "postproc-concurrency"), "value"))
	card := text(byID(doc, "queue-card"))
	assert.Contains(t, card, "at the same time")
	assert.Contains(t, card, "restart", "post-processing concurrency is read at start-up")
}

func TestSavingQueueSizesChangesOnlyTheQueuesOfTheSettings(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	before.Speed.Global.DownKBps = 100
	before.Queues.MinFree = 3 << 30
	require.NoError(t, e.settings.Update(t.Context(), before))

	rec := b.settingsPost("/settings/speed/queue", queueForm("2", " 8 ", "2"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := e.settings.Current()
	assert.Equal(t, core.QueueLimits{Usenet: 2, Torrent: 8, PostprocConcurrency: 2, MinFree: 3 << 30}, after.Queues, "the free space setting is kept")
	before.Queues = after.Queues
	assert.Equal(t, before, after)
	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level, "no restart is pending without a daemon to compare with")
	assert.Contains(t, msg, "Queue sizes saved")
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "queue-card"))
}

func TestBadQueueSizesAreRejectedNextToTheirFields(t *testing.T) {
	for name, tc := range map[string]struct{ field, id, value string }{
		"zero usenet":      {"usenet_queue", "usenet-queue", "0"},
		"empty torrent":    {"torrent_queue", "torrent-queue", ""},
		"too many":         {"torrent_queue", "torrent-queue", "1001"},
		"negative":         {"usenet_queue", "usenet-queue", "-1"},
		"words":            {"usenet_queue", "usenet-queue", "<b>two</b>"},
		"zero postproc":    {"postproc_concurrency", "postproc-concurrency", "0"},
		"too much postpro": {"postproc_concurrency", "postproc-concurrency", "17"},
	} {
		t.Run(name, func(t *testing.T) {
			e, b := settingsSignedIn(t)
			before := e.settings.Current()
			form := queueForm("1", "5", "1")
			form.Set(tc.field, tc.value)

			rec := b.settingsPost("/settings/speed/queue", form)

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, before, e.settings.Current())
			assert.NotContains(t, rec.Body.String(), "<b>two")
			doc := parseHTML(t, rec.Body.String())
			assert.Equal(t, tc.value, attr(byID(doc, tc.id), "value"))
			assert.Equal(t, "true", attr(byID(doc, tc.id), "aria-invalid"))
			assert.NotEmpty(t, text(byID(doc, tc.id+"-error")))
			assert.Equal(t, "true", attr(byID(doc, "queue"), "data-dirty"))
		})
	}
}

func TestChangingPostProcessingConcurrencyTellsYouToRestart(t *testing.T) {
	started := core.DefaultSettings()
	cfg := &fakeConfig{started: started}
	_, b := settingsSignedIn(t, withConfig(cfg))

	same := b.settingsPost("/settings/speed/queue", queueForm("1", "5", "1"))
	level, _ := settingsToastOf(t, same)
	assert.Equal(t, "success", level, "nothing to restart for")
	assert.Nil(t, byID(parseHTML(t, same.Body.String()), "queue-restart"))

	changed := b.settingsPost("/settings/speed/queue", queueForm("1", "5", "3"))

	require.Equal(t, http.StatusOK, changed.Code)
	level, msg := settingsToastOf(t, changed)
	assert.Equal(t, "info", level)
	assert.Contains(t, msg, "Restart bitnews")
	note := byID(parseHTML(t, changed.Body.String()), "queue-restart")
	require.NotNil(t, note)
	assert.Contains(t, text(note), "restart")

	page := b.get("/settings/speed").Body.String()
	assert.NotNil(t, byID(parseHTML(t, page), "queue-restart"), "the note stays until bitnews restarts")
}

func TestQueueSizesApplyWithoutARestart(t *testing.T) {
	cfg := &fakeConfig{started: core.DefaultSettings()}
	_, b := settingsSignedIn(t, withConfig(cfg))

	rec := b.settingsPost("/settings/speed/queue", queueForm("4", "9", "1"))

	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	assert.NotContains(t, msg, "Restart")
	assert.Nil(t, byID(parseHTML(t, rec.Body.String()), "queue-restart"))
}

func TestSpeedPageShowsTheFreeSpaceToKeepInGigabytes(t *testing.T) {
	e, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/speed").Body.String())

	assert.Equal(t, "5", attr(byID(doc, "min-free"), "value"), "the 5 GB default")
	assert.Equal(t, "/settings/speed/space", attr(byID(doc, "space"), "hx-post"))

	cur := e.settings.Current()
	cur.Queues.MinFree = 3 << 29 // 1.5 GiB
	require.NoError(t, e.settings.Update(t.Context(), cur))
	doc = parseHTML(t, b.get("/settings/speed").Body.String())
	assert.Equal(t, "1.5", attr(byID(doc, "min-free"), "value"))
}

func TestSavingTheFreeSpaceChangesOnlyMinFree(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	before.Queues.PostprocConcurrency = 2
	require.NoError(t, e.settings.Update(t.Context(), before))

	rec := b.settingsPost("/settings/speed/space", url.Values{"min_free_gb": {" 2.5 "}})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := e.settings.Current()
	assert.Equal(t, int64(5<<29), after.Queues.MinFree)
	before.Queues.MinFree = after.Queues.MinFree
	assert.Equal(t, before, after)
	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	assert.Contains(t, msg, "Free space saved")
	assert.Equal(t, "2.5", attr(byID(parseHTML(t, rec.Body.String()), "min-free"), "value"))
}

func TestBadFreeSpaceIsRejectedNextToItsField(t *testing.T) {
	for name, bad := range map[string]string{
		"empty":    "",
		"zero":     "0",
		"tiny":     "0.01",
		"letters":  "lots",
		"negative": "-1",
		"exponent": "1e3",
		"hex":      "0x10",
		"NaN":      "NaN",
		"inf":      "Inf",
		"huge":     "99999999999",
		"comma":    "1,5",
		"markup":   "<i>5</i>",
	} {
		t.Run(name, func(t *testing.T) {
			e, b := settingsSignedIn(t)
			before := e.settings.Current()

			rec := b.settingsPost("/settings/speed/space", url.Values{"min_free_gb": {bad}})

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, before, e.settings.Current())
			assert.NotContains(t, rec.Body.String(), "<i>5")
			doc := parseHTML(t, rec.Body.String())
			assert.Equal(t, bad, attr(byID(doc, "min-free"), "value"))
			assert.Contains(t, text(byID(doc, "min-free-error")), "GB")
			assert.Equal(t, "true", attr(byID(doc, "space"), "data-dirty"))
		})
	}
}
