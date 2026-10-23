package runner

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// The files of a Download's directory that are not part of the torrent.
const (
	unpackName       = "_unpack"
	unpackTmpName    = "_unpack.tmp"
	unpackSetsSuffix = ".sets"
	doneMarkerName   = ".bitnews-done"
)

// Managed reports whether the engine has the torrent of the Download. The
// engine then owns the files: nobody else may move or delete them.
func (r *Runner) Managed(id core.ID) bool { return r.lookup(id) != nil }

// Dir returns the Download's directory: the storage directory of its torrent.
// It holds the stored .torrent file and the torrent's data, and it is where
// the engine's files are, wherever the move to complete has put them.
func (r *Runner) Dir(d core.Download) string {
	if t := r.lookup(d.ID); t != nil {
		return r.dirOf(t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings, err := r.cfg.Settings(ctx)
	if err != nil {
		return ""
	}
	src, err := r.cfg.Sources.Get(ctx, d.ID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		r.log.Debug("torrent runner: no stored source, using the settings", "id", d.ID, "err", err)
	}
	rec, err := r.loadRecord(ctx, d.ID)
	if err != nil {
		rec = core.TorrentRecord{DownloadID: d.ID}
	}
	return r.locate(ctx, settings, d, src, rec)
}

// ContentDir returns the directory Post-processing works in: the torrent's
// own root directory inside the Download's directory, or the Download's
// directory itself for a single-file torrent. This is where Unpack looks for
// archives and puts _unpack/, and the path media managers are given.
func (r *Runner) ContentDir(d core.Download) string {
	dir := r.Dir(d)
	if dir == "" {
		return ""
	}
	skip := map[string]struct{}{
		unpackName: {}, unpackTmpName: {}, unpackName + unpackSetsSuffix: {}, doneMarkerName: {}, reservedMetainfoName: {},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if src, err := r.cfg.Sources.Get(ctx, d.ID); err == nil && src.TorrentPath != "" {
		skip[filepath.Base(src.TorrentPath)] = struct{}{}
	}
	return contentRoot(dir, skip)
}

// contentRoot finds the torrent's root directory in dir: the one directory
// that is left once the names in skip are ignored. Anything else (several
// entries, a lone file, a symlink) means the torrent has no root directory of
// its own, and dir is the content.
func contentRoot(dir string, skip map[string]struct{}) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dir
	}
	var only fs.DirEntry
	for _, e := range entries {
		if _, ignore := skip[e.Name()]; ignore {
			continue
		}
		if only != nil {
			return dir
		}
		only = e
	}
	if only == nil || !only.IsDir() { // IsDir is false for a symlink
		return dir
	}
	return filepath.Join(dir, only.Name())
}

// onTorrentError handles an error the engine reports about a torrent. A
// Download that is still being fetched fails and its torrent leaves the
// engine (the files stay); one that was Fetched keeps what it has and the
// error is only logged, because Ready and Seeding do not depend on it. A full
// disk does not fail the Download: the Queue pauses with disk_full and the
// Download waits Queued until the space is back.
func (r *Runner) onTorrentError(ctx context.Context, t *tor, cause error) {
	r.log.Warn("torrent runner: the engine reports an error", "id", t.id, "err", cause)
	if r.isFetched(t.id) {
		return
	}
	d, err := r.cfg.Downloads.Get(ctx, t.id)
	if err != nil {
		return
	}
	switch d.State {
	case core.StateFetchingMetadata, core.StateQueued, core.StateFetching:
	default:
		return
	}
	if errors.Is(cause, core.ErrDiskFull) || errors.Is(cause, syscall.ENOSPC) {
		r.diskFull(ctx, t.id, cause)
		return
	}
	reason, detail := core.FailureInternal, "the torrent engine reported an error"
	switch {
	case errors.Is(cause, core.ErrInvalidTorrentSpec):
		reason, detail = core.FailureSourceInvalid, "the torrent cannot be used"
	case errors.Is(cause, fs.ErrPermission):
		// Name the folder and the user id: the usual fix is a chown or a
		// different `user:` in the container.
		detail = fsutil.WriteError(cause).Error()
	}
	r.fail(ctx, t.id, reason, detail)
	r.drop(ctx, t.id)
}

// diskFull pauses the Queue for a torrent that ran out of disk space. The
// engine's torrent is dead after an error, so it leaves the engine (its
// files and verified pieces stay) and the Download goes back to Queued; the
// Queue adds it again once the space is back.
func (r *Runner) diskFull(ctx context.Context, id core.ID, cause error) {
	// Pause first, so no pass starts the Download again in between.
	r.q.DiskFull("the disk is full: " + cause.Error())
	r.drop(ctx, id)
	_, err := r.q.SetState(context.WithoutCancel(ctx), id, core.StateChange{To: core.StateQueued, At: r.now()})
	if err != nil && !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrIllegalTransition) {
		r.log.Warn("torrent runner: cannot requeue the download", "id", id, "err", err)
	}
}

// Info is a snapshot of what the runner knows about a torrent in the engine,
// taken from memory only (no disk, no database).
type Info struct {
	Hash core.InfoHash
	// Dir is the Download's directory, wherever the move to complete put it.
	Dir string
	// BaseUploaded and BaseSeeding are the counters of earlier runs; the
	// totals are these plus the engine's own.
	BaseUploaded int64
	BaseSeeding  time.Duration
	// RatioLimit and SeedingTimeLimit are the Download's own Share Limit; nil
	// means the default applies, a negative value is unlimited.
	RatioLimit       *float64
	SeedingTimeLimit *time.Duration
	// Fetched is set once the torrent is complete and Seeding.
	Fetched bool
	// LimitPaused is set while the torrent is paused by its Share Limit.
	LimitPaused bool
}

// Info returns the snapshot for the Download; false means the engine does
// not have its torrent.
func (r *Runner) Info(id core.ID) (Info, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tors[id]
	if t == nil {
		return Info{}, false
	}
	return Info{
		Hash: t.hash, Dir: t.dir, BaseUploaded: t.baseUploaded, BaseSeeding: t.baseSeeding,
		RatioLimit: t.ratioLimit, SeedingTimeLimit: t.timeLimit, Fetched: t.fetched, LimitPaused: t.limitPaused,
	}, true
}
