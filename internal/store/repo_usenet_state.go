package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// MaxUsenetProgressBlob is the largest resume blob UsenetProgressRepo
// stores. A blob holds the file writer's snapshot and one record per
// resolved Article (a few bytes each), so even a very large release stays
// well below it.
const MaxUsenetProgressBlob = 16 << 20

// ErrBlobTooLarge is returned by UsenetProgressRepo.Save for a blob above
// MaxUsenetProgressBlob.
var ErrBlobTooLarge = errors.New("store: resume blob too large")

// UsenetProgressRepo keeps the opaque resume blob of Downloads that are
// being fetched (table usenet_progress). Its methods match
// runner.ProgressStore, which this package must not import.
type UsenetProgressRepo struct{ db *DB }

// NewUsenetProgressRepo returns a UsenetProgressRepo on db.
func NewUsenetProgressRepo(db *DB) *UsenetProgressRepo { return &UsenetProgressRepo{db: db} }

// Load returns the blob saved for the Download, or an error wrapping
// core.ErrNotFound.
func (r *UsenetProgressRepo) Load(ctx context.Context, id core.ID) ([]byte, error) {
	var blob []byte
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT blob FROM usenet_progress WHERE download_id = ?`, string(id)).Scan(&blob)
	if err != nil {
		return nil, mapErr("load usenet progress", err)
	}
	return blob, nil
}

// Save stores blob for the Download, replacing the previous one. A blob
// above MaxUsenetProgressBlob is refused and the previous one stays.
func (r *UsenetProgressRepo) Save(ctx context.Context, id core.ID, blob []byte) error {
	if len(blob) > MaxUsenetProgressBlob {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrBlobTooLarge, len(blob), MaxUsenetProgressBlob)
	}
	if blob == nil {
		blob = []byte{}
	}
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO usenet_progress (download_id, blob, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (download_id) DO UPDATE SET blob = excluded.blob, updated_at = excluded.updated_at`,
		string(id), blob, toMillis(time.Now()))
	return mapErr("save usenet progress", err)
}

// Delete forgets the blob. An unknown ID is not an error.
func (r *UsenetProgressRepo) Delete(ctx context.Context, id core.ID) error {
	_, err := r.db.Writer().ExecContext(ctx, `DELETE FROM usenet_progress WHERE download_id = ?`, string(id))
	return mapErr("delete usenet progress", err)
}

// UsenetReport is what a finished fetch recorded about a Download's
// Articles. It mirrors runner.FetchDetail, which this package cannot import;
// the daemon converts between them.
type UsenetReport struct {
	MissingArticles int
	MissingBytes    int64
	DamagedArticles int
	DamagedBytes    int64
	MissingFiles    int
}

// UsenetReportRepo keeps the UsenetReport of Fetched Downloads (table
// usenet_reports).
type UsenetReportRepo struct{ db *DB }

// NewUsenetReportRepo returns a UsenetReportRepo on db.
func NewUsenetReportRepo(db *DB) *UsenetReportRepo { return &UsenetReportRepo{db: db} }

// SaveReport stores the report of the Download, replacing the previous one.
func (r *UsenetReportRepo) SaveReport(ctx context.Context, id core.ID, rep UsenetReport) error {
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO usenet_reports (download_id, missing_articles, missing_bytes, damaged_articles,
			damaged_bytes, missing_files, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (download_id) DO UPDATE SET
			missing_articles = excluded.missing_articles, missing_bytes = excluded.missing_bytes,
			damaged_articles = excluded.damaged_articles, damaged_bytes = excluded.damaged_bytes,
			missing_files = excluded.missing_files, updated_at = excluded.updated_at`,
		string(id), rep.MissingArticles, rep.MissingBytes, rep.DamagedArticles, rep.DamagedBytes,
		rep.MissingFiles, toMillis(time.Now()))
	return mapErr("save usenet report", err)
}

// LoadReport returns the report of the Download, or an error wrapping
// core.ErrNotFound.
func (r *UsenetReportRepo) LoadReport(ctx context.Context, id core.ID) (UsenetReport, error) {
	var rep UsenetReport
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT missing_articles, missing_bytes, damaged_articles, damaged_bytes, missing_files
		 FROM usenet_reports WHERE download_id = ?`, string(id)).
		Scan(&rep.MissingArticles, &rep.MissingBytes, &rep.DamagedArticles, &rep.DamagedBytes, &rep.MissingFiles)
	if err != nil {
		return UsenetReport{}, mapErr("load usenet report", err)
	}
	return rep, nil
}

// UsenetUndeferredRepo records which PAR2 recovery volumes of a Download
// were un-deferred (table usenet_undeferred). Its methods match
// runner.VolumeStore.
type UsenetUndeferredRepo struct{ db *DB }

// NewUsenetUndeferredRepo returns a UsenetUndeferredRepo on db.
func NewUsenetUndeferredRepo(db *DB) *UsenetUndeferredRepo { return &UsenetUndeferredRepo{db: db} }

// Undeferred returns the un-deferred volumes of the Download as NZB file
// index to fetched.
func (r *UsenetUndeferredRepo) Undeferred(ctx context.Context, id core.ID) (map[int]bool, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT file_index, fetched FROM usenet_undeferred WHERE download_id = ?`, string(id))
	if err != nil {
		return nil, mapErr("list undeferred volumes", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int]bool{}
	for rows.Next() {
		var idx int
		var fetched bool
		if err := rows.Scan(&idx, &fetched); err != nil {
			return nil, mapErr("list undeferred volumes", err)
		}
		out[idx] = fetched
	}
	return out, mapErr("list undeferred volumes", rows.Err())
}

// Want records that the volumes at fileIndexes are wanted. A volume already
// recorded keeps its fetched flag.
func (r *UsenetUndeferredRepo) Want(ctx context.Context, id core.ID, fileIndexes []int) error {
	if len(fileIndexes) == 0 {
		return nil
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, idx := range fileIndexes {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO usenet_undeferred (download_id, file_index, fetched) VALUES (?, ?, 0)
				 ON CONFLICT (download_id, file_index) DO NOTHING`, string(id), idx); err != nil {
				return mapErr("want undeferred volumes", err)
			}
		}
		return nil
	})
}

// MarkFetched sets the fetched flag of the volumes at fileIndexes.
func (r *UsenetUndeferredRepo) MarkFetched(ctx context.Context, id core.ID, fileIndexes []int) error {
	if len(fileIndexes) == 0 {
		return nil
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, idx := range fileIndexes {
			if _, err := tx.ExecContext(ctx,
				`UPDATE usenet_undeferred SET fetched = 1 WHERE download_id = ? AND file_index = ?`,
				string(id), idx); err != nil {
				return mapErr("mark undeferred volumes fetched", err)
			}
		}
		return nil
	})
}
