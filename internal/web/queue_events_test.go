package web_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// parseSnippet parses OOB fragments, which are not tables.
func parseSnippet(t *testing.T, s string) *html.Node { return parseHTML(t, "<body>"+s+"</body>") }

func TestStreamRequiresASession(t *testing.T) {
	q := newQueueEnv(t)
	ts := httptest.NewServer(q.handler)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/events", nil)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)

	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a stream is not redirected to the login page")
}

func TestStreamSaysHelloThenTheStatus(t *testing.T) {
	q := newQueueEnv(t)
	q.rt.setRates(3<<20, 1<<20)

	s := q.openStream(t, "?rows=1")

	assert.Equal(t, "text/event-stream", s.resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", s.resp.Header.Get("Cache-Control"))
	assert.Equal(t, "hello", s.next().Name)
	st := s.next()
	assert.Equal(t, "status", st.Name)
	doc := parseSnippet(t, st.Data)
	down, up, badge := byID(doc, "status-down"), byID(doc, "status-up"), byID(doc, "status-queue")
	require.NotNil(t, down)
	assert.Equal(t, "3.0 MB/s", text(down))
	assert.Equal(t, "1.0 MB/s", text(up))
	assert.Equal(t, "Active", text(badge))
	assert.Equal(t, "true", attr(down, "hx-swap-oob"))
	assert.Equal(t, "status", attr(badge, "role"))
	assert.Contains(t, attr(badge, "class"), "badge-downloading")
}

func TestStatusShowsPausedOnlyWhenEveryQueuePauses(t *testing.T) {
	q := newQueueEnv(t)
	q.rt.PauseQueue(core.SourceNZB)
	s := q.openStream(t, "")
	s.until("hello")
	assert.Equal(t, "Idle", text(byID(parseSnippet(t, s.until("status").Data), "status-queue")))

	q.rt.PauseQueue(core.SourceTorrent)
	q.flush()
	assert.Equal(t, "Paused", text(byID(parseSnippet(t, s.until("status").Data), "status-queue")))
}

func TestStatusIsSentOnlyWhenItChanges(t *testing.T) {
	q := newQueueEnv(t)
	s := q.openStream(t, "")
	s.until("hello")
	s.until("status")

	q.flush()
	s.quiet(150 * time.Millisecond)

	q.rt.setRates(1<<20, 0)
	q.flush()
	assert.Equal(t, "1.0 MB/s", text(byID(parseSnippet(t, s.until("status").Data), "status-down")))
}

func TestProgressArrivesAsRenderedCellsOncePerTick(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{Name: "Some.Release", State: core.StateFetching, TotalBytes: 1000})
	s := q.openStream(t, "?rows=1")
	s.until("status")

	for i := int64(1); i <= 50; i++ {
		q.bus.Publish(core.DownloadProgress{ID: d.ID, DoneBytes: i * 10, TotalBytes: 1000, BytesPerSecond: 2 << 20})
	}
	s.quiet(100 * time.Millisecond) // nothing before the tick
	q.flush()

	live := s.until("live")
	doc := parseSnippet(t, live.Data)
	progress := byID(doc, "dl-"+string(d.ID)+"-progress")
	require.NotNil(t, progress)
	assert.Equal(t, "true", attr(progress, "hx-swap-oob"))
	assert.Equal(t, "50", attr(find(progress, func(n *html.Node) bool { return n.Data == "progress" }), "value"), "the last of the 50 events")
	assert.Contains(t, text(progress), "50%")
	assert.Contains(t, text(byID(doc, "dl-"+string(d.ID)+"-speed")), "2.0 MB/s")
	assert.Contains(t, text(byID(doc, "dl-"+string(d.ID)+"-eta")), "s")
	assert.Equal(t, "Downloading", text(byID(doc, "dl-"+string(d.ID)+"-status")))
	assert.Nil(t, byID(doc, "dl-"+string(d.ID)), "cells only: an open menu is not rebuilt")
	q.flush()
	s.quiet(150 * time.Millisecond)
}

func TestProgressOfSeveralDownloadsShareOneEvent(t *testing.T) {
	q := newQueueEnv(t)
	a := q.add(core.Download{State: core.StateFetching, TotalBytes: 100})
	b := q.add(core.Download{State: core.StateFetching, TotalBytes: 100})
	s := q.openStream(t, "?rows=1")
	s.until("status")

	q.bus.Publish(core.DownloadProgress{ID: a.ID, DoneBytes: 10, TotalBytes: 100})
	q.bus.Publish(core.DownloadProgress{ID: b.ID, DoneBytes: 20, TotalBytes: 100})
	q.flush()

	doc := parseSnippet(t, s.until("live").Data)
	assert.NotNil(t, byID(doc, "dl-"+string(a.ID)+"-progress"))
	assert.NotNil(t, byID(doc, "dl-"+string(b.ID)+"-progress"))
}

