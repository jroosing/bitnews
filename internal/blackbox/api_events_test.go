package blackbox_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// frame is one dispatched server-sent event.
type frame struct {
	name string
	data string
}

// json decodes the data line as an object.
func (f frame) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.data), &m), "event %s: %q", f.name, f.data)
	return m
}

// stream is an open GET /api/v1/events. A goroutine reads and parses the
// frames, so a frame split across reads is put together and the test code
// can wait with a timeout.
type stream struct {
	t      *testing.T
	resp   *http.Response
	frames chan frame
	cancel context.CancelFunc
}

// eventWait is how long a test waits for a frame before it fails. Events
// are published synchronously by the request that causes them, so they are
// there within milliseconds; the margin is for a loaded machine.
const eventWait = 5 * time.Second

// quietFor is how long requireQuiet waits to see that nothing else comes.
const quietFor = 250 * time.Millisecond

// openStream connects to the event stream and returns once the response
// headers have arrived.
func (d *daemon) openStream() *stream {
	d.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.addr+"/api/v1/events", nil)
	require.NoError(d.t, err)
	req.Header.Set("Authorization", "Bearer "+d.cfg.AppKey)
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	require.NoError(d.t, err)
	s := &stream{t: d.t, resp: resp, frames: make(chan frame, 1024), cancel: cancel}
	d.t.Cleanup(s.close)
	go s.read()
	return s
}

// openStreamAfterHello opens the stream and consumes the hello frame, so
// every event caused afterwards is seen.
func (d *daemon) openStreamAfterHello() *stream {
	d.t.Helper()
	s := d.openStream()
	hello := s.next()
	require.Equal(d.t, "hello", hello.name)
	return s
}

func (s *stream) close() {
	s.cancel()
	_ = s.resp.Body.Close()
}

// read parses the body into frames until it ends. Comment lines (": ping")
// are dropped.
func (s *stream) read() {
	defer close(s.frames)
	br := bufio.NewReader(s.resp.Body)
	var cur frame
	var have bool
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if have {
				s.frames <- cur
			}
			cur, have = frame{}, false
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			cur.name, have = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " "), true
		case strings.HasPrefix(line, "data:"):
			cur.data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			have = true
		}
	}
}

// next waits for the next frame.
func (s *stream) next() frame {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		require.True(s.t, ok, "the event stream ended")
		return f
	case <-time.After(eventWait):
		s.t.Fatal("timed out waiting for an event")
		return frame{}
	}
}

// take returns the next n frames.
func (s *stream) take(n int) []frame {
	s.t.Helper()
	out := make([]frame, 0, n)
	for range n {
		out = append(out, s.next())
	}
	return out
}

// requireQuiet asserts that no frame arrives for a short while.
func (s *stream) requireQuiet() {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			s.t.Fatalf("unexpected event %s %s", f.name, f.data)
		}
		s.t.Fatal("the event stream ended")
	case <-time.After(quietFor):
	}
}

// names returns the event names of frames.
func names(fs []frame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.name
	}
	return out
}

func TestAPIEventsAnswerWithTheStreamHeadersAndHelloFirst(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	s := d.openStream()

	assert.Equal(t, http.StatusOK, s.resp.StatusCode)
	assert.Equal(t, "text/event-stream", s.resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", s.resp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", s.resp.Header.Get("X-Accel-Buffering"))
	assert.Equal(t, "nosniff", s.resp.Header.Get("X-Content-Type-Options"))
	assert.NotEmpty(t, s.resp.Header.Get("X-Request-Id"))
	hello := s.next()
	assert.Equal(t, "hello", hello.name)
	m := hello.json(t)
	require.Len(t, m, 1)
	id, ok := m["instance_id"].(string)
	assert.True(t, ok && id != "", "instance_id: %v", m["instance_id"])
	s.requireQuiet()
}

func TestAPIEventsHelloCarriesTheSameInstanceIDOnEveryConnection(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	first := d.openStream().next().json(t)["instance_id"]
	second := d.openStream().next().json(t)["instance_id"]

	assert.Equal(t, first, second, "the ID names the process, not the connection")
}

func TestAPIEventsNeedTheKeyAndNothingElse(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)

	none := d.get("/api/v1/events", nil)
	wrong := d.get("/api/v1/events", bearer(newAppKey(t)))
	post := d.request(http.MethodPost, "/api/v1/events", bearer(d.cfg.AppKey), "")

	assert.Equal(t, http.StatusUnauthorized, none.status)
	assert.Equal(t, errUnauthorized, none.body)
	assert.Equal(t, http.StatusUnauthorized, wrong.status)
	assert.Equal(t, http.StatusMethodNotAllowed, post.status)
	assert.Equal(t, "GET, HEAD", post.header.Get("Allow"))
}

