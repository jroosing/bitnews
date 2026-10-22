package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
)

// None of the event stream tests run in parallel: the goroutine-leak checks
// need a quiet process.

// sseEvent is one dispatched server-sent event, or a comment.
type sseEvent struct {
	Name    string
	Data    string
	Comment string
}

func (e sseEvent) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(e.Data), &m), e.Data)
	return m
}

// sseClient reads an event stream in the background.
type sseClient struct {
	resp   *http.Response
	events chan sseEvent
	cancel context.CancelFunc
}

func connectSSE(t *testing.T, base string, mods ...func(*http.Request)) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/api/v1/events", nil)
	require.NoError(t, err)
	for _, m := range mods {
		m(req)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	c := &sseClient{resp: resp, events: make(chan sseEvent, 4096), cancel: cancel}
	t.Cleanup(c.close)
	go c.read()
	return c
}

func (c *sseClient) read() {
	defer close(c.events)
	sc := bufio.NewScanner(c.resp.Body)
	sc.Buffer(nil, 1<<20)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Name != "" || cur.Data != "" {
				c.events <- cur
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, ":"):
			c.events <- sseEvent{Comment: strings.TrimSpace(line[1:])}
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data += strings.TrimPrefix(line, "data: ")
		}
	}
}

// next waits for the next event; ok is false when the stream ended.
func (c *sseClient) next(t *testing.T) (ev sseEvent, ok bool) {
	t.Helper()
	select {
	case ev, ok = <-c.events:
		return ev, ok
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return sseEvent{}, false
	}
}

func (c *sseClient) close() {
	c.cancel()
	_ = c.resp.Body.Close()
}

// fakeTicker stands in for the ping ticker.
type fakeTicker struct {
	mu        sync.Mutex
	intervals []time.Duration
	chans     []chan time.Time
	created   chan struct{}
}

func newFakeTicker() *fakeTicker { return &fakeTicker{created: make(chan struct{}, 16)} }

func (f *fakeTicker) New(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make(chan time.Time, 1)
	f.intervals = append(f.intervals, d)
	f.chans = append(f.chans, c)
	f.created <- struct{}{}
	return c, func() {}
}