func TestStageEventUpdatesTheStatusCell(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StatePostProcessing, TotalBytes: 100, DoneBytes: 100})
	s := q.openStream(t, "?rows=1")
	s.until("status")

	q.bus.Publish(core.PostProcessStage{ID: d.ID, Stage: core.StageRepairing})
	q.flush()

	doc := parseSnippet(t, s.until("live").Data)
	badge := byID(doc, "dl-"+string(d.ID)+"-status")
	assert.Equal(t, "Repairing", text(badge))
	assert.Contains(t, attr(find(badge, func(n *html.Node) bool { return n.Data == "span" }), "class"), "badge-processing")
}

func TestLifecycleEventsAskForOneRefresh(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateQueued})
	s := q.openStream(t, "?rows=1")
	s.until("status")

	q.bus.Publish(core.DownloadAdded{Download: d})
	q.bus.Publish(core.DownloadUpdated{Download: d})
	q.bus.Publish(core.Fetched{ID: d.ID})
	q.bus.Publish(core.Ready{ID: d.ID, OutputPath: "/x"})
	q.bus.Publish(core.Failed{ID: d.ID, Reason: core.FailureInternal})
	q.flush()

	assert.Equal(t, "refresh", s.next().Name)
	s.quiet(150 * time.Millisecond)
}

func TestRemovedRowsAreDeletedOutOfBand(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateQueued})
	s := q.openStream(t, "?rows=1")
	s.until("status")

	q.bus.Publish(core.Removed{ID: d.ID})
	q.flush()

	removed := s.until("removed")
	del := byID(parseSnippet(t, removed.Data), "dl-"+string(d.ID))
	require.NotNil(t, del)
	assert.Equal(t, "delete", attr(del, "hx-swap-oob"))
	assert.Equal(t, "refresh", s.next().Name, "and the empty state may need to come back")
}

func TestRunnerQueueEventsRefreshTheLists(t *testing.T) {
	q := newQueueEnv(t)
	s := q.openStream(t, "?rows=1")
	s.until("status")

	for _, kind := range []core.EventKind{web.KindUsenetQueuePaused, web.KindTorrentQueueResumed} {
		q.bus.Publish(kindEvent(kind))
		q.flush()
		assert.Equal(t, "refresh", s.until("refresh").Name, kind)
	}
	for _, ev := range []core.Event{core.QueuePaused{Source: core.SourceNZB, Reason: "user"}, core.QueueResumed{Source: core.SourceNZB}} {
		q.bus.Publish(ev)
		q.flush()
		assert.Equal(t, "refresh", s.until("refresh").Name, ev.Kind())
	}
}

type kindEvent core.EventKind

func (k kindEvent) Kind() core.EventKind { return core.EventKind(k) }

func TestStreamWithoutRowsOnlyCarriesTheStatus(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetching})
	s := q.openStream(t, "")
	s.until("status")

	q.bus.Publish(core.DownloadProgress{ID: d.ID, DoneBytes: 1, TotalBytes: 10})
	q.bus.Publish(core.Removed{ID: d.ID})
	q.flush()

	s.quiet(200 * time.Millisecond)
}

func TestHostileNamesStayInertInLiveCells(t *testing.T) {
	for _, name := range hostile {
		q := newQueueEnv(t)
		d := q.add(core.Download{Name: name, State: core.StateFetching, TotalBytes: 10})
		s := q.openStream(t, "?rows=1")
		s.until("status")

		q.bus.Publish(core.DownloadProgress{ID: d.ID, DoneBytes: 5, TotalBytes: 10})
		q.flush()

		live := s.until("live").Data
		assertInert(t, "<body>"+live+"</body>", "live "+name)
		assert.NotContains(t, live, "<script")
	}
}

func TestMultilineFragmentsKeepEveryLineInTheDataField(t *testing.T) {
	q := newQueueEnv(t)
	d := q.add(core.Download{State: core.StateFetching, TotalBytes: 10})
	s := q.openStream(t, "?rows=1")
	s.until("status")

	q.bus.Publish(core.DownloadProgress{ID: d.ID, DoneBytes: 5, TotalBytes: 10})
	q.flush()

	data := s.until("live").Data
	assert.Contains(t, data, "\n", "the template output spans lines")
	assert.NotNil(t, byID(parseSnippet(t, data), "dl-"+string(d.ID)+"-eta"), "and still parses whole")
}

