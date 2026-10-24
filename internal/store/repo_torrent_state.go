package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"

	"github.com/jroosing/bitnews/internal/core"
)

// sqliteConstraintForeignKey is SQLITE_CONSTRAINT_FOREIGNKEY.
const sqliteConstraintForeignKey = 787

// TorrentStateRepo keeps the core.TorrentRecord of torrent Downloads (table
// torrent_state): the seeding counters of earlier runs, the per-Download
// Share Limit and where the data lives after the move to complete.
type TorrentStateRepo struct{ db *DB }

var _ core.TorrentRecordRepo = (*TorrentStateRepo)(nil)

// NewTorrentStateRepo returns a TorrentStateRepo on db.
func NewTorrentStateRepo(db *DB) *TorrentStateRepo { return &TorrentStateRepo{db: db} }

// Get returns the record of the Download, or an error wrapping
// core.ErrNotFound.
func (r *TorrentStateRepo) Get(ctx context.Context, id core.ID) (core.TorrentRecord, error) {
	var (
		rec      core.TorrentRecord
		seconds  int64
		ratio    sql.NullFloat64
		limitSec sql.NullInt64
		location sql.NullString
		updated  int64
	)
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT uploaded, seeding_seconds, ratio_limit, seeding_limit_seconds, moved, location, updated_at
		 FROM torrent_state WHERE download_id = ?`, string(id)).
		Scan(&rec.Uploaded, &seconds, &ratio, &limitSec, &rec.Moved, &location, &updated)
	if err != nil {
		return core.TorrentRecord{}, mapErr("get torrent state", err)
	}
	rec.DownloadID = id
	rec.SeedingTime = time.Duration(seconds) * time.Second
	if ratio.Valid {
		rec.RatioLimit = &ratio.Float64
	}
	if limitSec.Valid {
		d := time.Duration(limitSec.Int64) * time.Second
		rec.SeedingTimeLimit = &d
	}
	rec.Location = location.String
	rec.UpdatedAt = fromMillis(updated)
	return rec, nil
}

// Upsert stores rec, replacing the previous record of the Download. A
// Download that does not exist is core.ErrNotFound. Durations are stored in
// whole seconds; UpdatedAt is set to now.
func (r *TorrentStateRepo) Upsert(ctx context.Context, rec core.TorrentRecord) error {
	var ratio sql.NullFloat64
	if rec.RatioLimit != nil {
		ratio = sql.NullFloat64{Float64: *rec.RatioLimit, Valid: true}
	}
	var limitSec sql.NullInt64
	if rec.SeedingTimeLimit != nil {
		limitSec = sql.NullInt64{Int64: durationSeconds(*rec.SeedingTimeLimit), Valid: true}
	}
	var location sql.NullString
	if rec.Location != "" {
		location = sql.NullString{String: rec.Location, Valid: true}
	}
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO torrent_state (download_id, uploaded, seeding_seconds, ratio_limit, seeding_limit_seconds,
			moved, location, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (download_id) DO UPDATE SET
			uploaded = excluded.uploaded, seeding_seconds = excluded.seeding_seconds,
			ratio_limit = excluded.ratio_limit, seeding_limit_seconds = excluded.seeding_limit_seconds,
			moved = excluded.moved, location = excluded.location, updated_at = excluded.updated_at`,
		string(rec.DownloadID), rec.Uploaded, durationSeconds(rec.SeedingTime), ratio, limitSec,
		rec.Moved, location, toMillis(time.Now()))
	if serr, ok := errors.AsType[*sqlite.Error](err); ok && serr.Code() == sqliteConstraintForeignKey {
		return fmt.Errorf("store: upsert torrent state: download %s: %w", rec.DownloadID, core.ErrNotFound)
	}
	return mapErr("upsert torrent state", err)
}

// Delete forgets the record. An unknown ID is not an error.
func (r *TorrentStateRepo) Delete(ctx context.Context, id core.ID) error {
	_, err := r.db.Writer().ExecContext(ctx, `DELETE FROM torrent_state WHERE download_id = ?`, string(id))
	return mapErr("delete torrent state", err)
}

// durationSeconds converts d to whole seconds, keeping a negative duration
// (the "unlimited" marker) negative.
func durationSeconds(d time.Duration) int64 {
	s := int64(d / time.Second)
	if s == 0 && d < 0 {
		return -1
	}
	return s
}
