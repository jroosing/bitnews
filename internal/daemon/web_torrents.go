package daemon

import (
	"context"
	"errors"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
	"gitlab.com/kyura4k/gitnews/internal/web"
)

// webTorrents implements web.TorrentSource for the Download detail page over
// the same pieces the qBittorrent adapter uses: the remembered info hashes,
// the torrent runner's in-memory snapshot, the engine's stats and the stored
// torrent record. Nothing here touches the disk besides the repositories.
type webTorrents struct{ q *qbitTorrents }

var _ web.TorrentSource = webTorrents{}

// newWebTorrents returns the TorrentSource of the web UI, or nil (torrents
// are off) when there is no torrent side.
func newWebTorrents(q *qbitTorrents) web.TorrentSource {
	if q == nil {
		return nil
	}
	return webTorrents{q: q}
}

// Torrent implements web.TorrentSource.
func (w webTorrents) Torrent(ctx context.Context, id core.ID) (web.TorrentFacts, bool, error) {
	q := w.q
	h, err := q.hashOf(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return web.TorrentFacts{}, false, nil
	}
	if err != nil {
		return web.TorrentFacts{}, false, err
	}
	f := web.TorrentFacts{InfoHash: h.String()}
	if info, registered := q.run.Info(id); registered {
		f.Registered, f.LimitPaused = true, info.LimitPaused
		f.OwnRatio, f.OwnSeedingTime = info.RatioLimit, info.SeedingTimeLimit
		f.Uploaded, f.SeedingTime = info.BaseUploaded, info.BaseSeeding
		if st, err := q.engine.Stats(info.Hash); err == nil {
			f.Private, f.Size = st.Private, st.Size
			f.Peers, f.Seeds, f.DownRate, f.UpRate = st.Peers, st.Seeds, st.DownRate, st.UpRate
			f.Uploaded += st.Uploaded
			f.SeedingTime += st.SeedingTime
			f.Files = make([]web.TorrentFile, len(st.Files))
			for i, file := range st.Files {
				f.Files[i] = web.TorrentFile{Path: file.Path, Size: file.Size}
			}
		}
	} else if rec, err := q.records.Get(ctx, id); err == nil {
		f.OwnRatio, f.OwnSeedingTime = rec.RatioLimit, rec.SeedingTimeLimit
		f.Uploaded, f.SeedingTime = rec.Uploaded, rec.SeedingTime
	} else if !errors.Is(err, core.ErrNotFound) {
		return web.TorrentFacts{}, false, err
	}
	if f.Size == 0 {
		if d, err := q.downloads.Get(ctx, id); err == nil {
			f.Size = d.TotalBytes
		}
	}
	if f.Size > 0 {
		f.Ratio = float64(f.Uploaded) / float64(f.Size)
	}
	return f, true, nil
}

// SetShareLimit implements web.TorrentSource.
func (w webTorrents) SetShareLimit(ctx context.Context, id core.ID, c web.ShareLimitChange) error {
	if c.UseDefault {
		return w.q.run.ClearShareLimit(ctx, id)
	}
	l := core.UnlimitedShareLimit()
	if c.Ratio >= 0 {
		l.Ratio = c.Ratio
	}
	if c.Minutes >= 0 {
		l.SeedingTime = time.Duration(c.Minutes) * time.Minute
	}
	return w.q.run.SetShareLimit(ctx, id, l)
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
