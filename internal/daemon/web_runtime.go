package daemon

import (
	"context"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/queue"
	trunner "gitlab.com/kyura4k/gitnews/internal/torrent/runner"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// queueControl is the pause side of a Queue; *queue.Queue implements it.
type queueControl interface {
	Pause(reason, detail string)
	Resume(reason string)
	Paused() (paused bool, reason, detail string)
}

// usenetLimiter is what webRuntime needs of the Usenet runner besides its
// Queue.
type usenetLimiter interface {
	SetSpeedLimit(bytesPerSecond int64)
}

// torrentRunner is what webRuntime needs of the torrent runner besides its
// Queue.
type torrentRunner interface {
	SetRateLimits(down, up int64)
	UploadRate() int64
	Facts(ctx context.Context, id core.ID) (trunner.Facts, bool, error)
}

// webRuntime implements web.RuntimeSource for the queue page: speeds and
// stages from the event tracker, Queue pause and speed limits through the
// runners, torrent facts through the torrent runner. Speed limits are not
// persisted: the runners start unlimited, and what the UI set is remembered
// here only to show it again.
type webRuntime struct {
	queues   map[core.SourceKind]queueControl // no torrent entry when torrents are off
	usenet   usenetLimiter
	torrents torrentRunner // nil when torrents are off
	track    *tracker

	mu     sync.Mutex
	limits map[core.SourceKind][2]int64
}

var _ web.RuntimeSource = (*webRuntime)(nil)

// newWebRuntime builds the source. torrents may be nil, and then queues has
// no torrent entry.
func newWebRuntime(queues map[core.SourceKind]queueControl, usenet usenetLimiter, torrents torrentRunner, track *tracker) *webRuntime {
	return &webRuntime{queues: queues, usenet: usenet, torrents: torrents, track: track, limits: map[core.SourceKind][2]int64{}}
}

// Rates sums the download rates the tracker follows and the upload rates of
// the torrents in the engine.
func (w *webRuntime) Rates() (down, up int64) {
	if w.torrents != nil {
		up = w.torrents.UploadRate()
	}
	return w.track.speed(), up
}

func (w *webRuntime) QueueState(kind core.SourceKind) web.QueueState {
	q, ok := w.queues[kind]
	if !ok {
		return web.QueueState{}
	}
	paused, reason, _ := q.Paused()
	return web.QueueState{Available: true, Paused: paused, Reason: reason}
}

func (w *webRuntime) PauseQueue(kind core.SourceKind) {
	if q, ok := w.queues[kind]; ok {
		q.Pause(queue.ReasonUser, "")
	}
}

func (w *webRuntime) ResumeQueue(kind core.SourceKind) {
	if q, ok := w.queues[kind]; ok {
		q.Resume(queue.ReasonUser)
	}
}

func (w *webRuntime) SpeedLimits(kind core.SourceKind) (down, up int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	l := w.limits[kind]
	return l[0], l[1]
}

func (w *webRuntime) SetSpeedLimit(kind core.SourceKind, down, up int64) {
	down, up = max(down, 0), max(up, 0)
	switch {
	case kind == core.SourceNZB:
		up = 0 // the Usenet Queue only receives
		w.usenet.SetSpeedLimit(down)
	case kind == core.SourceTorrent && w.torrents != nil:
		w.torrents.SetRateLimits(down, up)
	default:
		return
	}
	w.mu.Lock()
	w.limits[kind] = [2]int64{down, up}
	w.mu.Unlock()
}

func (w *webRuntime) Live(id core.ID) web.DownloadLive {
	l := web.DownloadLive{DownRate: w.track.rate(id)}
	l.Stage, _ = w.track.stage(id)
	if w.torrents == nil {
		return l
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	f, ok, err := w.torrents.Facts(ctx, id)
	if err != nil || !ok || !f.Live {
		return l
	}
	l.Torrent, l.UpRate, l.Peers, l.Seeds, l.Ratio = true, f.UpRate, f.Peers, f.Seeds, f.Ratio
	return l
}
