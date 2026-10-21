package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// reconcileMetadata keeps the magnet Downloads that wait for their metadata
// in the engine, running unless they or the Queue are paused.
func (r *Runner) reconcileMetadata(ctx context.Context) {
	ds, err := r.list(ctx, core.StateFetchingMetadata)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("torrent runner: cannot list downloads", "err", err)
		}
		return
	}
	queuePaused, _ := r.QueuePaused()
	for _, d := range ds {
		if r.isBad(d.ID) {
			continue
		}
		wanted := !d.Paused && !queuePaused
		t := r.lookup(d.ID)
		if t == nil && !wanted {
			continue
		}
		if t == nil {
			t, err = r.ensureRegistered(ctx, d, true)
			if err != nil {
				r.addFailed(ctx, d, err)
				continue
			}
		}
		r.setRunning(t, wanted)
		// A torrent added from a stored file knows its metadata at once and
		// the engine says nothing about it.
		if src, err := r.cfg.Sources.Get(ctx, d.ID); err == nil && src.TorrentPath != "" {
			r.goStep(t, func(ctx context.Context) { r.onMetadata(ctx, t) })
		}
	}
}

// goStep runs f in the background, serialised with the torrent's other steps.
func (r *Runner) goStep(t *tor, f func(ctx context.Context)) {
	r.wg.Go(func() {
		if !t.lock(t.ctx) {
			return
		}
		defer t.unlock()
		f(t.ctx)
	})
}

// onMetadata handles a magnet Download whose metadata arrived: it saves the
// .torrent file, learns the real name and size and moves the Download to
// Queued. The torrent keeps running: if the Queue has room it goes on
// downloading at once (a pause would drop the peers that just delivered the
// metadata), and the next control pass pauses it if it has not been chosen.
func (r *Runner) onMetadata(ctx context.Context, t *tor) {
	d, err := r.cfg.Downloads.Get(ctx, t.id)
	if err != nil || d.State != core.StateFetchingMetadata {
		return
	}
	st, err := r.engine.Stats(t.hash)
	if err != nil || st.Size <= 0 {
		return // not yet; a later pass or event tries again
	}
	if err := r.saveTorrentFile(ctx, t, st); err != nil {
		r.log.Warn("torrent runner: cannot save the torrent file", "id", t.id, "err", err)
		return
	}
	if err := r.cfg.Downloads.UpdateDisplay(ctx, t.id, st.Name, st.Size); err != nil && !errors.Is(err, core.ErrNotFound) {
		r.log.Warn("torrent runner: cannot store the name", "id", t.id, "err", err)
	}
	if _, err := r.setState(ctx, t.id, core.StateChange{To: core.StateQueued, At: r.now()}); err != nil {
		if !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrIllegalTransition) {
			r.log.Warn("torrent runner: cannot queue the download", "id", t.id, "err", err)
		}
		return
	}
	r.poke()
}

// saveTorrentFile writes the .torrent file of a magnet torrent into the
// Download's directory (mode 0600: it may carry tracker passkeys) and records
// its path. It does nothing when the Download has the file already.
func (r *Runner) saveTorrentFile(ctx context.Context, t *tor, st core.TorrentStats) error {
	src, err := r.cfg.Sources.Get(ctx, t.id)
	if err != nil {
		return err
	}
	if src.TorrentPath != "" {
		return nil
	}
	raw, err := r.engine.Metainfo(t.hash)
	if err != nil {
		return err
	}
	path := filepath.Join(t.dir, metainfoFileName(st))
	if err := writeFileAtomic(path, raw); err != nil {
		return err
	}
	return r.cfg.Sources.SetTorrentPath(ctx, t.id, path)
}

// reservedMetainfoName is the fallback name of a stored .torrent file when
// the natural one would collide with the torrent's own data. It is the name
// app.AddService uses.
const reservedMetainfoName = ".bitnews-metainfo.torrent"

// metainfoFileName is the name of the stored .torrent file of a magnet
// Download, chosen like app.AddService chooses it for an uploaded file: the
// torrent's name made safe plus ".torrent". It must not be the name of
// anything the torrent itself puts directly into the Download directory (its
// root directory or top-level files), so a hostile torrent cannot make the
// engine write over its own metainfo.
func metainfoFileName(st core.TorrentStats) string {
	name, err := fsutil.SafeName(st.Name + ".torrent")
	if err != nil {
		return reservedMetainfoName
	}
	for _, f := range st.Files {
		top, _, _ := strings.Cut(f.Path, "/")
		if strings.EqualFold(top, name) {
			return reservedMetainfoName
		}
	}
	return name
}

// writeFileAtomic writes data to path through a temporary file, so a crash
// never leaves a half-written .torrent file behind.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".torrent-*")
	if err != nil {
		return fmt.Errorf("create torrent file: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o600)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write torrent file: %w", werr)
	}
	return nil
}
