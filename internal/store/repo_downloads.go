package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// DownloadRepo implements core.DownloadRepo on SQLite.
type DownloadRepo struct {
	db *DB
}

// NewDownloadRepo returns a DownloadRepo that stores Downloads in db.
func NewDownloadRepo(db *DB) *DownloadRepo { return &DownloadRepo{db: db} }

const downloadColumns = `id, name, source_kind, category_id, state, priority, queue_position,
	paused, failure_reason, failure_detail, total_bytes, done_bytes,
	added_at, fetched_at, ready_at, updated_at, output_path`

// Create stores d.
func (r *DownloadRepo) Create(ctx context.Context, d core.Download) error {
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO downloads (`+downloadColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(d.ID), d.Name, string(d.SourceKind), nullID(d.CategoryID), string(d.State),
		string(d.Priority), d.QueuePosition, d.Paused, nullString(string(d.FailureReason)),
		d.FailureDetail, d.TotalBytes, d.DoneBytes,
		toMillis(d.AddedAt), nullMillis(d.FetchedAt), nullMillis(d.ReadyAt), toMillis(d.UpdatedAt),
		d.OutputPath)
	return mapErr("create download", err)
}

// Get returns the Download with the given ID.
func (r *DownloadRepo) Get(ctx context.Context, id core.ID) (core.Download, error) {
	d, err := scanDownload(r.db.Reader().QueryRowContext(ctx,
		`SELECT `+downloadColumns+` FROM downloads WHERE id = ?`, string(id)))
	return d, mapErr("get download", err)
}

// List paging limits: a query with no Limit gets DefaultListLimit rows and
// no query gets more than MaxListLimit.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ErrInvalidCursor is returned by List for a DownloadQuery.After that this
// repository did not hand out.
var ErrInvalidCursor = errors.New("invalid cursor")

// List returns one page of Downloads ordered by (AddedAt, ID) and the cursor
// of the next page, which is empty on the last page. The cursor is keyset
// based, so Downloads inserted between two calls never duplicate or skip a
// Download that was already there.
func (r *DownloadRepo) List(ctx context.Context, q core.DownloadQuery) ([]core.Download, string, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)

	var (
		where []string
		args  []any
	)
	if q.State != "" {
		where = append(where, "state = ?")
		args = append(args, string(q.State))
	}
	if q.CategoryID != "" {
		where = append(where, "category_id = ?")
		args = append(args, string(q.CategoryID))
	}
	if q.SourceKind != "" {
		where = append(where, "source_kind = ?")
		args = append(args, string(q.SourceKind))
	}
	if q.After != "" {
		addedAt, id, err := decodeCursor(q.After)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(added_at, id) > (?, ?)")
		args = append(args, addedAt, string(id))
	}

	query := `SELECT ` + downloadColumns + ` FROM downloads`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// One extra row tells whether another page follows.
	query += " ORDER BY added_at, id LIMIT ?"
	args = append(args, limit+1)

	rows, err := r.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", mapErr("list downloads", err)
	}
	defer func() { _ = rows.Close() }()
	page := []core.Download{}
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, "", mapErr("list downloads", err)
		}
		page = append(page, d)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapErr("list downloads", err)
	}
	if len(page) <= limit {
		return page, "", nil
	}
	page = page[:limit]
	last := page[limit-1]
	return page, encodeCursor(toMillis(last.AddedAt), last.ID), nil
}

func encodeCursor(addedAt int64, id core.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(addedAt, 10) + ":" + string(id)))
}

func decodeCursor(cursor string) (addedAt int64, id core.ID, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", fmt.Errorf("store: list downloads: %w: %w", ErrInvalidCursor, err)
	}
	ms, rest, ok := strings.Cut(string(raw), ":")
	if !ok || rest == "" {
		return 0, "", fmt.Errorf("store: list downloads: %w", ErrInvalidCursor)
	}
	addedAt, err = strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("store: list downloads: %w: %w", ErrInvalidCursor, err)
	}
	return addedAt, core.ID(rest), nil
}

// Update stores every field of d except ID, AddedAt (immutable, so the
// (AddedAt, ID) paging order and its cursors stay stable), State and the
// fields only a StateChange sets (FetchedAt, ReadyAt, OutputPath,
// FailureReason, FailureDetail).
func (r *DownloadRepo) Update(ctx context.Context, d core.Download) error {
	res, err := r.db.Writer().ExecContext(ctx,
		`UPDATE downloads SET name = ?, source_kind = ?, category_id = ?, priority = ?,
			queue_position = ?, paused = ?, total_bytes = ?, done_bytes = ?,
			updated_at = ?
		WHERE id = ?`,
		d.Name, string(d.SourceKind), nullID(d.CategoryID), string(d.Priority),
		d.QueuePosition, d.Paused, d.TotalBytes, d.DoneBytes,
		toMillis(d.UpdatedAt), string(d.ID))
	return mapExec("update download", res, err)
}

// UpdateState loads the Download, applies c with Download.ChangeState and
// stores the result, all in one write transaction. Core rejects illegal
// moves, so a rejected change leaves the row untouched.
func (r *DownloadRepo) UpdateState(ctx context.Context, id core.ID, c core.StateChange) (core.Download, error) {
	var d core.Download
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		d, err = scanDownload(tx.QueryRowContext(ctx,
			`SELECT `+downloadColumns+` FROM downloads WHERE id = ?`, string(id)))
		if err != nil {
			return mapErr("update download state", err)
		}
		if err := d.ChangeState(c); err != nil {
			return fmt.Errorf("store: update download state: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE downloads SET state = ?, failure_reason = ?, failure_detail = ?,
				fetched_at = ?, ready_at = ?, output_path = ?, updated_at = ?
			WHERE id = ?`,
			string(d.State), nullString(string(d.FailureReason)), d.FailureDetail,
			nullMillis(d.FetchedAt), nullMillis(d.ReadyAt), d.OutputPath,
			toMillis(d.UpdatedAt), string(d.ID))
		return mapErr("update download state", err)
	})
	if err != nil {
		return core.Download{}, err
	}
	return d, nil
}

