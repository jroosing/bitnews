package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// GET /ui/events is the UI's live stream: server-sent events whose data is
// server-rendered HTML for htmx's SSE extension, not the JSON of the API's
// /api/v1/events.
//
//	hello     once, first
//	status    out-of-band spans for the top bar (speeds, queue state); sent
//	          first, then whenever they change
//	live      (queue view only) out-of-band cells of the rows whose progress
//	          or stage changed: status badge, progress, speed and ETA
//	removed   (queue view only) out-of-band deletes of removed rows
//	refresh   (queue view only) the lists changed shape: refetch /queue/fragment
//	resync    the stream missed events and ends: the client reconnects, and on
//	          every (re)connect refetches the lists (queue.js)
//	: ping    a comment, as a heartbeat
//
// Bus events are only noted when they arrive. Once per Tick the batch is
// rendered: at most one update per Download per Tick however fast progress
// events come, and at most one refresh. The batch is bounded, so a flood
// cannot grow a connection's memory; overflowing it asks for a refresh.

// LiveConfig tunes the stream. The zero value is what production uses.
type LiveConfig struct {
	// Tick is how often pending updates are rendered and sent (1 s).
	Tick time.Duration
	// Ping is the heartbeat period (15 s).
	Ping time.Duration
	// WriteTimeout bounds each single write; a client that does not read
	// within it is dropped (10 s).
	WriteTimeout time.Duration
	// MaxStreams caps the open streams (64); more get 503.
	MaxStreams int
	// NewTicker makes the tickers. Tests replace it with a fake.
	NewTicker func(d time.Duration) (c <-chan time.Time, stop func())
}

func (c LiveConfig) withDefaults() LiveConfig {
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
	if c.Ping <= 0 {
		c.Ping = 15 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = 64
	}
	if c.NewTicker == nil {
		c.NewTicker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	return c
}

// maxPending bounds the Downloads one batch tracks. Beyond it the batch just
// asks for a refresh of the lists.
const maxPending = 256

// The kinds of the runners' queue events. The web package does not import
// the runners; the daemon tests pin these to the runners' constants.
const (
	KindUsenetQueuePaused   core.EventKind = "usenet_queue_paused"
	KindUsenetQueueResumed  core.EventKind = "usenet_queue_resumed"
	KindTorrentQueuePaused  core.EventKind = "torrent_queue_paused"
	KindTorrentQueueResumed core.EventKind = "torrent_queue_resumed"
)

func (s *Server) registerLive() {
	s.live.cfg = s.Live.withDefaults()
	s.handleAuthed("GET /ui/events", s.getLiveEvents)
}

// liveHub is the per-server state of the streams.
type liveHub struct {
	cfg  LiveConfig
	open atomic.Int32
}

// batch collects what happened since the last Tick.
type batch struct {
	progress map[core.ID]core.DownloadProgress
	stages   map[core.ID]core.Stage
	removed  []core.ID
	refresh  bool
}

func newBatch() *batch {
	return &batch{progress: map[core.ID]core.DownloadProgress{}, stages: map[core.ID]core.Stage{}}
}

// note records an event. It does no I/O.
func (b *batch) note(ev core.Event) {
	switch e := ev.(type) {
	case core.DownloadProgress:
		if _, ok := b.progress[e.ID]; !ok && len(b.progress) >= maxPending {
			b.refresh = true
			return
		}
		b.progress[e.ID] = e
	case core.PostProcessStage:
		if _, ok := b.stages[e.ID]; !ok && len(b.stages) >= maxPending {
			b.refresh = true
			return
		}
		b.stages[e.ID] = e.Stage
	case core.DownloadAdded, core.DownloadUpdated, core.Fetched, core.Ready, core.Failed, core.QueuePaused, core.QueueResumed:
		b.refresh = true
	case core.Removed:
		b.refresh = true
		delete(b.progress, e.ID)
		delete(b.stages, e.ID)
		if len(b.removed) < maxPending {
			b.removed = append(b.removed, e.ID)
		}
	default:
		switch ev.Kind() {
		case KindUsenetQueuePaused, KindUsenetQueueResumed, KindTorrentQueuePaused, KindTorrentQueueResumed:
			b.refresh = true
		}
	}
}

func (b *batch) empty() bool {
	return !b.refresh && len(b.progress) == 0 && len(b.stages) == 0 && len(b.removed) == 0
}

// getLiveEvents serves the stream.
func (s *Server) getLiveEvents(w http.ResponseWriter, r *http.Request) {
	cfg := s.live.cfg
	if s.Bus == nil {
		http.Error(w, "live updates are not available", http.StatusServiceUnavailable)
		return
	}
	if int(s.live.open.Add(1)) > cfg.MaxStreams {
		s.live.open.Add(-1)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many open streams", http.StatusServiceUnavailable)
		return
	}
	defer s.live.open.Add(-1)

	ctx := r.Context()
	rows := r.URL.Query().Get("rows") == "1"
	rc := http.NewResponseController(w)

	// Subscribe before the first byte goes out: after hello nothing is missed.
	sub := s.Bus.Subscribe(ctx)
	defer sub.Close()
	ticks, stopTick := cfg.NewTicker(cfg.Tick)
	defer stopTick()
	pings, stopPing := cfg.NewTicker(cfg.Ping)
	defer stopPing()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(format string, args ...any) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(name, data string) bool { return write("%s", sseFrame(name, data)) }

	if !send("hello", "ok") {
		return
	}
	var last liveStatus
	b := newBatch()
	// The first flush sends the status; the lists are refetched by the
	// client when the stream opens, so nothing else is due.
	if !s.flushLive(ctx, b, rows, &last, send) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-pings:
			if !write(": ping\n\n") {
				return
			}
		case <-ticks:
			if !s.flushLive(ctx, b, rows, &last, send) {
				return
			}
		case ev, ok := <-sub.Events():
			if !ok {
				if errors.Is(sub.Err(), core.ErrResync) {
					send("resync", "ok")
				}
				return
			}
			if rows {
				b.note(ev)
			}
		}
	}
}

