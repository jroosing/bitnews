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
// qbit.ShareLimiter over the Download repository, the torrent runner and the
// engine. List is called on every poll of a media manager, so it touches no
// disk: a registered torrent is described from the runner's memory and the
// engine's stats, and the info hash of a Download is read once (it never
// changes) and remembered.
type qbitTorrents struct {
	downloads core.DownloadRepo
	sources   core.TorrentSourceRepo
	records   core.TorrentRecordRepo
	engine    core.TorrentEngine
	run       *trunner.Runner
	settings  func() core.Settings
	log       *slog.Logger

	mu     sync.Mutex
	hashes map[core.ID]core.InfoHash
	ids    map[core.InfoHash]core.ID
}

var (
	_ qbit.TorrentSource = (*qbitTorrents)(nil)
	_ qbit.FileSource    = (*qbitTorrents)(nil)
	_ qbit.ShareLimiter  = (*qbitTorrents)(nil)
	_ qbit.TorrentAdder  = (*qbitAdder)(nil)
)

func newQBitTorrents(downloads core.DownloadRepo, sources core.TorrentSourceRepo, records core.TorrentRecordRepo,
	engine core.TorrentEngine, run *trunner.Runner, settings func() core.Settings, log *slog.Logger) *qbitTorrents {
	return &qbitTorrents{
		downloads: downloads, sources: sources, records: records, engine: engine, run: run, settings: settings, log: log,
		hashes: map[core.ID]core.InfoHash{}, ids: map[core.InfoHash]core.ID{},
	}
}

// hashOf returns the info hash of the Download, remembering it.
func (q *qbitTorrents) hashOf(ctx context.Context, id core.ID) (core.InfoHash, error) {
	q.mu.Lock()
	h, ok := q.hashes[id]
	q.mu.Unlock()
	if ok {
		return h, nil
	}
	src, err := q.sources.Get(ctx, id)
	if err != nil && !(errors.Is(err, core.ErrSecretUnavailable) && !src.InfoHash.IsZero()) {
		return core.InfoHash{}, err
	}
	q.mu.Lock()
	q.hashes[id], q.ids[src.InfoHash] = src.InfoHash, id
	q.mu.Unlock()
	return src.InfoHash, nil
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
		q.mu.Lock()
		q.hashes[id], q.ids[h] = h, id
		q.mu.Unlock()
	}
	d, err := q.downloads.Get(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		q.mu.Lock()
		delete(q.hashes, id)
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
			v, err := q.view(ctx, d, false)
			if err != nil {
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
	v, err := q.view(ctx, d, true)
	if err != nil {
		return qbit.TorrentView{}, false, err
	}
	return v, true, nil
}

// view describes the torrent of d. full also finds the content directory on
// disk, which List must not do.
func (q *qbitTorrents) view(ctx context.Context, d core.Download, full bool) (qbit.TorrentView, error) {
	h, err := q.hashOf(ctx, d.ID)
	if err != nil {
		return qbit.TorrentView{}, err
	}
	global := q.settings().Seeding
	v := qbit.TorrentView{
		DownloadID: d.ID, InfoHash: h.String(), Name: d.Name, Size: d.TotalBytes, DoneBytes: d.DoneBytes,
		AddedAt: d.AddedAt, CompletedAt: d.FetchedAt,
	}
	var ratioLimit *float64
	var timeLimit *time.Duration
	var baseUp int64
	var baseSeed time.Duration
	info, registered := q.run.Info(d.ID)
	if registered {
		ratioLimit, timeLimit, baseUp, baseSeed = info.RatioLimit, info.SeedingTimeLimit, info.BaseUploaded, info.BaseSeeding
		v.Paused = info.LimitPaused
		v.ContentPath = info.Dir
		v.SavePath = filepath.Dir(info.Dir)
		if st, err := q.engine.Stats(h); err == nil {
			if st.Name != "" {
				v.Name = st.Name
			}
			if st.Size > 0 {
				v.Size = st.Size
			}
			v.DoneBytes = st.DoneBytes
			v.DownRate, v.UpRate = st.DownRate, st.UpRate
			v.Peers, v.Seeds = st.Peers, st.Seeds
			v.Private = st.Private
			v.Uploaded = baseUp + st.Uploaded
			v.SeedingTime = baseSeed + st.SeedingTime
		} else {
			v.Uploaded, v.SeedingTime = baseUp, baseSeed
		}
	} else if rec, err := q.records.Get(ctx, d.ID); err == nil {
		ratioLimit, timeLimit = rec.RatioLimit, rec.SeedingTimeLimit
		v.Uploaded, v.SeedingTime = rec.Uploaded, rec.SeedingTime
		if rec.Moved && rec.Location != "" {
			v.ContentPath, v.SavePath = rec.Location, filepath.Dir(rec.Location)
		}
	}
	if !d.FetchedAt.IsZero() && v.Size > 0 {
		v.DoneBytes = v.Size
	}
	if v.Size > 0 {
		v.Ratio = float64(v.Uploaded) / float64(v.Size)
	}
	v.ShareLimit = shareLimitView(global, ratioLimit, timeLimit)
	if full && registered {
		v.ContentPath = q.run.ContentDir(d)
	}
	return v, nil
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
	st, err := q.engine.Stats(h)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, true, nil // known, but not in the engine (yet)
		}
		return nil, false, err
	}
	progress := 0.0
	switch {
	case !d.FetchedAt.IsZero():
		progress = 1
	case st.Size > 0:
		progress = float64(st.DoneBytes) / float64(st.Size)
	}
	files := make([]qbit.TorrentFile, len(st.Files))
	for i, f := range st.Files {
		files[i] = qbit.TorrentFile{Name: f.Path, Size: f.Size, Progress: progress}
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
