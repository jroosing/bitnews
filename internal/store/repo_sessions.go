package store

import (
	"context"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// SessionRepo implements core.SessionRepo on SQLite. Only the SHA-256 hash of
// a session token is stored. Deleting a User deletes their Sessions (foreign
// key cascade).
type SessionRepo struct {
	db *DB
}

// NewSessionRepo returns a SessionRepo that stores Sessions in db.
func NewSessionRepo(db *DB) *SessionRepo { return &SessionRepo{db: db} }

// Create stores s. A taken token hash is core.ErrConflict.
func (r *SessionRepo) Create(ctx context.Context, s core.Session) error {
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at) VALUES (?, ?, ?, ?)`,
		s.TokenHash[:], string(s.UserID), toMillis(s.CreatedAt), toMillis(s.LastSeenAt))
	return mapErr("create session", err)
}

// Get returns the Session with the given token hash.
func (r *SessionRepo) Get(ctx context.Context, h core.TokenHash) (core.Session, error) {
	var (
		s              core.Session
		raw            []byte
		created, lastS int64
	)
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT token_hash, user_id, created_at, last_seen_at FROM sessions WHERE token_hash = ?`, h[:]).
		Scan(&raw, &s.UserID, &created, &lastS)
	if err != nil {
		return core.Session{}, mapErr("get session", err)
	}
	copy(s.TokenHash[:], raw)
	s.CreatedAt, s.LastSeenAt = fromMillis(created), fromMillis(lastS)
	return s, nil
}

// Touch sets the Session's last-seen time.
func (r *SessionRepo) Touch(ctx context.Context, h core.TokenHash, lastSeen time.Time) error {
	res, err := r.db.Writer().ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, toMillis(lastSeen), h[:])
	return mapExec("touch session", res, err)
}

// Delete removes the Session. An unknown hash is core.ErrNotFound.
func (r *SessionRepo) Delete(ctx context.Context, h core.TokenHash) error {
	res, err := r.db.Writer().ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, h[:])
	return mapExec("delete session", res, err)
}

// DeleteExpired removes the Sessions created before createdBefore or last
// seen before seenBefore and reports how many it removed.
func (r *SessionRepo) DeleteExpired(ctx context.Context, createdBefore, seenBefore time.Time) (int, error) {
	res, err := r.db.Writer().ExecContext(ctx,
		`DELETE FROM sessions WHERE created_at < ? OR last_seen_at < ?`,
		toMillis(createdBefore), toMillis(seenBefore))
	if err != nil {
		return 0, mapErr("delete expired sessions", err)
	}
	n, err := res.RowsAffected()
	return int(n), mapErr("delete expired sessions", err)
}