// tick fires the first ticker created, waiting for the stream to make it.
func (f *fakeTicker) tick(t *testing.T) {
	t.Helper()
	select {
	case <-f.created:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never created its ticker")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chans[0] <- time.Now()
}

func withTicker(f *fakeTicker) envOption {
	return withConfig(func(c *api.Config) { c.NewTicker = f.New })
}

func TestEventsRequireAuthentication(t *testing.T) {
	e := newEnv(t)

	anonymous := e.do("GET", "/api/v1/events", "")
	badKey := e.do("GET", "/api/v1/events", "", withBearer("bn_nope"))

	assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
	assert.Equal(t, "unauthorized", errorOf(t, anonymous))
	assert.Equal(t, http.StatusUnauthorized, badKey.Code)
}

func TestEventStreamStartsWithHelloCarryingTheInstanceID(t *testing.T) {
	e := newEnv(t)
	base, _ := start(t, e)

	c := connectSSE(t, base, withBearer(e.newKey(core.APIKeyScopeRead)))

	assert.Equal(t, http.StatusOK, c.resp.StatusCode)
	assert.Equal(t, "text/event-stream", c.resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", c.resp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", c.resp.Header.Get("X-Accel-Buffering"))
	assert.Equal(t, "nosniff", c.resp.Header.Get("X-Content-Type-Options"))
	hello, ok := c.next(t)
	require.True(t, ok)
	assert.Equal(t, "hello", hello.Name)
	assert.Equal(t, "instance-1", hello.json(t)["instance_id"])
}

func TestEventStreamAcceptsASessionCookie(t *testing.T) {
	e := newEnv(t)
	base, _ := start(t, e)
	sess := e.login()

	c := connectSSE(t, base, withCookie(sess.cookie))

	hello, _ := c.next(t)
	assert.Equal(t, "hello", hello.Name)
}

func TestEventStreamDeliversEventsPublishedAfterConnecting(t *testing.T) {
	e := newEnv(t)
	base, _ := start(t, e)
	c := connectSSE(t, base, withBearer(e.newKey(core.APIKeyScopeRead)))
	hello, _ := c.next(t)
	require.Equal(t, "hello", hello.Name)
	d := core.Download{
		ID: "dl1", Name: "movie", SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, AddedAt: epoch, UpdatedAt: epoch,
	}
	published := []core.Event{
		core.DownloadAdded{Download: d},
		core.DownloadUpdated{Download: d},
		core.DownloadProgress{ID: "dl1", DoneBytes: 50, TotalBytes: 200, BytesPerSecond: 7},
		core.Fetched{ID: "dl1"},
		core.Ready{ID: "dl1", OutputPath: "/data/out"},
		core.Failed{ID: "dl1", Reason: core.FailureMissingArticles, Detail: "12 articles"},
		core.Removed{ID: "dl1"},
		core.SettingsChanged{},
		core.ServersChanged{},
	}

	for _, ev := range published {
		e.bus.Publish(ev)
	}

	var got []sseEvent
	for range published {
		ev, ok := c.next(t)
		require.True(t, ok)
		got = append(got, ev)
	}
	names := make([]string, 0, len(got))
	for _, ev := range got {
		names = append(names, ev.Name)
	}
	assert.Equal(t, []string{
		"download_added", "download_updated", "download_progress", "fetched", "ready", "failed", "removed", "settings_changed", "servers_changed",
	}, names)
	for _, i := range []int{0, 1} {
		snap := got[i].json(t)
		assert.Equal(t, "dl1", snap["id"])
		assert.Equal(t, "movie", snap["name"])
		assert.Equal(t, "queued", snap["state"])
		assert.Equal(t, "normal", snap["priority"])
	}
	assert.Equal(t, map[string]any{"id": "dl1", "done_bytes": 50.0, "total_bytes": 200.0, "bytes_per_second": 7.0}, got[2].json(t))
	assert.Equal(t, map[string]any{"id": "dl1"}, got[3].json(t))
	assert.Equal(t, map[string]any{"id": "dl1", "output_path": "/data/out"}, got[4].json(t))
	assert.Equal(t, map[string]any{"id": "dl1", "reason": "missing_articles", "detail": "12 articles"}, got[5].json(t))
	assert.Equal(t, map[string]any{"id": "dl1"}, got[6].json(t))
	assert.Equal(t, map[string]any{}, got[7].json(t), "settings_changed carries no payload but must still have a data line")
	assert.Equal(t, map[string]any{}, got[8].json(t), "servers_changed carries no payload (passwords stay out of the stream)")
}

func TestEventStreamDeliversPostProcessingAndQueueEvents(t *testing.T) {
	e := newEnv(t)
	base, _ := start(t, e)
	c := connectSSE(t, base, withBearer(e.newKey(core.APIKeyScopeRead)))
	hello, _ := c.next(t)
	require.Equal(t, "hello", hello.Name)

	e.bus.Publish(core.PostProcessStage{ID: "dl1", Stage: core.StageRepairing})
	e.bus.Publish(runner.QueuePaused{Reason: runner.ReasonDiskFull, Detail: "10 bytes free, 20 needed"})
	e.bus.Publish(runner.QueueResumed{})

	stage, _ := c.next(t)
	paused, _ := c.next(t)
	resumed, _ := c.next(t)
	assert.Equal(t, "postprocess_stage", stage.Name)
	assert.Equal(t, map[string]any{"id": "dl1", "stage": "repairing"}, stage.json(t))
	assert.Equal(t, "usenet_queue_paused", paused.Name)
	assert.Equal(t, map[string]any{"reason": "disk_full", "detail": "10 bytes free, 20 needed"}, paused.json(t))
	assert.Equal(t, "usenet_queue_resumed", resumed.Name)
	assert.Equal(t, map[string]any{"reason": "", "detail": ""}, resumed.json(t))
}

func TestEventStreamDoesNotReplayEventsFromBeforeConnectingOrHonourLastEventID(t *testing.T) {
	e := newEnv(t)
	base, _ := start(t, e)
	e.bus.Publish(core.Removed{ID: "old"})

	c := connectSSE(t, base, withBearer(e.newKey(core.APIKeyScopeRead)), withHeader("Last-Event-ID", "5"))
	hello, _ := c.next(t)
	e.bus.Publish(core.Removed{ID: "new"})
	next, _ := c.next(t)

	assert.Equal(t, "hello", hello.Name)
	assert.Equal(t, map[string]any{"id": "new"}, next.json(t))
}

func TestEventStreamSendsAPingCommentOnTheTickerEvery15Seconds(t *testing.T) {
	ticker := newFakeTicker()
	e := newEnv(t, withTicker(ticker))
	base, _ := start(t, e)
	c := connectSSE(t, base, withBearer(e.newKey(core.APIKeyScopeRead)))
	hello, _ := c.next(t)
	require.Equal(t, "hello", hello.Name)

	ticker.tick(t)
	ping, ok := c.next(t)

	require.True(t, ok)
	assert.Equal(t, "ping", ping.Comment)
	assert.Equal(t, []time.Duration{15 * time.Second}, ticker.intervals)
}

func TestEventStreamSurvivesTheServersWriteTimeout(t *testing.T) {
	ticker := newFakeTicker()
	e := newEnv(t, withTicker(ticker))
	ln, srv := listenWith(t, e, func(s *http.Server) {
		s.WriteTimeout = 150 * time.Millisecond
		s.ReadTimeout = 150 * time.Millisecond
	})
	_ = ln
	c := connectSSE(t, "http://"+srv.Addr, withBearer(e.newKey(core.APIKeyScopeRead)))
	hello, _ := c.next(t)
	require.Equal(t, "hello", hello.Name)

	time.Sleep(500 * time.Millisecond) // well past the write timeout
	ticker.tick(t)
	ping, ok := c.next(t)

	require.True(t, ok, "the stream was cut by the write timeout")
	assert.Equal(t, "ping", ping.Comment)
}

func TestOrdinaryRoutesStillHaveAWriteTimeout(t *testing.T) {
	e := newEnv(t)
	e.server.HandleForTest("GET /stall", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, "late")
	})
	_, srv := listenWith(t, e, func(s *http.Server) { s.WriteTimeout = 150 * time.Millisecond })

	resp, err := http.Get("http://" + srv.Addr + "/stall")

	if err == nil {
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.True(t, readErr != nil || len(body) == 0, "the write timeout must cut a normal route, got %q", body)
	}
}

