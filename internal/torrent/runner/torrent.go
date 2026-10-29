package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/fsutil"
)

// tor is the runner's record of one torrent that is in the engine.
type tor struct {
	id   core.ID
	hash core.InfoHash
	dir  string

	// ctx ends when the torrent is dropped or the Runner shuts down. Steps
	// that run in the background (saving the metadata, the move to
	// complete) use it.
	ctx    context.Context
	cancel context.CancelFunc
	// op serialises those steps; Hold waits on it.
	op chan struct{}

	// The fields below are guarded by Runner.mu.

	// running is false while the engine has the torrent paused for the Queue.
	running bool
	// fetched is set once the torrent is complete: it is Seeding and no
	// longer part of the Queue.
	fetched bool

	// The Seeding counters of earlier runs and the Download's own Share
	// Limit (nil: the default), read from the record when the torrent was
	// added. The engine counts from zero, so the totals are base plus
	// engine.
	baseUploaded int64
	baseSeeding  time.Duration
	ratioLimit   *float64
	timeLimit    *time.Duration
	// limitPaused is set while the torrent is paused because its Share
	// Limit was reached; removing is set while a remove action is under way.
	limitPaused bool
	removing    bool
	// userPaused is set while the user has paused the Seeding of this
	// Fetched torrent (Download.Paused, ADR-0012). It is independent of
	// limitPaused: the torrent uploads only while neither is set.
	userPaused bool
	// persisted is what was last written to the record.
	persistedUp   int64
	persistedSeed time.Duration
}

func newTor(id core.ID, hash core.InfoHash, dir string, running bool) *tor {
	ctx, cancel := context.WithCancel(context.Background())
	return &tor{id: id, hash: hash, dir: dir, ctx: ctx, cancel: cancel, op: make(chan struct{}, 1), running: running}
}

// lock waits for the torrent's background step slot. It reports false when
// ctx or the torrent's own context ended first.
func (t *tor) lock(ctx context.Context) bool {
	if ctx.Err() != nil || t.ctx.Err() != nil {
		return false
	}
	select {
	case t.op <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-t.ctx.Done():
		return false
	}
}

func (t *tor) unlock() { <-t.op }

func (r *Runner) lookup(id core.ID) *tor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tors[id]
}

func (r *Runner) lookupHash(h core.InfoHash) *tor {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tors {
		if t.hash == h {
			return t
		}
	}
	return nil
}

// setRunning starts or pauses the torrent in the engine, unless it already is
// as asked.
func (r *Runner) setRunning(t *tor, running bool) {
	r.mu.Lock()
	was := t.running
	t.running = running
	r.mu.Unlock()
	if was == running {
		return
	}
	var err error
	if running {
		err = r.engine.Resume(t.hash)
	} else {
		r.q.ForgetProgress(t.id)
		err = r.engine.Pause(t.hash)
	}
	if err != nil {
		r.log.Warn("torrent runner: cannot change the state of a torrent", "id", t.id, "running", running, "err", err)
	}
}

// ensureRegistered makes sure the engine has the torrent of d, started when
// running is true.
func (r *Runner) ensureRegistered(ctx context.Context, d core.Download, running bool) (*tor, error) {
	if t := r.lookup(d.ID); t != nil {
		return t, nil
	}
	src, err := r.cfg.Sources.Get(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("load torrent source: %w", err)
	}
	settings, err := r.cfg.Settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	rec, err := r.loadRecord(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("read torrent record: %w", err)
	}
	dir := r.locate(ctx, settings, d, src, rec)
	// A Fetched torrent that had reached its Share Limit before the restart
	// is added paused, so it does not upload until the first evaluation.
	limitPaused := slices.Contains(seedingStates, d.State) &&
		r.limitReached(settings.Seeding, rec, d.TotalBytes, rec.Uploaded, rec.SeedingTime)
	// A Fetched torrent the user paused stays paused across a restart.
	userPaused := slices.Contains(seedingStates, d.State) && d.Paused
	if limitPaused || userPaused {
		running = false
	}
	spec := core.TorrentSpec{StorageDir: dir, Paused: !running}
	if src.TorrentPath != "" {
		meta, err := readMetainfo(filepath.Join(dir, filepath.Base(src.TorrentPath)))
		if err != nil {
			return nil, err
		}
		spec.Metainfo = meta
	} else {
		spec.Magnet = src.Magnet.Reveal()
	}
	if err := fsutil.MkdirAll(dir, 0o777); err != nil {
		return nil, fmt.Errorf("create the download directory: %w", err)
	}
	h, err := r.engine.Add(ctx, spec)
	if err != nil && !errors.Is(err, core.ErrConflict) {
		return nil, fmt.Errorf("add torrent: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.tors[d.ID]; t != nil { // lost a race with another registration
		return t, nil
	}
	t := newTor(d.ID, h, dir, running)
	t.baseUploaded, t.baseSeeding = rec.Uploaded, rec.SeedingTime
	t.persistedUp, t.persistedSeed = rec.Uploaded, rec.SeedingTime
	t.ratioLimit, t.timeLimit = rec.RatioLimit, rec.SeedingTimeLimit
	t.limitPaused = limitPaused
	t.userPaused = userPaused
	r.tors[d.ID] = t
	return t, nil
}

// readMetainfo reads a stored .torrent file, refusing one above the limit.
func readMetainfo(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open torrent file: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, core.MaxMetainfoSize+1))
	if err != nil {
		return nil, fmt.Errorf("read torrent file: %w", err)
	}
	if len(b) > core.MaxMetainfoSize {
		return nil, fmt.Errorf("torrent file larger than %d bytes: %w", core.MaxMetainfoSize, core.ErrInvalidTorrentSpec)
	}
	return b, nil
}

// addFailed handles an error from ensureRegistered. A torrent that can never
// be added fails its Download with source_invalid (when it has not been
// Fetched) and is not tried again; anything else is retried on the next
// pass.
func (r *Runner) addFailed(ctx context.Context, d core.Download, err error) {
	if ctx.Err() != nil {
		return
	}
	permanent := errors.Is(err, core.ErrInvalidTorrentSpec) || errors.Is(err, core.ErrNotFound)
	if !permanent {
		r.log.Warn("torrent runner: cannot add the torrent, will retry", "id", d.ID, "err", err)
		return
	}
	r.log.Warn("torrent runner: the torrent cannot be used", "id", d.ID, "err", err)
	r.mu.Lock()
	r.bad[d.ID] = struct{}{}
	r.mu.Unlock()
	if d.FetchedAt.IsZero() {
		r.fail(ctx, d.ID, core.FailureSourceInvalid, "the torrent cannot be used")
	}
}

// fail moves the Download to Failed and publishes the events.
func (r *Runner) fail(ctx context.Context, id core.ID, reason core.FailureReason, detail string) {
	d, err := r.q.SetState(ctx, id, core.StateChange{To: core.StateFailed, At: r.now(), FailureReason: reason, FailureDetail: detail})
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrIllegalTransition) {
			r.log.Warn("torrent runner: cannot fail the download", "id", id, "err", err)
		}
		return
	}
	r.cfg.Bus.Publish(core.Failed{ID: d.ID, Reason: reason, Detail: detail})
}
