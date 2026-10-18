package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// streamWriteTimeout bounds each single write to an event stream, so a client
// that stopped reading cannot hold its handler (and subscription) forever.
// The server-wide write timeout does not apply to the stream; this replaces
// it, per write.
const streamWriteTimeout = 30 * time.Second

// events serves GET /api/v1/events as server-sent events:
//
//	event: hello            {"instance_id": "..."}   first, once
//	event: <kind>           one of the core event kinds, with its payload
//	: ping                  a comment, every PingInterval
//	event: resync           the client missed events: refetch state; the stream ends
//
// Events are not persisted, so there is nothing to replay: Last-Event-ID is
// ignored and a client refetches its state after (re)connecting. The stream
// ends when the client disconnects, when the server shuts down, or after
// resync.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	// The request context ends when the client goes away; s.streams ends it
	// on server shutdown too, even when the request came in through a server
	// that was not built by HTTPServer.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(s.streams, cancel)()

	// Subscribe before the first byte goes out: once a client has seen hello,
	// it misses no event.
	sub := s.Bus.Subscribe(ctx)
	defer sub.Close()
	ticks, stopTicker := s.NewTicker(s.PingInterval)
	defer stopTicker()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // tell nginx not to buffer the stream
	w.WriteHeader(http.StatusOK)

	send := func(format string, args ...any) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !sendEvent(send, "hello", map[string]string{"instance_id": s.InstanceID}) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if !send(": ping\n\n") {
				return
			}
		case ev, ok := <-sub.Events():
			if !ok {
				if errors.Is(sub.Err(), core.ErrResync) {
					sendEvent(send, "resync", struct{}{})
				}
				return
			}
			name, payload, known := eventPayload(ev)
			if !known {
				logFrom(ctx).Debug("event stream: skipping unmapped event", "kind", ev.Kind())
				continue
			}
			if !sendEvent(send, name, payload) {
				return
			}
		}
	}
}

// sendEvent writes one event with a JSON data line. JSON has no raw
// newlines, so one data line always holds the whole payload. Every event has
// a data line, even an empty object: a browser's EventSource drops events
// without one.
func sendEvent(send func(string, ...any) bool, name string, payload any) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	return send("event: %s\ndata: %s\n\n", name, data)
}

// eventPayload maps a core event to its SSE name and JSON payload. Settings
// changes carry no payload: receivers fetch the settings.
func eventPayload(ev core.Event) (name string, payload any, ok bool) {
	name = string(ev.Kind())
	switch e := ev.(type) {
	case core.DownloadAdded:
		return name, toDownloadJSON(e.Download), true
	case core.DownloadUpdated:
		return name, toDownloadJSON(e.Download), true
	case core.DownloadProgress:
		return name, struct {
			ID             core.ID `json:"id"`
			DoneBytes      int64   `json:"done_bytes"`
			TotalBytes     int64   `json:"total_bytes"`
			BytesPerSecond int64   `json:"bytes_per_second"`
		}{e.ID, e.DoneBytes, e.TotalBytes, e.BytesPerSecond}, true
	case core.Fetched:
		return name, idPayload{e.ID}, true
	case core.Ready:
		return name, struct {
			ID         core.ID `json:"id"`
			OutputPath string  `json:"output_path"`
		}{e.ID, e.OutputPath}, true
	case core.Failed:
		return name, struct {
			ID     core.ID            `json:"id"`
			Reason core.FailureReason `json:"reason"`
			Detail string             `json:"detail"`
		}{e.ID, e.Reason, e.Detail}, true
	case core.Removed:
		return name, idPayload{e.ID}, true
	case core.SettingsChanged, core.ServersChanged:
		return name, struct{}{}, true
	}
	return "", nil, false
}

type idPayload struct {
	ID core.ID `json:"id"`
}
