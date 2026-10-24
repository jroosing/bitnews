package runner

import (
	"context"
	"errors"

	"github.com/jroosing/bitnews/internal/core"
)

// loadRecord returns the record of the Download, or an empty one when it has
// none.
func (r *Runner) loadRecord(ctx context.Context, id core.ID) (core.TorrentRecord, error) {
	rec, err := r.cfg.Records.Get(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return core.TorrentRecord{DownloadID: id}, nil
	}
	return rec, err
}

// updateRecord applies f to the record of the Download and stores it. All
// writers go through here, one at a time, so none overwrites another's field.
// A Download that no longer exists is not an error: its torrent is dropped
// on the next pass.
func (r *Runner) updateRecord(ctx context.Context, id core.ID, f func(*core.TorrentRecord)) error {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	rec, err := r.loadRecord(ctx, id)
	if err != nil {
		return err
	}
	f(&rec)
	rec.DownloadID = id
	if err := r.cfg.Records.Upsert(ctx, rec); err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	return nil
}