func TestAPIEventsEveryConnectionSeesEveryEvent(t *testing.T) {
	t.Parallel()
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, newDownload("one")) })
	a, b := d.openStreamAfterHello(), d.openStreamAfterHello()
	id := itemsOf(t, d.apiGet("/api/v1/downloads"))[0]["id"].(string)

	requireOK(t, d.api(http.MethodPatch, "/api/v1/downloads/"+id, `{"priority":"low"}`))

	for _, s := range []*stream{a, b} {
		f := s.next()
		assert.Equal(t, "download_updated", f.name)
		assert.Equal(t, "low", f.json(t)["priority"])
		s.requireQuiet()
	}
}

func TestAPIEventsDownloadUpdatedCarriesTheWholeDownload(t *testing.T) {
	t.Parallel()
	dl := newDownload("movie", at(7))
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, dl) })
	s := d.openStreamAfterHello()

	r := d.api(http.MethodPatch, "/api/v1/downloads/"+string(dl.ID), `{"priority":"high"}`)
	requireOK(t, r)

	f := s.next()
	assert.Equal(t, "download_updated", f.name)
	// The event data is the same document as the answer of the PATCH.
	assert.JSONEq(t, r.body, f.data)
	assert.Equal(t, "high", f.json(t)["priority"])
	assert.Equal(t, string(dl.ID), f.json(t)["id"])
	s.requireQuiet()
}

func TestAPIEventsPauseAndResumeAreAnnounced(t *testing.T) {
	t.Parallel()
	dl := newDownload("movie")
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, dl) })
	s := d.openStreamAfterHello()

	requireOK(t, d.api(http.MethodPost, "/api/v1/downloads/"+string(dl.ID)+"/pause", ""))
	paused := s.next()
	requireOK(t, d.api(http.MethodPost, "/api/v1/downloads/"+string(dl.ID)+"/pause", ""))
	requireOK(t, d.api(http.MethodPost, "/api/v1/downloads/"+string(dl.ID)+"/resume", ""))
	resumed := s.next()

	assert.Equal(t, "download_updated", paused.name)
	assert.Equal(t, true, paused.json(t)["paused"])
	assert.Equal(t, "download_updated", resumed.name)
	assert.Equal(t, false, resumed.json(t)["paused"])
	s.requireQuiet() // pausing a paused Download announced nothing
}

func TestAPIEventsRemovedCarriesOnlyTheID(t *testing.T) {
	t.Parallel()
	dl := newDownload("movie")
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, dl) })
	s := d.openStreamAfterHello()

	requireNoContent(t, d.api(http.MethodDelete, "/api/v1/downloads/"+string(dl.ID), ""))

	f := s.next()
	assert.Equal(t, "removed", f.name)
	assert.JSONEq(t, `{"id":"`+string(dl.ID)+`"}`, f.data)
	s.requireQuiet()
}

func TestAPIEventsSettingsChangedHasAnEmptyObjectAsData(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	s := d.openStreamAfterHello()
	cur := d.apiGet("/api/v1/settings")
	requireOK(t, cur)

	requireOK(t, d.api(http.MethodPut, "/api/v1/settings", cur.body))

	f := s.next()
	assert.Equal(t, "settings_changed", f.name)
	assert.Equal(t, "{}", f.data, "an event always has a data line")
	s.requireQuiet()
}

func TestAPIEventsAFailedSettingsChangeAnnouncesNothing(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	s := d.openStreamAfterHello()

	requireError(t, d.api(http.MethodPut, "/api/v1/settings", `{}`), http.StatusBadRequest, "invalid_settings")

	s.requireQuiet()
}

func TestAPIEventsServerChangesAreAnnouncedAsServersChanged(t *testing.T) {
	t.Parallel()
	d := startAPI(t, nil)
	s := d.openStreamAfterHello()

	created := d.api(http.MethodPost, "/api/v1/servers", `{"name":"two","host":"two.example.net"}`)
	require.Equal(t, http.StatusCreated, created.status, created.body)
	id := obj(t, created)["id"].(string)
	requireOK(t, d.api(http.MethodPatch, "/api/v1/servers/"+id, `{"priority":3}`))
	requireNoContent(t, d.api(http.MethodDelete, "/api/v1/servers/"+id, ""))

	for _, f := range s.take(3) {
		assert.Equal(t, "servers_changed", f.name)
		assert.Equal(t, "{}", f.data)
	}
	s.requireQuiet()
}