// sseFrame formats one event. Every line of data gets its own "data:" field.
func sseFrame(name, data string) string {
	var sb strings.Builder
	sb.WriteString("event: " + name + "\n")
	data = strings.ReplaceAll(data, "\r", "")
	for line := range strings.SplitSeq(data, "\n") {
		sb.WriteString("data: " + line + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// liveStatus is the top bar's content.
type liveStatus struct {
	Down, Up string
	Badge    string
	Label    string
}

// flushLive renders and sends the pending batch and the status when it
// changed. It returns false when a write failed.
func (s *Server) flushLive(ctx context.Context, b *batch, rows bool, last *liveStatus, send func(name, data string) bool) bool {
	if rows && !b.empty() {
		pending := *b
		*b = *newBatch()
		if len(pending.removed) > 0 && !send("removed", removedFragments(pending.removed)) {
			return false
		}
		if frag := s.liveCells(ctx, pending); frag != "" && !send("live", frag) {
			return false
		}
		if pending.refresh && !send("refresh", "ok") {
			return false
		}
	}
	st := s.currentStatus(ctx)
	if st != *last {
		out, err := s.tpl.execute("queue.html", "live_status_oob", st)
		if err != nil {
			s.Logger.Error("live status render failed", "err", err)
			return true
		}
		if !send("status", string(out)) {
			return false
		}
		*last = st
	}
	return true
}

// removedFragments deletes rows out of band.
func removedFragments(ids []core.ID) string {
	var sb strings.Builder
	for _, id := range ids {
		if idValue.MatchString(string(id)) {
			fmt.Fprintf(&sb, `<div id="dl-%s" hx-swap-oob="delete"></div>`, id)
		}
	}
	return sb.String()
}

// liveCells renders the changed cells of the rows in the batch.
func (s *Server) liveCells(ctx context.Context, b batch) string {
	if s.Downloads == nil {
		return ""
	}
	ids := make([]core.ID, 0, len(b.progress)+len(b.stages))
	for id := range b.progress {
		ids = append(ids, id)
	}
	for id := range b.stages {
		if _, ok := b.progress[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var sb strings.Builder
	for _, id := range ids {
		if !idValue.MatchString(string(id)) {
			continue
		}
		d, err := s.Downloads.Get(ctx, id)
		if err != nil {
			continue // gone: the refresh that follows a removal covers it
		}
		if !slices.Contains(queueStates, d.State) {
			continue
		}
		var live DownloadLive
		if s.Runtime != nil {
			live = s.Runtime.Live(id)
		}
		if p, ok := b.progress[id]; ok {
			d.DoneBytes = p.DoneBytes
			if p.TotalBytes > 0 {
				d.TotalBytes = p.TotalBytes
			}
			live.DownRate = p.BytesPerSecond
		}
		if st, ok := b.stages[id]; ok {
			live.Stage = st
		}
		out, err := s.tpl.execute("queue.html", "queue_live", queueRowCtx{queueRow: newQueueRow(d, live, ""), OOB: true})
		if err != nil {
			s.Logger.Error("live row render failed", "err", err)
			continue
		}
		sb.Write(out)
	}
	return sb.String()
}

// currentStatus summarises the daemon for the top bar.
func (s *Server) currentStatus(ctx context.Context) liveStatus {
	var down, up int64
	allPaused := false
	if s.Runtime != nil {
		down, up = s.Runtime.Rates()
		allPaused = true
		any := false
		for _, k := range []core.SourceKind{core.SourceNZB, core.SourceTorrent} {
			st := s.Runtime.QueueState(k)
			if !st.Available {
				continue
			}
			any = true
			allPaused = allPaused && st.Paused
		}
		allPaused = allPaused && any
	}
	st := liveStatus{Down: formatRateZero(down), Up: formatRateZero(up)}
	switch {
	case allPaused:
		st.Badge, st.Label = "paused", "Paused"
	case down > 0 || up > 0 || s.anyIn(ctx, core.StateFetching, core.StatePostProcessing):
		st.Badge, st.Label = "downloading", "Active"
	default:
		st.Badge, st.Label = "queued", "Idle"
	}
	return st
}

// anyIn reports whether a Download is in one of the states.
func (s *Server) anyIn(ctx context.Context, states ...core.State) bool {
	if s.Downloads == nil {
		return false
	}
	for _, st := range states {
		page, err := s.Downloads.List(ctx, app.ListFilter{State: st, Limit: 1})
		if err == nil && len(page.Items) > 0 {
			return true
		}
	}
	return false
}

func formatRateZero(bytesPerSecond int64) string {
	if bytesPerSecond <= 0 {
		return "0 B/s"
	}
	return formatRate(bytesPerSecond)
}
