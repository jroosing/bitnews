package runner

import (
	"context"
	"errors"

	"github.com/jroosing/bitnews/internal/core"
)

// sample reads the stats of the torrents the Queue runs and reports their
// progress to the Queue.
func (r *Runner) sample(ctx context.Context) {
	for _, t := range r.runningTors() {
		st, err := r.engine.Stats(t.hash)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				r.log.Warn("torrent runner: the engine lost a torrent", "id", t.id)
			}
			continue
		}
		// Seeding, not DoneBytes: DoneBytes counts written chunks whose piece
		// is not hashed yet.
		if st.State == core.TorrentSeeding {
			r.fetched(t) // the event may have been dropped
			continue
		}
		if st.Size > 0 { // nothing is known before the metadata
			r.q.Progress(ctx, t.id, st.DoneBytes, st.Size, st.DownRate)
		}
	}
}