func TestHeartbeatIsACommentOnItsOwnTicker(t *testing.T) {
	q := newQueueEnv(t)
	s := q.openStream(t, "")
	s.until("status")

	q.ticks.fire(testPing)

	select {
	case line := <-s.lines:
		assert.Equal(t, ": ping", line)
	case <-time.After(3 * time.Second):
		t.Fatal("no heartbeat")
	}
}

func TestOverflowingTheSubscriptionResyncsAndEndsTheStream(t *testing.T) {
	q := newQueueEnv(t)
	s := q.openStream(t, "?rows=1")
	s.until("status")
	release := q.rt.blockRates() // the handler is busy rendering the status
	q.flush()
	time.Sleep(100 * time.Millisecond)

	for range core.SubscriberBuffer + 10 {
		q.bus.Publish(core.Fetched{ID: core.NewID()})
	}
	release()

	assert.Equal(t, "resync", s.until("resync").Name)
	assert.True(t, s.ended(), "the stream ends so the client reconnects")
}

func TestClosingTheClientEndsTheStreamAndLeaksNothing(t *testing.T) {
	q := newQueueEnv(t)
	ts := httptest.NewServer(q.handler)
	t.Cleanup(ts.Close)
	before := runtime.NumGoroutine()
	var streams []*sseStream
	for range 5 {
		s := q.openStreamAt(t, ts, "?rows=1")
		s.until("status")
		streams = append(streams, s)
	}
	assert.Equal(t, 5, web.OpenStreams(q.server))

	for _, s := range streams {
		s.close()
	}

	assert.Eventually(t, func() bool { return web.OpenStreams(q.server) == 0 }, 3*time.Second, 10*time.Millisecond)
	assert.Eventually(t, func() bool {
		ts.CloseClientConnections()
		return runtime.NumGoroutine() <= before+2
	}, 5*time.Second, 20*time.Millisecond, "goroutines: before %d, now %d", before, runtime.NumGoroutine())
}

func TestShuttingTheServerDownEndsTheStreams(t *testing.T) {
	q := newQueueEnv(t)
	base, cancel := context.WithCancel(context.Background())
	ts := httptest.NewUnstartedServer(q.handler)
	ts.Config.BaseContext = func(net.Listener) context.Context { return base }
	ts.Start()
	t.Cleanup(ts.Close)
	s := q.openStreamAt(t, ts, "?rows=1")
	s.until("status")

	cancel() // what api.Server.HTTPServer's shutdown hook does

	assert.True(t, s.ended())
	assert.Eventually(t, func() bool { return web.OpenStreams(q.server) == 0 }, 3*time.Second, 10*time.Millisecond)
}

func TestTooManyStreamsAreRefused(t *testing.T) {
	q := newQueueEnv(t, withLive(func(c *web.LiveConfig) { c.MaxStreams = 2 }))
	ts := httptest.NewServer(q.handler)
	t.Cleanup(ts.Close)
	q.openStreamAt(t, ts, "").until("hello")
	q.openStreamAt(t, ts, "").until("hello")
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/events", nil)
	for _, c := range q.b.cookies {
		req.AddCookie(c)
	}

	resp, err := http.DefaultClient.Do(req)

	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, 2, web.OpenStreams(q.server), "the refused one is not counted")
}

func TestASlowClientIsDropped(t *testing.T) {
	q := newQueueEnv(t, withLive(func(c *web.LiveConfig) { c.WriteTimeout = 200 * time.Millisecond }))
	for range 200 {
		q.add(core.Download{Name: strings.Repeat("n", 200), State: core.StateFetching, TotalBytes: 1000})
	}
	ts := httptest.NewServer(q.handler)
	t.Cleanup(ts.Close)
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(1024))
	var cookie []string
	for _, c := range q.b.cookies {
		cookie = append(cookie, c.Name+"="+c.Value)
	}
	fmt.Fprintf(conn, "GET /ui/events?rows=1 HTTP/1.1\r\nHost: example.com\r\nCookie: %s\r\nAccept: text/event-stream\r\n\r\n", strings.Join(cookie, "; "))
	r := bufio.NewReader(conn)
	_, err = r.ReadString('\n') // the status line: then never read again
	require.NoError(t, err)
	require.Eventually(t, func() bool { return web.OpenStreams(q.server) == 1 }, 3*time.Second, 10*time.Millisecond)

	ids := q.allIDs()
	deadline := time.Now().Add(20 * time.Second)
	for web.OpenStreams(q.server) > 0 && time.Now().Before(deadline) {
		for _, id := range ids {
			q.bus.Publish(core.DownloadProgress{ID: id, DoneBytes: 5, TotalBytes: 1000, BytesPerSecond: 1})
		}
		q.flush()
		time.Sleep(20 * time.Millisecond)
	}

	assert.Zero(t, web.OpenStreams(q.server), "the stream was dropped when a write timed out")
}
