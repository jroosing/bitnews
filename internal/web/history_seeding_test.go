package web_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// seeding adds a Ready torrent with the live state of an uploading torrent:
// 1400 of 1000 bytes uploaded, so a ratio of 1.4.
func (d *denv) seeding(name string, mutate ...func(*core.Download, *view.Live)) core.Download {
	d.t.Helper()
	dl := core.Download{Name: name, SourceKind: core.SourceTorrent, TotalBytes: 1000}
	live := view.Live{UpRate: 1_200_000, Torrent: &view.TorrentLive{
		Hash: "0123456789abcdef0123456789abcdef01234567", Registered: true, Size: 1000, Uploaded: 1400,
		OwnRatio: new(2.0), OwnSeedingTime: new(-time.Second),
	}}
	for _, m := range mutate {
		m(&dl, &live)
	}
	dl = d.add(dl)
	d.live.set(dl.ID, live)
	return dl
}

func historyRowOfID(t *testing.T, body string, id core.ID) *html.Node {
	t.Helper()
	row := byID(parseHTML(t, body), "download-"+string(id))
	require.NotNil(t, row, "row of %s", id)
	return row
}

func TestHistoryLabelsEveryRowAsNZBOrTorrent(t *testing.T) {
	d := newDetailEnv(t)
	nzb := d.add(core.Download{Name: "Usenet.One"})
	tor := d.seeding("Torrent.One")

	body := d.b.get("/history").Body.String()

	assert.Contains(t, text(historyRowOfID(t, body, nzb.ID)), "NZB")
	assert.NotContains(t, text(historyRowOfID(t, body, nzb.ID)), "Torrent")
	assert.Contains(t, text(historyRowOfID(t, body, tor.ID)), "Torrent")
}

func TestHistoryShowsASeedingTorrentWithUploadSpeedAndShareLimitProgress(t *testing.T) {
	d := newDetailEnv(t)
	tor := d.seeding("Torrent.Seeding")

	row := historyRowOfID(t, d.b.get("/history").Body.String(), tor.ID)

	got := text(row)
	assert.Contains(t, got, "Seeding")
	assert.Contains(t, got, "1.1 MB/s", "upload speed of 1 200 000 B/s")
	assert.Contains(t, got, "Ratio 1.4 of 2.0")
	pause := find(row, func(n *html.Node) bool {
		return n.Data == "button" && attr(n, "hx-post") == "/history/"+string(tor.ID)+"/pause"
	})
	require.NotNil(t, pause, "an inline pause button")
	assert.Equal(t, "closest tr", attr(pause, "hx-target"))
	assert.Equal(t, "outerHTML", attr(pause, "hx-swap"))
}

func TestHistoryShowsTheDefaultShareLimitAndNoLimit(t *testing.T) {
	d := newDetailEnv(t)
	s := d.settings.Current()
	s.Seeding.RatioLimit = 3
	require.NoError(t, d.settings.Update(t.Context(), s))
	global := d.seeding("Global", func(_ *core.Download, l *view.Live) { l.Torrent.OwnRatio, l.Torrent.OwnSeedingTime = nil, nil })
	unlimited := d.seeding("Unlimited", func(_ *core.Download, l *view.Live) {
		l.Torrent.OwnRatio, l.Torrent.OwnSeedingTime = new(-1.0), new(-time.Second)
	})
	timed := d.seeding("Timed", func(_ *core.Download, l *view.Live) {
		l.Torrent.OwnRatio, l.Torrent.OwnSeedingTime = new(-1.0), new(24*time.Hour)
		l.Torrent.SeedingTime = 3 * time.Hour
	})

	body := d.b.get("/history").Body.String()

	assert.Contains(t, text(historyRowOfID(t, body, global.ID)), "Ratio 1.4 of 3.0")
	assert.Contains(t, text(historyRowOfID(t, body, unlimited.ID)), "Ratio 1.4 · no limit")
	assert.Contains(t, text(historyRowOfID(t, body, timed.ID)), "3h of 1d")
}

func TestHistoryShowsSeedingPausedWithAResumeButton(t *testing.T) {
	d := newDetailEnv(t)
	tor := d.seeding("Torrent.Paused", func(dl *core.Download, _ *view.Live) { dl.Paused = true })

	row := historyRowOfID(t, d.b.get("/history").Body.String(), tor.ID)

	assert.Contains(t, text(row), "Seeding paused")
	assert.NotContains(t, text(row), "MB/s", "a paused torrent does not upload")
	assert.NotNil(t, find(row, func(n *html.Node) bool {
		return n.Data == "button" && attr(n, "hx-post") == "/history/"+string(tor.ID)+"/resume"
	}))
}

