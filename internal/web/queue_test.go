package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// rowFor finds the table row of a Download.
func rowFor(doc *html.Node, id core.ID) *html.Node {
	return byID(doc, "dl-"+string(id))
}

func queueDoc(t *testing.T, q *qenv) *html.Node {
	t.Helper()
	rec := q.b.get("/queue")
	require.Equal(t, http.StatusOK, rec.Code)
	return parseHTML(t, rec.Body.String())
}

func badgeOf(row *html.Node) *html.Node {
	return find(row, func(n *html.Node) bool { return strings.Contains(attr(n, "class"), "badge ") })
}

func TestQueuePageIsEmptyWithoutDownloads(t *testing.T) {
	q := newQueueEnv(t)

	doc := queueDoc(t, q)

	assert.Contains(t, text(doc), "No Usenet downloads")
	assert.Contains(t, text(doc), "No torrents")
	assert.Empty(t, findAll(doc, func(n *html.Node) bool { return n.Data == "tr" && strings.HasPrefix(attr(n, "id"), "dl-") }))
}

func TestQueueListsOnlyActiveDownloadsInTheSectionOfTheirProtocol(t *testing.T) {
	q := newQueueEnv(t)
	nzb := q.add(core.Download{Name: "Some.Show.S01E01", State: core.StateFetching, TotalBytes: 1000, DoneBytes: 250})
	tor := q.add(core.Download{Name: "ubuntu.iso", SourceKind: core.SourceTorrent, State: core.StateQueued})
	ready := q.add(core.Download{Name: "Done.Release", State: core.StateReady, OutputPath: "/x"})
	failed := q.add(core.Download{Name: "Broken.Release", State: core.StateFailed, FailureReason: core.FailureInternal})

	doc := queueDoc(t, q)

	usenet, torrents := byID(doc, "queue-rows-usenet"), byID(doc, "queue-rows-torrent")
	require.NotNil(t, usenet)
	require.NotNil(t, torrents)
	assert.NotNil(t, rowFor(usenet, nzb.ID))
	assert.Nil(t, rowFor(torrents, nzb.ID))
	assert.NotNil(t, rowFor(torrents, tor.ID))
	assert.Nil(t, rowFor(usenet, tor.ID))
	assert.Nil(t, rowFor(doc, ready.ID), "Ready belongs to the history")
	assert.Nil(t, rowFor(doc, failed.ID), "Failed belongs to the history")
}

func TestQueueRowShowsTheFactsOfADownload(t *testing.T) {
	q := newQueueEnv(t)
	cat, err := q.categories.Create(t.Context(), "movies", "")
	require.NoError(t, err)
	d := q.add(core.Download{
		Name: "Big.Release.2160p", State: core.StateFetching, CategoryID: cat.ID, Priority: core.DownloadPriorityHigh,
		TotalBytes: 4 << 30, DoneBytes: 1 << 30,
	})
	q.rt.setLive(d.ID, web.DownloadLive{DownRate: 8 << 20})

	row := rowFor(queueDoc(t, q), d.ID)

	require.NotNil(t, row)
	got := text(row)
	assert.Contains(t, got, "Big.Release.2160p")
	assert.Contains(t, got, "Downloading")
	assert.Contains(t, got, "movies")
	assert.Contains(t, got, "High")
	assert.Contains(t, got, "25%")
	assert.Contains(t, got, "1.0 GB / 4.0 GB")
	assert.Contains(t, got, "8.0 MB/s")
	assert.Contains(t, got, "6m 24s", "ETA: 3 GB at 8 MB/s")
	bar := find(row, func(n *html.Node) bool { return n.Data == "progress" })
	require.NotNil(t, bar)
	assert.Equal(t, "25", attr(bar, "value"))
	assert.Equal(t, "100", attr(bar, "max"))
	assert.NotEmpty(t, attr(bar, "aria-label"))
	assert.Equal(t, "Big.Release.2160p", attr(find(row, func(n *html.Node) bool { return hasAttr(n, "title") && attr(n, "title") == d.Name }), "title"), "full name as tooltip")
}

