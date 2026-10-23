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
// qbit.ShareLimiter over the Download repository and the torrent runner,
// whose Facts answer everything about a torrent. List is called on every poll
// of a media manager, so it touches no disk: Facts of a torrent in the engine
// come from memory, and the Download of an info hash is found once and
// remembered.
type qbitTorrents struct {
	downloads core.DownloadRepo
	sources   core.TorrentSourceRepo
	run       *trunner.Runner
	settings  func() core.Settings
	log       *slog.Logger

	mu  sync.Mutex
	ids map[core.InfoHash]core.ID
}

var (
	_ qbit.TorrentSource = (*qbitTorrents)(nil)
	_ qbit.FileSource    = (*qbitTorrents)(nil)
	_ qbit.ShareLimiter  = (*qbitTorrents)(nil)
	_ qbit.TorrentAdder  = (*qbitAdder)(nil)
)

func newQBitTorrents(downloads core.DownloadRepo, sources core.TorrentSourceRepo, run *trunner.Runner,
	settings func() core.Settings, log *slog.Logger) *qbitTorrents {
	return &qbitTorrents{
		downloads: downloads, sources: sources, run: run, settings: settings, log: log,
		ids: map[core.InfoHash]core.ID{},
	}
}

// remember notes the Download of an info hash.
func (q *qbitTorrents) remember(h core.InfoHash, id core.ID) {
	q.mu.Lock()
	q.ids[h] = id
	q.mu.Unlock()
}

// downloadOf finds the Download of an info hash.
func (q *qbitTorrents) downloadOf(ctx context.Context, h core.InfoHash) (core.Download, bool, error) {
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

// List implements qbit.TorrentSource.
func (q *qbitTorrents) List(ctx context.Context) ([]qbit.TorrentView, error) {
	var out []qbit.TorrentView
	for after := ""; ; {
		page, next, err := q.downloads.List(ctx, core.DownloadQuery{SourceKind: core.SourceTorrent, After: after, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, d := range page {
			v, ok, err := q.view(ctx, d, false)
			if err != nil || !ok {
				q.log.Debug("qbit: torrent skipped", "id", d.ID, "err", err)
				continue
			}
			out = append(out, v)
		}
		if next == "" {
			return out, nil
		}
		after = next
	}
}

// Get implements qbit.TorrentSource.
func (q *qbitTorrents) Get(ctx context.Context, hash string) (qbit.TorrentView, bool, error) {
	h, err := core.ParseInfoHash(hash)
	if err != nil {
		return qbit.TorrentView{}, false, nil
	}
	d, ok, err := q.downloadOf(ctx, h)
	if err != nil || !ok {
		return qbit.TorrentView{}, false, err
	}
	return q.view(ctx, d, true)
}

// view translates the Facts of d's torrent to the adapter's form. full also
// finds the content directory on disk, which List must not do.
func (q *qbitTorrents) view(ctx context.Context, d core.Download, full bool) (qbit.TorrentView, bool, error) {
	f, ok, err := q.run.Facts(ctx, d.ID)
	if err != nil || !ok {
		return qbit.TorrentView{}, false, err
	}
	q.remember(f.Hash, d.ID)
	v := qbit.TorrentView{
		DownloadID: d.ID, InfoHash: f.Hash.String(), Name: f.Name, Size: f.Size, DoneBytes: f.DoneBytes,
		AddedAt: d.AddedAt, CompletedAt: d.FetchedAt,
		DownRate: f.DownRate, UpRate: f.UpRate, Peers: f.Peers, Seeds: f.Seeds, Private: f.Private,
		Uploaded: f.Uploaded, SeedingTime: f.SeedingTime, Ratio: f.Ratio,
		Paused:     f.LimitPaused,
		ShareLimit: shareLimitView(q.settings().Seeding, f.OwnRatioLimit, f.OwnSeedingTimeLimit),
	}
	if f.Dir != "" {
		v.ContentPath, v.SavePath = f.Dir, filepath.Dir(f.Dir)
	}
	if !d.FetchedAt.IsZero() && v.Size > 0 {
		v.DoneBytes = v.Size
	}
	if full && f.Registered {
		v.ContentPath = q.run.ContentDir(d)
	}
	return v, true, nil
}

// shareLimitView converts a Download's own limit and the global default to
// the adapter's form: Global when the Download has no limit of its own, else
// the effective values, with the global default filling in a half.
func shareLimitView(global core.SeedingSettings, ratio *float64, seeding *time.Duration) qbit.ShareLimit {
	if ratio == nil && seeding == nil {
		return qbit.ShareLimit{Global: true}
	}
	l := global.ShareLimit()
	if ratio != nil {
		l.Ratio = *ratio
	}
	if seeding != nil {
		l.SeedingTime = *seeding
	}
	out := qbit.ShareLimit{Ratio: -1, SeedingSeconds: -1}
	if l.Ratio >= 0 {
		out.Ratio = l.Ratio
	}
	if l.SeedingTime >= 0 {
		out.SeedingSeconds = int64(l.SeedingTime / time.Second)
	}
	return out
}

// Files implements qbit.FileSource from the engine's file list. Progress is
// that of the whole torrent: the engine does not report it per file.
func (q *qbitTorrents) Files(ctx context.Context, hash string) ([]qbit.TorrentFile, bool, error) {
	h, err := core.ParseInfoHash(hash)
	if err != nil {
		return nil, false, nil
	}
	d, ok, err := q.downloadOf(ctx, h)
	if err != nil || !ok {
		return nil, false, err
	}
	f, ok, err := q.run.Facts(ctx, d.ID)
	if err != nil {
		return nil, false, err
	}
	if !ok || !f.Live {
		return nil, true, nil // known, but not in the engine (yet)
	}
	progress := 0.0
	switch {
	case !d.FetchedAt.IsZero():
		progress = 1
	case f.Size > 0:
		progress = float64(f.DoneBytes) / float64(f.Size)
	}
	files := make([]qbit.TorrentFile, len(f.Files))
	for i, file := range f.Files {
		files[i] = qbit.TorrentFile{Name: file.Path, Size: file.Size, Progress: progress}
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
