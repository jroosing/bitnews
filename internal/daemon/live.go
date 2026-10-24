package daemon

import (
	"context"

	"github.com/jroosing/bitnews/internal/core"
	trunner "github.com/jroosing/bitnews/internal/torrent/runner"
	"github.com/jroosing/bitnews/internal/view"
)

// torrentFacts is what liveSource needs of the torrent runner.
type torrentFacts interface {
	Facts(ctx context.Context, id core.ID) (trunner.Facts, bool, error)
}

// liveSource is the one place that gathers what is live about a Download
// for the view module: the stage and rate the event tracker follows, and for
// a torrent the facts of the torrent runner.
type liveSource struct {
	track    *tracker
	torrents torrentFacts // nil when torrents are off
}

var _ view.LiveSource = liveSource{}

// Live implements view.LiveSource.
func (l liveSource) Live(ctx context.Context, d core.Download) view.Live {
	live := view.Live{DownRate: l.track.rate(d.ID)}
	live.Stage, live.HaveStage = l.track.stage(d.ID)
	if d.SourceKind != core.SourceTorrent || l.torrents == nil {
		return live
	}
	f, ok, err := l.torrents.Facts(ctx, d.ID)
	if err != nil || !ok {
		return live
	}
	if f.Live {
		live.DownRate, live.UpRate = f.DownRate, f.UpRate
	}
	live.Torrent = &view.TorrentLive{
		Hash: f.Hash.String(), Registered: f.Registered,
		Size: f.Size, DoneBytes: f.DoneBytes, Peers: f.Peers, Seeds: f.Seeds, Private: f.Private,
		Uploaded: f.Uploaded, SeedingTime: f.SeedingTime,
		OwnRatio: f.OwnRatioLimit, OwnSeedingTime: f.OwnSeedingTimeLimit, LimitPaused: f.LimitPaused,
	}
	return live
}
