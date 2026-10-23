package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/adapter/qbit"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
	trunner "gitlab.com/kyura4k/gitnews/internal/torrent/runner"
)

// qbitTorrents implements qbit.TorrentSource, qbit.FileSource and
// qbit.ShareLimiter over the Download repository and the torrent runner. The
// adapter reads everything it can from the view module; this glue answers only
// what the runner alone knows: which Download an info hash belongs to, the
// torrent's name and paths, and its files. Torrent is called for every
// torrent on every poll of a media manager, so it touches no disk unless
// asked: Facts of a torrent in the engine come from memory, and the Download
// of an info hash is found once and remembered.
type qbitTorrents struct {
	downloads core.DownloadRepo
	sources   core.TorrentSourceRepo
	run       *trunner.Runner

	mu  sync.Mutex
	ids map[core.InfoHash]core.ID
}

var (
	_ qbit.TorrentSource = (*qbitTorrents)(nil)
	_ qbit.FileSource    = (*qbitTorrents)(nil)
	_ qbit.ShareLimiter  = (*qbitTorrents)(nil)
	_ qbit.TorrentAdder  = (*qbitAdder)(nil)
)

func newQBitTorrents(downloads core.DownloadRepo, sources core.TorrentSourceRepo, run *trunner.Runner) *qbitTorrents {
	return &qbitTorrents{downloads: downloads, sources: sources, run: run, ids: map[core.InfoHash]core.ID{}}
}

// remember notes the Download of an info hash.
func (q *qbitTorrents) remember(h core.InfoHash, id core.ID) {
	q.mu.Lock()
	q.ids[h] = id
	q.mu.Unlock()
}

// Lookup implements qbit.TorrentSource.
func (q *qbitTorrents) Lookup(ctx context.Context, hash string) (core.Download, bool, error) {
	h, err := core.ParseInfoHash(hash)
	if err != nil {
		return core.Download{}, false, nil
	}
	q.mu.Lock()
	id, ok := q.ids[h]
	q.mu.Unlock()
	if !ok {
		src, err := q.sources.GetByInfoHash(ctx, h)
		if errors.Is(err, core.ErrNotFound) {
			return core.Download{}, false, nil
		}
		if err != nil && !(errors.Is(err, core.ErrSecretUnavailable) && src.DownloadID != "") {
			return core.Download{}, false, err
		}
		id = src.DownloadID
		q.remember(h, id)
	}
	d, err := q.downloads.Get(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		q.mu.Lock()
		delete(q.ids, h)
		q.mu.Unlock()
		return core.Download{}, false, nil
	}
	return d, err == nil, err
}

// Torrent implements qbit.TorrentSource. full also finds the torrent's own
// root directory on disk.
func (q *qbitTorrents) Torrent(ctx context.Context, d core.Download, full bool) (qbit.Torrent, bool, error) {
	f, ok, err := q.run.Facts(ctx, d.ID)
	if err != nil || !ok {
		return qbit.Torrent{}, false, err
	}
	q.remember(f.Hash, d.ID)
	t := qbit.Torrent{Name: f.Name}
	if f.Dir != "" {
		t.ContentPath, t.SavePath = f.Dir, filepath.Dir(f.Dir)
	}
	if full && f.Registered {
		t.ContentPath = q.run.ContentDir(d)
	}
	return t, true, nil
}

// Files implements qbit.FileSource from the engine's file list; empty while
// the engine does not have the torrent.
func (q *qbitTorrents) Files(ctx context.Context, id core.ID) ([]qbit.TorrentFile, bool, error) {
	f, ok, err := q.run.Facts(ctx, id)
	if err != nil || !ok {
		return nil, false, err
	}
	files := make([]qbit.TorrentFile, len(f.Files))
	for i, file := range f.Files {
		files[i] = qbit.TorrentFile{Name: file.Path, Size: file.Size}
	}
	return files, true, nil
}