func TestQueueBadgesFollowTheStateOfTheDownload(t *testing.T) {
	q := newQueueEnv(t)
	for _, tc := range []struct {
		name   string
		d      core.Download
		stage  core.Stage
		badge  string
		label  string
		absent string
	}{
		{"added", core.Download{State: core.StateAdded}, "", "badge-queued", "Queued", ""},
		{"queued", core.Download{State: core.StateQueued}, "", "badge-queued", "Queued", ""},
		{"metadata", core.Download{State: core.StateFetchingMetadata, SourceKind: core.SourceTorrent}, "", "badge-downloading", "Metadata", ""},
		{"fetching", core.Download{State: core.StateFetching}, "", "badge-downloading", "Downloading", ""},
		{"paused", core.Download{State: core.StateFetching, Paused: true}, "", "badge-paused", "Paused", ""},
		{"paused queued", core.Download{State: core.StateQueued, Paused: true}, "", "badge-paused", "Paused", ""},
		{"fetched nzb", core.Download{State: core.StateFetched}, "", "badge-processing", "Fetched", ""},
		{"seeding", core.Download{State: core.StateFetched, SourceKind: core.SourceTorrent}, "", "badge-ready", "Seeding", ""},
		{"processing", core.Download{State: core.StatePostProcessing}, "", "badge-processing", "Processing", ""},
		{"verifying", core.Download{State: core.StatePostProcessing}, core.StageVerifying, "badge-processing", "Verifying", ""},
		{"repairing", core.Download{State: core.StatePostProcessing}, core.StageRepairing, "badge-processing", "Repairing", ""},
		{"extracting", core.Download{State: core.StatePostProcessing}, core.StageUnpacking, "badge-processing", "Extracting", ""},
		{"moving", core.Download{State: core.StatePostProcessing}, core.StageFinishing, "badge-processing", "Moving", ""},
	} {
		d := q.add(tc.d)
		if tc.stage != "" {
			q.rt.setLive(d.ID, web.DownloadLive{Stage: tc.stage})
		}

		row := rowFor(queueDoc(t, q), d.ID)

		require.NotNil(t, row, tc.name)
		badge := badgeOf(row)
		require.NotNil(t, badge, tc.name)
		assert.Contains(t, attr(badge, "class"), tc.badge, tc.name)
		assert.Equal(t, tc.label, text(badge), tc.name)
	}
}

func TestQueueRowsCoverEveryNonTerminalState(t *testing.T) {
	q := newQueueEnv(t)
	var ids []core.ID
	for _, st := range []core.State{core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching, core.StateFetched, core.StatePostProcessing} {
		ids = append(ids, q.add(core.Download{State: st}).ID)
	}

	doc := queueDoc(t, q)

	for _, id := range ids {
		assert.NotNil(t, rowFor(doc, id), id)
	}
}

func TestQueueOrdersProcessingFirstThenFetchedThenTheQueueByPosition(t *testing.T) {
	q := newQueueEnv(t)
	third := q.add(core.Download{Name: "third", State: core.StateQueued, QueuePosition: 30})
	first := q.add(core.Download{Name: "first", State: core.StateFetching, QueuePosition: 10})
	second := q.add(core.Download{Name: "second", State: core.StateQueued, QueuePosition: 20})
	fetched := q.add(core.Download{Name: "fetched", State: core.StateFetched, QueuePosition: 1})
	processing := q.add(core.Download{Name: "processing", State: core.StatePostProcessing, QueuePosition: 2})

	doc := queueDoc(t, q)

	var got []string
	for _, tr := range findAll(byID(doc, "queue-rows-usenet"), func(n *html.Node) bool { return n.Data == "tr" }) {
		got = append(got, attr(tr, "id"))
	}
	want := []core.ID{processing.ID, fetched.ID, first.ID, second.ID, third.ID}
	var wantIDs []string
	for _, id := range want {
		wantIDs = append(wantIDs, "dl-"+string(id))
	}
	assert.Equal(t, wantIDs, got)
}

func TestTorrentRowShowsUploadSwarmAndRatioWhileSeeding(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{Name: "ubuntu.iso", SourceKind: core.SourceTorrent, State: core.StateFetched, TotalBytes: 100, DoneBytes: 100})
	q.rt.setLive(d.ID, web.DownloadLive{Torrent: true, UpRate: 2 << 20, Peers: 12, Seeds: 3, Ratio: 1.256})

	row := rowFor(queueDoc(t, q), d.ID)

	got := text(row)
	assert.Contains(t, got, "Seeding")
	assert.Contains(t, got, "100%")
	assert.Contains(t, got, "2.0 MB/s")
	assert.Contains(t, got, "12 peers")
	assert.Contains(t, got, "ratio 1.26")
}

func TestUsenetRowHasNoUploadOrSwarm(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetching, TotalBytes: 10, DoneBytes: 5})
	q.rt.setLive(d.ID, web.DownloadLive{DownRate: 1 << 20})

	row := rowFor(queueDoc(t, q), d.ID)

	assert.NotContains(t, text(row), "peers")
	assert.NotContains(t, text(row), "ratio")
}

