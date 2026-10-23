package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Facts is everything known about the torrent of one Download, in one
// answer: the runner's memory and the engine's counters for a torrent in the
// engine, the stored record for one that is not, and the Download itself
// where neither knows better.
type Facts struct {
	Hash core.InfoHash
	// Registered is set when the engine has the torrent.
	Registered bool
	// Live is set when the engine answered: the rates, peers, seeds, the
	// private flag and the files are known only then.
	Live bool
	// Name and Size are the engine's, else the Download's.
	Name      string
	Size      int64
	DoneBytes int64
	DownRate  int64 // bytes per second
	UpRate    int64 // bytes per second
	Peers     int
	Seeds     int
	Private   bool
	// Uploaded and SeedingTime are the totals: the counters of earlier runs
	// plus the engine's own.
	Uploaded    int64
	SeedingTime time.Duration
	// Ratio is Uploaded over Size; zero while the size is unknown.
	Ratio float64
	// OwnRatioLimit and OwnSeedingTimeLimit are the Download's own Share
	// Limit halves; nil means the default applies, a negative value is
	// unlimited.
	OwnRatioLimit       *float64
	OwnSeedingTimeLimit *time.Duration
	// LimitPaused is set while the torrent is paused by its Share Limit.
	LimitPaused bool
	// Fetched is set once the torrent is complete.
	Fetched bool
	// Dir is the Download's directory: where the engine keeps the torrent,
	// or, for one not in the engine, where the move to complete put it
	// (empty when it was not moved).
	Dir   string
	Files []core.TorrentFile
}

// Facts answers what is known about the torrent of the Download. ok is false
// when the Download has no torrent. A torrent in the engine is answered from
// memory and the engine; the database is read only for what neither knows
// (the name and size before the metadata, or when the engine fails).
func (r *Runner) Facts(ctx context.Context, id core.ID) (Facts, bool, error) {
	var f Facts
	r.mu.Lock()
	t := r.tors[id]
	if t != nil {
		f = Facts{
			Hash: t.hash, Registered: true, Dir: t.dir, Fetched: t.fetched, LimitPaused: t.limitPaused,
			Uploaded: t.baseUploaded, SeedingTime: t.baseSeeding,
			OwnRatioLimit: t.ratioLimit, OwnSeedingTimeLimit: t.timeLimit,
		}
	}
	r.mu.Unlock()
	if t != nil {
		if st, err := r.engine.Stats(t.hash); err == nil {
			f.Live = true
			f.Name, f.Size, f.DoneBytes = st.Name, st.Size, st.DoneBytes
			f.DownRate, f.UpRate, f.Peers, f.Seeds, f.Private = st.DownRate, st.UpRate, st.Peers, st.Seeds, st.Private
			f.Uploaded += st.Uploaded
			f.SeedingTime += st.SeedingTime
			f.Files = st.Files
		}
	} else {
		h, ok, err := r.hashOf(ctx, id)
		if err != nil || !ok {
			return Facts{}, false, err
		}
		f.Hash = h
		rec, err := r.cfg.Records.Get(ctx, id)
		switch {
		case err == nil:
			f.Uploaded, f.SeedingTime = rec.Uploaded, rec.SeedingTime
			f.OwnRatioLimit, f.OwnSeedingTimeLimit = rec.RatioLimit, rec.SeedingTimeLimit
			if rec.Moved {
				f.Dir = rec.Location
			}
		case !errors.Is(err, core.ErrNotFound):
			return Facts{}, false, fmt.Errorf("torrent runner: facts: %w", err)
		}
	}
	if !f.Live || f.Size <= 0 || f.Name == "" {
		d, err := r.cfg.Downloads.Get(ctx, id)
		if errors.Is(err, core.ErrNotFound) {
			return Facts{}, false, nil
		}
		if err != nil {
			return Facts{}, false, fmt.Errorf("torrent runner: facts: %w", err)
		}
		if f.Name == "" {
			f.Name = d.Name
		}
		if f.Size <= 0 {
			f.Size = d.TotalBytes
		}
		if !f.Live {
			f.DoneBytes = d.DoneBytes
		}
		if !f.Registered {
			f.Fetched = !d.FetchedAt.IsZero()
		}
	}
	if f.Size > 0 {
		f.Ratio = float64(f.Uploaded) / float64(f.Size)
	}
	return f, true, nil
}

// hashOf returns the info hash of a Download whose torrent is not in the
// engine, from the stored source; false when it has none. The hash never
// changes, so it is remembered until the torrent is dropped.
func (r *Runner) hashOf(ctx context.Context, id core.ID) (core.InfoHash, bool, error) {
	r.mu.Lock()
	h, ok := r.hashes[id]
	r.mu.Unlock()
	if ok {
		return h, true, nil
	}
	src, err := r.cfg.Sources.Get(ctx, id)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return core.InfoHash{}, false, nil
	case err != nil && !(errors.Is(err, core.ErrSecretUnavailable) && !src.InfoHash.IsZero()):
		return core.InfoHash{}, false, fmt.Errorf("torrent runner: facts: %w", err)
	}
	r.mu.Lock()
	r.hashes[id] = src.InfoHash
	r.mu.Unlock()
	return src.InfoHash, true, nil
}

// UploadRate is the sum of the upload rates of the torrents in the engine, in
// bytes per second.
func (r *Runner) UploadRate() int64 {
	r.mu.Lock()
	hashes := make([]core.InfoHash, 0, len(r.tors))
	for _, t := range r.tors {
		hashes = append(hashes, t.hash)
	}
	r.mu.Unlock()
	var sum int64
	for _, h := range hashes {
		if st, err := r.engine.Stats(h); err == nil {
			sum += st.UpRate
		}
	}
	return sum
}