// blockingWriter is a ResponseWriter whose writes wait on a gate, standing in
// for a client that has stopped reading.
type blockingWriter struct {
	header  http.Header
	gate    chan struct{}
	entered chan struct{} // closed when the first write starts
	once    sync.Once
	mu      sync.Mutex
	buf     strings.Builder
}

func (b *blockingWriter) Header() http.Header { return b.header }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Flush()              {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.gate
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *blockingWriter) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestEventStreamEndsWithResyncWhenTheClientFallsTooFarBehind(t *testing.T) {
	e := newEnv(t)
	w := &blockingWriter{header: http.Header{}, gate: make(chan struct{}), entered: make(chan struct{})}
	ctx := t.Context()
	req := httptest.NewRequest("GET", "/api/v1/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+e.newKey(core.APIKeyScopeRead))
	done := make(chan struct{})
	go func() {
		e.server.Handler().ServeHTTP(w, req)
		close(done)
	}()
	// The handler subscribes before it writes hello, so once it is stuck
	// writing hello to the stalled client the subscriber exists.
	<-w.entered

	for i := range core.SubscriberBuffer + 10 {
		e.bus.Publish(core.Removed{ID: core.ID(strings.Repeat("x", i%3+1))})
	}
	close(w.gate)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end after the bus closed the subscription")
	}
	out := w.String()
	assert.Contains(t, out, "event: hello")
	assert.True(t, strings.HasSuffix(out, "event: resync\ndata: {}\n\n"), "stream must end with resync, got tail %q", out[max(0, len(out)-80):])
}

// streamHandlers counts goroutines currently inside the event stream handler.
func streamHandlers() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Count(string(buf), "api.(*Server).events(")
}

func TestDisconnectedClientReleasesItsStreamAndSubscription(t *testing.T) {
	e := newEnv(t)
	base, _ := start(t, e)
	key := e.newKey(core.APIKeyScopeRead)
	var clients []*sseClient
	for range 5 {
		c := connectSSE(t, base, withBearer(key))
		hello, _ := c.next(t)
		require.Equal(t, "hello", hello.Name)
		clients = append(clients, c)
	}
	require.Equal(t, 5, streamHandlers())

	for _, c := range clients {
		c.close()
	}

	require.Eventually(t, func() bool { return streamHandlers() == 0 }, 5*time.Second, 5*time.Millisecond,
		"stream handlers still running after every client hung up")
	e.bus.Publish(core.SettingsChanged{}) // must not block or panic with no subscribers left
}

func TestShutdownEndsOpenEventStreams(t *testing.T) {
	e := newEnv(t)
	base, stop := start(t, e)
	c := connectSSE(t, base, withBearer(e.newKey(core.APIKeyScopeRead)))
	hello, _ := c.next(t)
	require.Equal(t, "hello", hello.Name)

	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()

	select {
	case err := <-stopped:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for an event stream that never hangs up")
	}
	_, ok := c.next(t)
	assert.False(t, ok, "the stream must end when the server shuts down")
}
