package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// workDirs resolves the working directory of a Download.
//
// Changing the incomplete root in the Settings affects only new Downloads,
// so the directory of an existing one must not be recomputed from the
// current Settings. AddService created it as <incomplete root>/<id> and
// stored the NZB inside it, so the directory is the parent of the stored NZB
// path. It is cached per Download until the Download is removed. Only when
// there is no source row (a torrent, or a Download that has none yet) does the
// current Settings decide, and that answer is not cached.
type workDirs struct {
	sources  core.DownloadSourceRepo
	settings func() core.Settings
	log      *slog.Logger

	mu    sync.Mutex
	cache map[core.ID]string
}

func newWorkDirs(sources core.DownloadSourceRepo, settings func() core.Settings, log *slog.Logger) *workDirs {
	return &workDirs{sources: sources, settings: settings, log: log, cache: map[core.ID]string{}}
}

// dirFor implements the DirFor function of the runner and the Post-processor.
func (w *workDirs) dirFor(d core.Download) string {
	w.mu.Lock()
	dir, ok := w.cache[d.ID]
	w.mu.Unlock()
	if ok {
		return dir
	}
	if d.SourceKind == core.SourceNZB {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		path, _, err := w.sources.Get(ctx, d.ID)
		cancel()
		// A password that cannot be decrypted still comes with the path.
		if path != "" {
			dir = filepath.Dir(path)
			w.mu.Lock()
			w.cache[d.ID] = dir
			w.mu.Unlock()
			return dir
		}
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			w.log.Debug("working directory: no stored source, using the settings", "id", d.ID, "err", err)
		}
	}
	roots := w.settings().Paths.Usenet
	if d.SourceKind == core.SourceTorrent {
		roots = w.settings().Paths.Torrent
	}
	return filepath.Join(roots.Incomplete, string(d.ID))
}

// forget drops the cached directory of a removed Download.
func (w *workDirs) forget(id core.ID) {
	w.mu.Lock()
	delete(w.cache, id)
	w.mu.Unlock()
}

// forgetRemoved forgets Downloads as they are removed, until ctx ends.
func (w *workDirs) forgetRemoved(ctx context.Context, bus *core.Bus) {
	sub := bus.Subscribe(ctx)
	defer sub.Close()
	for ev := range sub.Events() {
		if rm, ok := ev.(core.Removed); ok {
			w.forget(rm.ID)
		}
	}
}
