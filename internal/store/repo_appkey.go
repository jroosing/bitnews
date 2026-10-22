package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// AppKeyRepo implements core.AppKeyRepo on SQLite. The table holds a single
// row (id 1) with the key in plain text (ADR-0006).
type AppKeyRepo struct {
	db *DB
}

// NewAppKeyRepo returns an AppKeyRepo that stores the app key in db.
func NewAppKeyRepo(db *DB) *AppKeyRepo { return &AppKeyRepo{db: db} }

// Get returns the app key. None stored is core.ErrNotFound.
func (r *AppKeyRepo) Get(ctx context.Context) (core.Secret, error) {
	var key string
	err := r.db.Reader().QueryRowContext(ctx, `SELECT key FROM app_key WHERE id = 1`).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNotFound
	}
	if err != nil {
		return "", mapErr("get app key", err)
	}
	return core.Secret(key), nil
}

// Set stores key as the one app key.
func (r *AppKeyRepo) Set(ctx context.Context, key core.Secret) error {
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO app_key (id, key, updated_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET key = excluded.key, updated_at = excluded.updated_at`,
		key.Reveal(), toMillis(time.Now()))
	return mapErr("set app key", err)
}