// SetShareLimit implements qbit.ShareLimiter.
func (q *qbitTorrents) SetShareLimit(ctx context.Context, id core.ID, ratio float64, seedingSeconds int64, useGlobal bool) error {
	if useGlobal {
		return q.run.ClearShareLimit(ctx, id)
	}
	return q.run.SetShareLimit(ctx, id, coreShareLimit(ratio, seedingSeconds))
}

// coreShareLimit converts the adapter's limit (-1 unlimited, seconds) to
// core's.
func coreShareLimit(ratio float64, seedingSeconds int64) core.ShareLimit {
	l := core.UnlimitedShareLimit()
	if ratio >= 0 {
		l.Ratio = ratio
	}
	if seedingSeconds >= 0 {
		l.SeedingTime = time.Duration(seedingSeconds) * time.Second
	}
	return l
}

// qbitGlobalShare reports the global Share Limit default for the adapter's
// preferences.
func qbitGlobalShare(settings func() core.Settings) func() qbit.GlobalShare {
	return func() qbit.GlobalShare {
		s := settings().Seeding
		g := qbit.GlobalShare{
			RatioEnabled: s.RatioLimit > 0, Ratio: max(s.RatioLimit, 0),
			SeedingTimeEnabled: s.SeedingTimeMinutes > 0, SeedingMinutes: max(s.SeedingTimeMinutes, 0),
			Action: "pause",
		}
		switch s.EffectiveAction() {
		case core.SeedingActionRemove:
			g.Action = "remove"
		case core.SeedingActionRemoveData:
			g.Action = "remove_files"
		}
		return g
	}
}

// qbitAdder implements qbit.TorrentAdder on the AddService, so a torrent
// added through the adapter goes the same way as one added through the own
// API: staged, checked, stored and queued. The qBit API is torrent-only, so a
// URL that turns out to be an NZB is refused.
type qbitAdder struct {
	add    *app.AddService
	limits *qbitTorrents
	log    *slog.Logger
}

// AddTorrentFile implements qbit.TorrentAdder.
func (a *qbitAdder) AddTorrentFile(ctx context.Context, data []byte, opts qbit.AddOptions) (string, error) {
	st, err := a.add.StageTorrent(ctx, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	return a.addStaged(ctx, st, opts)
}

// AddURL implements qbit.TorrentAdder. The URL can hold a passkey: it is
// never logged here.
func (a *qbitAdder) AddURL(ctx context.Context, url string, opts qbit.AddOptions) (string, error) {
	if len(url) >= len("magnet:") && strings.EqualFold(url[:len("magnet:")], "magnet:") {
		st, err := a.add.StageMagnet(ctx, url)
		if err != nil {
			return "", err
		}
		return a.addStaged(ctx, st, opts)
	}
	fetched, err := a.add.Fetch(ctx, url)
	if err != nil {
		return "", err
	}
	if fetched.Torrent == nil {
		fetched.Discard()
		return "", fmt.Errorf("%w: the URL is not a torrent", app.ErrInvalidInput)
	}
	return a.addStaged(ctx, fetched.Torrent, opts)
}

func (a *qbitAdder) addStaged(ctx context.Context, st *app.StagedTorrent, opts qbit.AddOptions) (string, error) {
	hash := st.InfoHash().String() // st must not be used after AddTorrent
	if opts.SavePath != "" {
		a.log.Debug("qbit: the save path of a torrent add is ignored; the Download goes to its Category's directory")
	}
	d, err := a.add.AddTorrent(ctx, st, app.AddRequest{CategoryID: opts.CategoryID, Paused: opts.Paused})
	if err != nil {
		return hash, err // a duplicate matches core.ErrConflict
	}
	if l := opts.ShareLimit; l != nil {
		if err := a.limits.SetShareLimit(ctx, d.ID, l.Ratio, l.SeedingSeconds, l.Global); err != nil {
			a.log.Warn("qbit: cannot set the share limit of a new torrent", "id", d.ID, "err", err)
		}
	}
	return hash, nil
}
