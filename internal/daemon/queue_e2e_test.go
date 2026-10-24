//go:build e2e

package daemon_test

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/queue"
	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
	"github.com/jroosing/bitnews/internal/web"
)

// uiStream is a client of /ui/events over real HTTP with the web client's
// session.
type uiStream struct {
	t      *testing.T
	events chan sseEvent
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *webClient) openStream(query string) *uiStream {
	w.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.base+"/ui/events"+query, nil)
	require.NoError(w.t, err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := w.c.Do(req)
	require.NoError(w.t, err)
	require.Equal(w.t, http.StatusOK, resp.StatusCode)
	s := &uiStream{t: w.t, events: make(chan sseEvent, 512), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.events)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var cur sseEvent
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Name != "" {
					cur.Data = strings.Join(data, "\n")
					s.events <- cur
				}
				cur, data = sseEvent{}, nil
			case strings.HasPrefix(line, "event: "):
				cur.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	w.t.Cleanup(func() { cancel(); <-s.done })
	return s
}

// waitFor reads events until one named name arrives.
func (s *uiStream) waitFor(name string, d time.Duration) sseEvent {
	s.t.Helper()
	timeout := time.After(d)
	for {
		select {
		case ev, ok := <-s.events:
			require.True(s.t, ok, "stream ended while waiting for %s", name)
			if ev.Name == name {
				return ev
			}
		case <-timeout:
			s.t.Fatalf("no %q event within %s", name, d)
		}
	}
}

// queueRow returns the table row of a Download from the queue fragment, or "".
func (w *webClient) queueRow(id core.ID) string {
	w.t.Helper()
	resp, body := w.do("GET", "/queue/fragment", nil)
	require.Equal(w.t, http.StatusOK, resp.StatusCode)
	m := regexp.MustCompile(`(?s)<tr id="dl-` + string(id) + `".*?</tr>`).FindString(body)
	return m
}

func (e *e2e) signedInWeb() *webClient {
	e.t.Helper()
	w := newWebClient(e.t, e.ts.URL)
	resp, body := w.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"correct horse battery staple"}})
	require.Equal(e.t, http.StatusSeeOther, resp.StatusCode, body)
	return w
}

func newReleaseFixture(t *testing.T, e *e2e) (*nntptest.Fixture, *fakeNNTP) {
	t.Helper()
	srv := e.addServer("main", 0)
	g := loadGolden(t, "explicit_uniform")
	files := g.files()
	fx := nntptest.BuildFixture(t, files, 8192)
	fx.Install(srv.Server)
	return &fx, srv
}

// The queue over real HTTP: a Download added through the API shows up in the
// fragment, the stream says so and says when it left, and pausing and
// resuming the Queue through the UI route holds and releases it.
func TestE2EWebQueueFollowsADownloadAndPausesTheQueue(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	w := e.signedInWeb()
	fx, _ := newReleaseFixture(t, e)
	stream := w.openStream("?rows=1")
	hello := stream.waitFor("hello", 5*time.Second)
	assert.Equal(t, "hello", hello.Name)
	assert.Contains(t, stream.waitFor("status", 5*time.Second).Data, `id="status-queue"`)

	resp, body := w.hx("POST", "/queues/usenet/pause", url.Values{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, "You paused this queue")
	stream.waitFor("refresh", 10*time.Second) // the runner's QueuePaused event

	added := e.addNZB("My.Show.S01E01", fx.NZB, "")
	stream.waitFor("refresh", 10*time.Second)
	row := w.queueRow(added.ID)
	require.NotEmpty(t, row, "the new download is in the queue")
	assert.Contains(t, row, "My.Show.S01E01")
	assert.Contains(t, row, "Queued")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, core.StateQueued, e.get(added.ID).State, "a paused queue starts nothing")

	resp, body = w.hx("POST", "/queues/usenet/resume", url.Values{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, "You paused this queue")
	e.waitState(added.ID, core.StateReady)

	require.Eventually(t, func() bool { return w.queueRow(added.ID) == "" }, 10*time.Second, 50*time.Millisecond, "a Ready download leaves the queue")
	stream.waitFor("refresh", 10*time.Second)
}

// Priority, order, Category and removal from the UI act on the real service.
func TestE2EWebQueueRowActionsAffectTheDownloads(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	w := e.signedInWeb()
	fx, _ := newReleaseFixture(t, e)
	tv := e.addCategory("tv")
	resp, _ := w.hx("POST", "/queues/usenet/pause", url.Values{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	a := e.addNZB("A.Release", fx.NZB, "")
	b := e.addNZB("B.Release", fx.NZB, "")

	resp, body := w.hx("POST", "/queue/"+string(b.ID)+"/move", url.Values{"to": {"top"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Less(t, strings.Index(body, string(b.ID)), strings.Index(body, string(a.ID)), "B is listed before A")

	resp, body = w.hx("POST", "/queue/"+string(a.ID)+"/priority", url.Values{"priority": {"force"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, "Force")

	resp, body = w.hx("POST", "/queue/"+string(a.ID)+"/category", url.Values{"category_id": {string(tv)}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	got := e.get(a.ID)
	require.NotNil(t, got.CategoryID)
	assert.Equal(t, tv, *got.CategoryID)

	resp, body = w.hx("POST", "/queue/"+string(a.ID)+"/pause", url.Values{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.True(t, e.get(a.ID).Paused)
	assert.Contains(t, body, "Paused")

	resp, body = w.hx("POST", "/queue/"+string(a.ID)+"/remove", url.Values{"delete_data": {"true"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	e.call("GET", "/api/v1/downloads/"+string(a.ID), nil, http.StatusNotFound, nil)

	resp, _ = w.hx("POST", "/queue/"+string(a.ID)+"/pause", url.Values{})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "gone is a toast, not a 500")
	resp, _ = w.hx("POST", "/queues/usenet/speed", url.Values{"down_kbps": {"1024"}})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	_, page := w.do("GET", "/queue", nil)
	assert.Contains(t, page, "1.0 MB/s", "the limit is shown")
	resp, _ = w.hx("POST", "/queues/torrent/pause", url.Values{})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "torrents are off in this daemon")
}

// The web package does not import the runners, so it names their queue
// events by string. This pins those names.
func TestWebQueueEventKindsMatchTheRunners(t *testing.T) {
	assert.Equal(t, queue.ReasonUser, web.PauseReasonUser)
	assert.Equal(t, queue.ReasonDiskFull, web.PauseReasonDiskFull)
	assert.Equal(t, queue.ReasonBindUnavailable, web.PauseReasonBindUnavailable)
}