func TestHistoryShowsAShareLimitStopWithoutAPauseOrResumeButton(t *testing.T) {
	d := newDetailEnv(t)
	tor := d.seeding("Torrent.Limit", func(_ *core.Download, l *view.Live) {
		l.UpRate, l.Torrent.LimitPaused, l.Torrent.Uploaded = 0, true, 2400
	})

	row := historyRowOfID(t, d.b.get("/history").Body.String(), tor.ID)

	assert.Contains(t, text(row), "Share Limit reached")
	assert.Contains(t, text(row), "Ratio 2.4 of 2.0")
	assert.Nil(t, find(row, func(n *html.Node) bool {
		return n.Data == "button" && (attr(n, "hx-post") == "/history/"+string(tor.ID)+"/pause" || attr(n, "hx-post") == "/history/"+string(tor.ID)+"/resume")
	}))
	link := find(row, func(n *html.Node) bool {
		return n.Data == "a" && attr(n, "href") == "/downloads/"+string(tor.ID)+"#share-card"
	})
	assert.NotNil(t, link, "a link to the Share Limit form on the detail page")
}

func TestHistoryRowsOfNZBsAndFailedTorrentsHaveNoSeedingParts(t *testing.T) {
	d := newDetailEnv(t)
	nzb := d.add(core.Download{Name: "Usenet.One"})
	failed := d.add(core.Download{Name: "Torrent.Failed", SourceKind: core.SourceTorrent, State: core.StateFailed, FailureReason: core.FailureSourceInvalid})

	body := d.b.get("/history").Body.String()

	for _, id := range []core.ID{nzb.ID, failed.ID} {
		row := historyRowOfID(t, body, id)
		assert.NotContains(t, text(row), "Seeding")
		assert.Nil(t, find(row, func(n *html.Node) bool {
			return n.Data == "button" && attr(n, "hx-post") == "/history/"+string(id)+"/pause"
		}))
	}
}

func TestHistoryPausesAndResumesASeedingTorrentAndAnswersWithTheRow(t *testing.T) {
	d := newDetailEnv(t)
	tor := d.seeding("Torrent.Seeding")

	rec := d.act("/history/"+string(tor.ID)+"/pause", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, "<table><tbody>"+rec.Body.String()+"</tbody></table>")
	assert.Contains(t, text(doc), "Seeding paused")
	assert.NotNil(t, find(doc, func(n *html.Node) bool { return n.Data == "tr" && attr(n, "id") == "download-"+string(tor.ID) }))
	got, err := d.repo.Get(t.Context(), tor.ID)
	require.NoError(t, err)
	assert.True(t, got.Paused)

	rec = d.act("/history/"+string(tor.ID)+"/resume", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	got, err = d.repo.Get(t.Context(), tor.ID)
	require.NoError(t, err)
	assert.False(t, got.Paused)
}

func TestHistoryPauseOfANZBIsAConflictToast(t *testing.T) {
	d := newDetailEnv(t)
	nzb := d.add(core.Download{Name: "Usenet.One"})

	rec := d.act("/history/"+string(nzb.ID)+"/pause", nil)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "toast")
}

func TestHistoryRowsCarryTheirIDForThePollAndThePageRunsOnePoll(t *testing.T) {
	d := newDetailEnv(t)
	tor := d.seeding("Torrent.Seeding")

	doc := parseHTML(t, d.b.get("/history").Body.String())

	row := byID(doc, "download-"+string(tor.ID))
	in := find(row, func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "id" })
	require.NotNil(t, in)
	assert.Equal(t, string(tor.ID), attr(in, "value"))
	poll := byID(doc, "history-seeding-poll")
	require.NotNil(t, poll)
	assert.Equal(t, "/history/seeding", attr(poll, "hx-get"))
	assert.Equal(t, "every 5s", attr(poll, "hx-trigger"))
	assert.Equal(t, "#history-rows [name=id]", attr(poll, "hx-include"))
	assert.Equal(t, "none", attr(poll, "hx-swap"))
}

func TestHistorySeedingPollAnswersOutOfBandCellsForTheAskedTorrentsOnly(t *testing.T) {
	d := newDetailEnv(t)
	tor := d.seeding("Torrent.Seeding")
	nzb := d.add(core.Download{Name: "Usenet.One"})

	rec := d.b.get("/history/seeding?"+url.Values{"id": {string(tor.ID), string(nzb.ID), "../x", string(core.NewID())}}.Encode(), htmx())

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	for _, part := range []string{"seed", "seedctl"} {
		cell := byID(doc, "download-"+string(tor.ID)+"-"+part)
		require.NotNil(t, cell, part)
		assert.Equal(t, "true", attr(cell, "hx-swap-oob"), part)
	}
	assert.Nil(t, byID(doc, "download-"+string(nzb.ID)+"-seed"), "an NZB has nothing to refresh")
	assert.Contains(t, text(doc), "1.1 MB/s")
}

func TestHistorySeedingPollWithoutIDsAnswersNoContent(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.b.get("/history/seeding", htmx())

	assert.Equal(t, http.StatusNoContent, rec.Code)
}
