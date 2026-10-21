package runner

import (
	"context"
	"os"
	"path/filepath"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// locate decides where the Download's directory is. It is the directory of
// the stored .torrent file (or, for a magnet whose metadata has not arrived,
// <torrent incomplete root>/<id>), unless the torrent was moved to complete.
//
// A record with a location but Moved false is a move that was started and not
// confirmed (a crash in the middle). The move renames the directory, so the
// data is at the original place if the move did not happen and at the
// location if it did; the record is settled accordingly.
func (r *Runner) locate(ctx context.Context, settings core.Settings, d core.Download, src core.TorrentSource, rec core.TorrentRecord) string {
	orig := filepath.Join(settings.Paths.Torrent.Incomplete, string(d.ID))
	if src.TorrentPath != "" {
		orig = filepath.Dir(src.TorrentPath)
	}
	switch {
	case rec.Location == "":
		return orig
	case rec.Moved:
		return rec.Location
	case exists(orig):
		_ = r.updateRecord(ctx, d.ID, func(rec *core.TorrentRecord) { rec.Location = "" })
		return orig
	case exists(rec.Location):
		loc := rec.Location
		_ = r.updateRecord(ctx, d.ID, func(rec *core.TorrentRecord) { rec.Moved = true })
		r.followTorrentFile(ctx, d.ID, loc)
		return loc
	}
	return orig
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