func TestQueueRowsAreInertForHostileNames(t *testing.T) {
	for _, name := range hostile {
		q := newQueueEnv(t)
		d := q.add(core.Download{Name: name, State: core.StateFetching})
		tor := q.add(core.Download{Name: name, SourceKind: core.SourceTorrent, State: core.StateQueued})

		rec := q.b.get("/queue")

		require.Equal(t, http.StatusOK, rec.Code)
		assertInert(t, rec.Body.String(), "queue "+name)
		doc := parseHTML(t, rec.Body.String())
		for _, id := range []core.ID{d.ID, tor.ID} {
			row := rowFor(doc, id)
			require.NotNil(t, row)
			assert.Contains(t, text(row), strings.Join(strings.Fields(name), " "), "shown as text")
		}
	}
}

func TestQueueWorksWithoutARuntimeSource(t *testing.T) {
	q := newQueueEnv(t, withoutRuntime())
	d := q.add(core.Download{State: core.StateFetching, TotalBytes: 10, DoneBytes: 5})

	doc := queueDoc(t, q)

	row := rowFor(doc, d.ID)
	require.NotNil(t, row)
	assert.Contains(t, text(row), "50%")
	assert.Nil(t, find(doc, func(n *html.Node) bool { return strings.Contains(attr(n, "hx-post"), "/queues/") }), "no queue controls without a runtime")
}

func TestQueueFragmentIsTheListsWithoutTheLayout(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetching})

	rec := q.b.get("/queue/fragment", htmx())

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "<html")
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, byID(doc, "queue-lists"))
	assert.NotNil(t, rowFor(doc, d.ID))
}

func TestQueuePagesRequireASession(t *testing.T) {
	q := newQueueEnv(t)
	anon := q.env.browser()

	rec := anon.get("/queue/fragment")
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/login?next=%2Fqueue%2Ffragment", location(rec))
}

func TestQueueResponsiveTableStructure(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetching})

	doc := queueDoc(t, q)

	table := find(doc, func(n *html.Node) bool {
		return n.Data == "table" && strings.Contains(attr(n, "class"), "table-responsive")
	})
	require.NotNil(t, table, "the table component's responsive pattern")
	assert.NotNil(t, find(table, func(n *html.Node) bool { return n.Data == "caption" }))
	for _, th := range findAll(table, func(n *html.Node) bool { return n.Data == "th" }) {
		assert.Equal(t, "col", attr(th, "scope"))
	}
	var labels []string
	for _, td := range findAll(rowFor(table, d.ID), func(n *html.Node) bool { return n.Data == "td" }) {
		require.True(t, hasAttr(td, "data-label"), "every cell has a data-label for the card layout")
		labels = append(labels, attr(td, "data-label"))
	}
	assert.Equal(t, []string{"", "Status", "Progress", "Speed", "ETA", ""}, labels)
}

// addQueueResponses renders the queue page and its fragments with every kind
// of row, for the invariants that hold for all HTML (security_test.go,
// a11y_test.go): no inline script or style, names on controls, unique ids.
func addQueueResponses(t *testing.T, out map[string]*httptest.ResponseRecorder) {
	q := newQueueEnv(t)
	cat, err := q.categories.Create(t.Context(), "movies", "")
	require.NoError(t, err)
	var first core.Download
	for i, st := range []core.State{core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching, core.StateFetched, core.StatePostProcessing} {
		for _, kind := range []core.SourceKind{core.SourceNZB, core.SourceTorrent} {
			d := q.add(core.Download{Name: hostile[i%len(hostile)], State: st, SourceKind: kind, CategoryID: cat.ID, Priority: core.DownloadPriorityHigh, TotalBytes: 100, DoneBytes: 40, Paused: i == 2 && kind == core.SourceNZB})
			if first.ID == "" {
				first = d
			}
			q.rt.setLive(d.ID, web.DownloadLive{Torrent: kind == core.SourceTorrent, Peers: 3, Ratio: 0.5, DownRate: 1 << 20, UpRate: 1 << 10, Stage: core.StageUnpacking})
		}
	}
	q.rt.setQueue(core.SourceTorrent, web.QueueState{Available: true, Paused: true, Reason: web.PauseReasonDiskFull})
	out["GET /queue (rows)"] = q.b.get("/queue")
	out["GET /queue/fragment"] = q.b.get("/queue/fragment", htmx())
	out["POST /queue/{id}/priority (row)"] = q.act("/queue/"+string(first.ID)+"/priority", url.Values{"priority": {"low"}})
	out["POST /queue/{id}/move (lists)"] = q.act("/queue/"+string(first.ID)+"/move", url.Values{"to": {"bottom"}})
	out["POST /queue/{id}/pause (conflict)"] = q.act("/queue/nope/pause", nil)
	out["POST /queues/usenet/speed"] = q.act("/queues/usenet/speed", url.Values{"down_kbps": {"10"}})
	empty := newQueueEnv(t, withoutRuntime())
	out["GET /queue (empty, no runtime)"] = empty.b.get("/queue")
}