// UpdateProgress stores done_bytes, total_bytes (when totalBytes is
// positive) and updated_at in one statement, leaving every other column as
// it is, so it cannot clobber a concurrent Update.
func (r *DownloadRepo) UpdateProgress(ctx context.Context, id core.ID, doneBytes, totalBytes int64) error {
	if doneBytes < 0 {
		return fmt.Errorf("store: update download progress: negative done bytes %d", doneBytes)
	}
	res, err := r.db.Writer().ExecContext(ctx,
		`UPDATE downloads SET done_bytes = ?, updated_at = ?,
			total_bytes = CASE WHEN ? > 0 THEN ? ELSE total_bytes END
		WHERE id = ?`,
		doneBytes, toMillis(time.Now()), totalBytes, totalBytes, string(id))
	return mapExec("update download progress", res, err)
}

// UpdateDisplay stores name (when it is not empty) and total_bytes (when
// totalBytes is positive) and refreshes updated_at in one statement, leaving
// every other column as it is.
func (r *DownloadRepo) UpdateDisplay(ctx context.Context, id core.ID, name string, totalBytes int64) error {
	res, err := r.db.Writer().ExecContext(ctx,
		`UPDATE downloads SET updated_at = ?,
			name = CASE WHEN ? <> '' THEN ? ELSE name END,
			total_bytes = CASE WHEN ? > 0 THEN ? ELSE total_bytes END
		WHERE id = ?`,
		toMillis(time.Now()), name, name, totalBytes, totalBytes, string(id))
	return mapExec("update download display", res, err)
}

// Delete removes the Download.
func (r *DownloadRepo) Delete(ctx context.Context, id core.ID) error {
	res, err := r.db.Writer().ExecContext(ctx, `DELETE FROM downloads WHERE id = ?`, string(id))
	return mapExec("delete download", res, err)
}

// DeleteUnlessIn deletes the Download unless its State is one of states, in
// one transaction, so a concurrent state change cannot slip in between the
// check and the delete. A Download in one of the states is core.ErrConflict.
func (r *DownloadRepo) DeleteUnlessIn(ctx context.Context, id core.ID, states []core.State) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var st string
		err := tx.QueryRowContext(ctx, `SELECT state FROM downloads WHERE id = ?`, string(id)).Scan(&st)
		if err != nil {
			return mapErr("delete download", err)
		}
		if slices.Contains(states, core.State(st)) {
			return fmt.Errorf("store: delete download in state %s: %w", st, core.ErrConflict)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM downloads WHERE id = ?`, string(id))
		return mapExec("delete download", res, err)
	})
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDownload(row rowScanner) (core.Download, error) {
	var (
		d                       core.Download
		category, failureReason sql.NullString
		fetched, ready          sql.NullInt64
		added, updated          int64
	)
	err := row.Scan(&d.ID, &d.Name, &d.SourceKind, &category, &d.State, &d.Priority,
		&d.QueuePosition, &d.Paused, &failureReason, &d.FailureDetail, &d.TotalBytes,
		&d.DoneBytes, &added, &fetched, &ready, &updated, &d.OutputPath)
	if err != nil {
		return core.Download{}, err
	}
	d.CategoryID = core.ID(category.String)
	d.FailureReason = core.FailureReason(failureReason.String)
	d.AddedAt = fromMillis(added)
	d.FetchedAt = fromNullMillis(fetched)
	d.ReadyAt = fromNullMillis(ready)
	d.UpdatedAt = fromMillis(updated)
	return d, nil
}

// toMillis encodes t as unix epoch milliseconds.
func toMillis(t time.Time) int64 { return t.UnixMilli() }

// nullMillis encodes t like toMillis, but the zero time as NULL.
func nullMillis(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: toMillis(t), Valid: true}
}

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func fromNullMillis(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return fromMillis(ms.Int64)
}

// nullString encodes the empty string as NULL.
func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullID(id core.ID) sql.NullString { return nullString(string(id)) }
