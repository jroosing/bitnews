package daemon

import (
	"context"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	trunner "gitlab.com/kyura4k/gitnews/internal/torrent/runner"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// webTorrents implements web.TorrentSource for the Download detail page on
// the torrent runner's Facts.
type webTorrents struct{ run *trunner.Runner }

var _ web.TorrentSource = webTorrents{}

// newWebTorrents returns the TorrentSource of the web UI, or nil (torrents
// are off) when there is no torrent runner.
func newWebTorrents(run *trunner.Runner) web.TorrentSource {
	if run == nil {
		return nil
	}
	return webTorrents{run: run}
}

// Torrent implements web.TorrentSource.
func (w webTorrents) Torrent(ctx context.Context, id core.ID) (web.TorrentFacts, bool, error) {
	f, ok, err := w.run.Facts(ctx, id)
	if err != nil || !ok {
		return web.TorrentFacts{}, false, err
	}
	out := web.TorrentFacts{
		InfoHash: f.Hash.String(), Registered: f.Registered, LimitPaused: f.LimitPaused,
		OwnRatio: f.OwnRatioLimit, OwnSeedingTime: f.OwnSeedingTimeLimit,
		Private: f.Private, Size: f.Size, Peers: f.Peers, Seeds: f.Seeds, DownRate: f.DownRate, UpRate: f.UpRate,
		Uploaded: f.Uploaded, SeedingTime: f.SeedingTime, Ratio: f.Ratio,
	}
	if f.Live {
		out.Files = make([]web.TorrentFile, len(f.Files))
		for i, file := range f.Files {
			out.Files[i] = web.TorrentFile{Path: file.Path, Size: file.Size}
		}
	}
	return out, true, nil
}

// SetShareLimit implements web.TorrentSource.
func (w webTorrents) SetShareLimit(ctx context.Context, id core.ID, c web.ShareLimitChange) error {
	if c.UseDefault {
		return w.run.ClearShareLimit(ctx, id)
	}
	l := core.UnlimitedShareLimit()
	if c.Ratio >= 0 {
		l.Ratio = c.Ratio
	}
	if c.Minutes >= 0 {
		l.SeedingTime = time.Duration(c.Minutes) * time.Minute
	}
	return w.run.SetShareLimit(ctx, id, l)
}

// webDetail implements web.DetailSource: the Article report of the Usenet
// runner (in memory, else the stored one) and the Post-processing stage the
// event tracker follows.
type webDetail struct {
	run     *runner.Runner
	reports reports
	track   *tracker
}

var _ web.DetailSource = webDetail{}

// FetchReport implements web.DetailSource.
func (d webDetail) FetchReport(ctx context.Context, id core.ID) (web.FetchReport, bool) {
	detail, ok := d.run.Detail(id)
	if !ok {
		var err error
		if detail, err = d.reports.LoadReport(ctx, id); err != nil {
			return web.FetchReport{}, false
		}
	}
	return web.FetchReport{
		MissingArticles: detail.MissingArticles, MissingBytes: detail.MissingBytes,
		DamagedArticles: detail.DamagedArticles, DamagedBytes: detail.DamagedBytes,
		MissingFiles: detail.MissingFiles,
	}, true
}

// PostProcessStage implements web.DetailSource.
func (d webDetail) PostProcessStage(id core.ID) (string, bool) {
	st, ok := d.track.stage(id)
	return string(st), ok
}