func TestAPIEventsCategoryChangesAnnounceNothingButDeletingOneAnnouncesItsDownloads(t *testing.T) {
	t.Parallel()
	var cat core.Category
	dl := newDownload("tagged")
	other := newDownload("untagged", at(1))
	d := startAPI(t, func(c configDir) {
		cat = addCategory(t, c, "anime")
		dl.CategoryID = cat.ID
		seedDownloads(t, c, dl, other)
	})
	s := d.openStreamAfterHello()

	created := d.api(http.MethodPost, "/api/v1/categories", `{"name":"docs"}`)
	require.Equal(t, http.StatusCreated, created.status, created.body)
	requireOK(t, d.api(http.MethodPatch, "/api/v1/categories/"+string(cat.ID), `{"name":"anime2"}`))
	s.requireQuiet()
	requireNoContent(t, d.api(http.MethodDelete, "/api/v1/categories/"+string(cat.ID), ""))

	f := s.next()
	assert.Equal(t, "download_updated", f.name)
	m := f.json(t)
	assert.Equal(t, string(dl.ID), m["id"])
	assert.Nil(t, m["category_id"], "the event shows the Download without its Category")
	s.requireQuiet()
}

func TestAPIEventsQueueMovesAnnounceEveryDownloadWhosePositionChanged(t *testing.T) {
	t.Parallel()
	a := newDownload("a", at(0), position(0))
	b := newDownload("b", at(1), position(1))
	c := newDownload("c", at(2), position(2))
	d := startAPI(t, func(cfg configDir) { seedDownloads(t, cfg, a, b, c) })
	s := d.openStreamAfterHello()

	requireOK(t, d.api(http.MethodPatch, "/api/v1/downloads/"+string(c.ID), `{"queue_position":{"move":"top"}}`))

	fs := s.take(3)
	var got []string
	pos := map[string]float64{}
	for _, f := range fs {
		assert.Equal(t, "download_updated", f.name)
		m := f.json(t)
		got = append(got, m["name"].(string))
		pos[m["name"].(string)] = m["queue_position"].(float64)
	}
	assert.Equal(t, []string{"c", "a", "b"}, got, "the moved Download first, then the others in queue order")
	assert.Equal(t, map[string]float64{"c": 0, "a": 1, "b": 2}, pos)
	s.requireQuiet()
}

func TestAPIEventsAMoveThatChangesNothingAnnouncesNothing(t *testing.T) {
	t.Parallel()
	a := newDownload("a", at(0), position(0))
	b := newDownload("b", at(1), position(1))
	d := startAPI(t, func(cfg configDir) { seedDownloads(t, cfg, a, b) })
	s := d.openStreamAfterHello()

	requireOK(t, d.api(http.MethodPatch, "/api/v1/downloads/"+string(a.ID), `{"queue_position":{"move":"up"}}`))

	s.requireQuiet()
}

func TestAPIEventsAnEventSplitAcrossManyWritesIsStillOneFrame(t *testing.T) {
	t.Parallel()
	// A name of 100 KB makes the data line larger than any single read of a
	// socket buffer, so the frame reaches the reader in pieces.
	long := strings.Repeat("n", 100_000)
	dl := newDownload(long)
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, dl) })
	s := d.openStreamAfterHello()

	requireOK(t, d.api(http.MethodPatch, "/api/v1/downloads/"+string(dl.ID), `{"priority":"force"}`))

	f := s.next()
	assert.Equal(t, "download_updated", f.name)
	assert.Equal(t, long, f.json(t)["name"])
	s.requireQuiet()
}

func TestAPIEventsAClientThatLeavesDoesNotDisturbTheOthers(t *testing.T) {
	t.Parallel()
	dl := newDownload("movie")
	d := startAPI(t, func(c configDir) { seedDownloads(t, c, dl) })
	gone := d.openStreamAfterHello()
	stays := d.openStreamAfterHello()
	gone.close()
	time.Sleep(50 * time.Millisecond)

	requireOK(t, d.api(http.MethodPatch, "/api/v1/downloads/"+string(dl.ID), `{"priority":"low"}`))

	assert.Equal(t, "download_updated", stays.next().name)
	requireOK(t, d.apiGet("/api/v1/downloads"))
}
