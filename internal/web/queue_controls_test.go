package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/web"
)

func section(doc *html.Node, kind string) *html.Node {
	return find(doc, func(n *html.Node) bool { return n.Data == "section" && attr(n, "data-queue") == kind })
}

func TestPausingAQueueCallsTheRuntimeAndShowsTheStrip(t *testing.T) {
	q := newQueueEnv(t)

	rec := q.act("/queues/usenet/pause", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"pause nzb"}, q.rt.recorded())
	doc := parseHTML(t, rec.Body.String())
	sec := section(doc, "usenet")
	require.NotNil(t, sec)
	alert := find(sec, func(n *html.Node) bool { return strings.Contains(attr(n, "class"), "alert-warn") })
	require.NotNil(t, alert, "the paused strip")
	assert.Equal(t, "alert", attr(alert, "role"))
	assert.Contains(t, text(alert), "You paused this queue")
	assert.NotNil(t, section(doc, "torrent"))
	assert.NotNil(t, find(sec, func(n *html.Node) bool { return attr(n, "action") == "/queues/usenet/resume" }), "the button flips to resume")
	level, _ := toastOf(t, rec.Header())
	assert.Equal(t, "success", level)
}

func TestResumingAQueueCallsTheRuntime(t *testing.T) {
	q := newQueueEnv(t)
	q.rt.PauseQueue(core.SourceTorrent)

	rec := q.act("/queues/torrent/resume", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"pause torrent", "resume torrent"}, q.rt.recorded())
	assert.Nil(t, find(section(parseHTML(t, rec.Body.String()), "torrent"), func(n *html.Node) bool { return strings.Contains(attr(n, "class"), "alert-warn") }))
}

func TestPauseReasonsAreExplained(t *testing.T) {
	for reason, want := range map[string]string{
		web.PauseReasonUser:            "You paused this queue",
		web.PauseReasonDiskFull:        "disk almost full",
		web.PauseReasonBindUnavailable: "network not available",
		"something_new":                "something_new",
	} {
		q := newQueueEnv(t)
		q.rt.setQueue(core.SourceTorrent, web.QueueState{Available: true, Paused: true, Reason: reason})

		sec := section(queueDoc(t, q), "torrent")

		alert := find(sec, func(n *html.Node) bool { return strings.Contains(attr(n, "class"), "alert-warn") })
		require.NotNil(t, alert, reason)
		assert.Contains(t, text(alert), want, reason)
	}
}

func TestAReasonIsEscaped(t *testing.T) {
	q := newQueueEnv(t)
	q.rt.setQueue(core.SourceNZB, web.QueueState{Available: true, Paused: true, Reason: `<img src=x onerror=alert(1)>`})

	rec := q.b.get("/queue")

	assertInert(t, rec.Body.String(), "pause reason")
}

func TestUnavailableQueueHasNoControls(t *testing.T) {
	q := newQueueEnv(t)
	q.rt.setQueue(core.SourceTorrent, web.QueueState{})

	doc := queueDoc(t, q)
	sec := section(doc, "torrent")

	assert.Contains(t, text(sec), "Torrents are turned off")
	assert.Nil(t, find(sec, func(n *html.Node) bool { return strings.HasPrefix(attr(n, "hx-post"), "/queues/") }))
	rec := q.act("/queues/torrent/pause", nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, []string(nil), q.rt.recorded())
}

func TestUnknownQueueIs404(t *testing.T) {
	q := newQueueEnv(t)

	rec := q.act("/queues/other/pause", nil)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSpeedLimitIsSetInBytesAndShown(t *testing.T) {
	q := newQueueEnv(t)

	rec := q.act("/queues/torrent/speed", url.Values{"down_kbps": {"2048"}, "up_kbps": {"512"}})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	down, up := q.rt.SpeedLimits(core.SourceTorrent)
	assert.Equal(t, int64(2048*1024), down)
	assert.Equal(t, int64(512*1024), up)
	_, msg := toastOf(t, rec.Header())
	assert.Contains(t, msg, "until bitnews restarts")
	sec := section(parseHTML(t, rec.Body.String()), "torrent")
	assert.Contains(t, text(sec), "2.0 MB/s")
	assert.Contains(t, text(sec), "512.0 KB/s")
	form := find(sec, func(n *html.Node) bool { return attr(n, "action") == "/queues/torrent/speed" })
	require.NotNil(t, form)
	assert.Equal(t, "2048", attr(byID(form, "limit-torrent-down"), "value"))
	assert.Equal(t, "512", attr(byID(form, "limit-torrent-up"), "value"))
	assert.Contains(t, text(form), "applies until bitnews restarts")
}

func TestUsenetSpeedLimitHasNoUploadField(t *testing.T) {
	q := newQueueEnv(t)

	rec := q.act("/queues/usenet/speed", url.Values{"down_kbps": {"100"}, "up_kbps": {"999"}})

	require.Equal(t, http.StatusOK, rec.Code)
	down, up := q.rt.SpeedLimits(core.SourceNZB)
	assert.Equal(t, int64(100*1024), down)
	assert.Zero(t, up, "the upload value is ignored for Usenet")
	assert.Nil(t, byID(parseHTML(t, rec.Body.String()), "limit-usenet-up"))
}

func TestSpeedLimitZeroOrEmptyRemovesIt(t *testing.T) {
	q := newQueueEnv(t)
	q.rt.SetSpeedLimit(core.SourceNZB, 5<<20, 0)

	rec := q.act("/queues/usenet/speed", url.Values{"down_kbps": {""}})

	require.Equal(t, http.StatusOK, rec.Code)
	down, _ := q.rt.SpeedLimits(core.SourceNZB)
	assert.Zero(t, down)
	assert.Contains(t, text(section(parseHTML(t, rec.Body.String()), "usenet")), "No limit")
}

func TestInvalidSpeedLimitsAreRefused(t *testing.T) {
	for _, v := range []string{"-1", "abc", "1.5", "99999999999999999999", "1e3"} {
		q := newQueueEnv(t)

		rec := q.act("/queues/usenet/speed", url.Values{"down_kbps": {v}})

		assert.Equal(t, http.StatusBadRequest, rec.Code, v)
		_, msg := toastOf(t, rec.Header())
		assert.Contains(t, msg, "KB/s", v)
		assert.Empty(t, q.rt.recorded(), v)
	}
}

func TestQueueControlsNeedASessionAndCSRF(t *testing.T) {
	q := newQueueEnv(t)

	rec := q.env.browser().post("/queues/usenet/pause", url.Values{}, htmx())
	assert.Equal(t, http.StatusNoContent, rec.Code)
	rec = q.b.post("/queues/usenet/pause", url.Values{}, htmx())
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, q.rt.recorded())
}

func TestSpeedFormHasLabelledFieldsAndAKeyboardOperablePopover(t *testing.T) {
	q := newQueueEnv(t)

	sec := section(queueDoc(t, q), "torrent")

	pop := find(sec, func(n *html.Node) bool { return attr(n, "x-data") == "popover" })
	require.NotNil(t, pop)
	assert.Equal(t, "close", attr(pop, "@keydown.escape"))
	assert.NotNil(t, find(pop, func(n *html.Node) bool { return attr(n, "aria-haspopup") == "dialog" }))
	for _, id := range []string{"limit-torrent-down", "limit-torrent-up"} {
		in := byID(pop, id)
		require.NotNil(t, in, id)
		assert.Equal(t, "number", attr(in, "type"))
		assert.NotNil(t, find(pop, func(n *html.Node) bool { return n.Data == "label" && attr(n, "for") == id }), "label for %s", id)
	}
}
