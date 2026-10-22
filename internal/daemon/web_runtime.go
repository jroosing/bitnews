package daemon

import (
	"context"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
	trunner "gitlab.com/kyura4k/gitnews/internal/torrent/runner"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// usenetQueue is what webRuntime needs of the Usenet runner.
type usenetQueue interface {
	PauseQueue()
	ResumeQueue()
	QueuePaused() (paused bool, reason string)
	SetSpeedLimit(bytesPerSecond int64)
}

// torrentQueue is what webRuntime needs of the torrent runner.
type torrentQueue interface {
	PauseQueue()
	ResumeQueue()
	QueuePaused() (paused bool, reason string)
	SetRateLimits(down, up int64)
	Info(id core.ID) (trunner.Info, bool)
}

var (
	_ usenetQueue  = (*runner.Runner)(nil)
	_ torrentQueue = (*trunner.Runner)(nil)
)

// webRuntime implements web.RuntimeSource for the queue page: speeds and
// stages from the event tracker, Queue pause and speed limits through the
// runners, torrent facts through the torrent runner and the engine. Speed
// limits are not persisted: the runners start unlimited, and what the UI set
// is remembered here only to show it again.
type webRuntime struct {
	usenet    usenetQueue
	torrents  torrentQueue // nil when torrents are off
	engine    core.TorrentEngine
	downloads core.DownloadRepo
	track     *tracker
	now       func() time.Time

	mu       sync.Mutex
	limits   map[core.SourceKind][2]int64
	upAt     time.Time
	upCached int64
}

var _ web.RuntimeSource = (*webRuntime)(nil)

// newWebRuntime builds the source. torrents and engine may be nil.
func newWebRuntime(usenet usenetQueue, torrents torrentQueue, engine core.TorrentEngine, downloads core.DownloadRepo, track *tracker) *webRuntime {
	return &webRuntime{
		usenet: usenet, torrents: torrents, engine: engine, downloads: downloads, track: track,
		now: time.Now, limits: map[core.SourceKind][2]int64{},
	}
}

// Rates sums the download rates the tracker follows and the upload rates of
// the torrents in the engine. The upload sum needs a database read and an
// engine call per torrent, so it is cached for a second.
func (w *webRuntime) Rates() (down, up int64) {
	return w.track.speed(), w.uploadRate()
}

func (w *webRuntime) uploadRate() int64 {
	if w.torrents == nil || w.engine == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if now := w.now(); !w.upAt.IsZero() && now.Sub(w.upAt) < time.Second {
		return w.upCached
	}
	var sum int64
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Every torrent the runner holds is Fetching, Fetched, PostProcessing or
	// Ready (a seeding one stays in the engine after it is Ready).
	for _, st := range []core.State{core.StateFetching, core.StateFetched, core.StatePostProcessing, core.StateReady} {
		for after := ""; ; {
			page, next, err := w.downloads.List(ctx, core.DownloadQuery{State: st, SourceKind: core.SourceTorrent, After: after, Limit: store.MaxListLimit})
			if err != nil {
				break
			}
			for _, d := range page {
				if info, ok := w.torrents.Info(d.ID); ok {
					if stats, err := w.engine.Stats(info.Hash); err == nil {
						sum += stats.UpRate
					}
				}
			}
			if next == "" {
				break
			}
			after = next
		}
	}
	w.upAt, w.upCached = w.now(), sum
	return sum
}

func (w *webRuntime) QueueState(kind core.SourceKind) web.QueueState {
	switch kind {
	case core.SourceNZB:
		paused, reason := w.usenet.QueuePaused()
		return web.QueueState{Available: true, Paused: paused, Reason: reason}
	case core.SourceTorrent:
		if w.torrents == nil {
			return web.QueueState{}
		}
		paused, reason := w.torrents.QueuePaused()
		return web.QueueState{Available: true, Paused: paused, Reason: reason}
	}
	return web.QueueState{}
}

func (w *webRuntime) PauseQueue(kind core.SourceKind) {
	switch {
	case kind == core.SourceNZB:
		w.usenet.PauseQueue()
	case kind == core.SourceTorrent && w.torrents != nil:
		w.torrents.PauseQueue()
	}
}

func (w *webRuntime) ResumeQueue(kind core.SourceKind) {
	switch {
	case kind == core.SourceNZB:
		w.usenet.ResumeQueue()
	case kind == core.SourceTorrent && w.torrents != nil:
		w.torrents.ResumeQueue()
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
	if w.torrents == nil || w.engine == nil {
		return l
	}
	info, ok := w.torrents.Info(id)
	if !ok {
		return l
	}
	st, err := w.engine.Stats(info.Hash)
	if err != nil {
		return l
	}
	l.Torrent, l.UpRate, l.Peers, l.Seeds = true, st.UpRate, st.Peers, st.Seeds
	if st.Size > 0 {
		l.Ratio = float64(info.BaseUploaded+st.Uploaded) / float64(st.Size)
	}
	return l
}
