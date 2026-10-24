package daemon

import (
	"context"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	trunner "github.com/jroosing/bitnews/internal/torrent/runner"
	"github.com/jroosing/bitnews/internal/usenet/runner"
	"github.com/jroosing/bitnews/internal/web"
)

// webTorrents implements web.TorrentSource for the Download detail page: the
// file list from the torrent runner's Facts and the Share Limit through the
// runner. The other torrent facts reach the page through the view module.
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

// Files implements web.TorrentSource: the files the engine knows of the
// torrent, nil when it is not in the engine.
func (w webTorrents) Files(ctx context.Context, id core.ID) ([]web.TorrentFile, error) {
	f, ok, err := w.run.Facts(ctx, id)
	if err != nil || !ok || !f.Live {
		return nil, err
	}
	out := make([]web.TorrentFile, len(f.Files))
	for i, file := range f.Files {
		out[i] = web.TorrentFile{Path: file.Path, Size: file.Size}
	}
	return out, nil
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
// runner (in memory, else the stored one).
type webDetail struct {
	run     *runner.Runner
	reports reports
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
