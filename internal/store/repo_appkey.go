package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// appKeyAAD binds the sealed key to its one row.
var appKeyAAD = AAD("app_key", "sealed_key", "1")

// AppKeyRepo implements core.AppKeyRepo on SQLite. The table holds a single
// row (id 1) whose key is sealed with the master Key.
type AppKeyRepo struct {
	db  *DB
	key Key
}

// NewAppKeyRepo returns an AppKeyRepo that stores the app key in db, sealed
// with key.
func NewAppKeyRepo(db *DB, key Key) *AppKeyRepo { return &AppKeyRepo{db: db, key: key} }

// Get returns the app key. None stored is core.ErrNotFound; one that the
// master key cannot open is core.ErrAppKeyUnreadable.
func (r *AppKeyRepo) Get(ctx context.Context) (core.Secret, error) {
	var sealed string
	err := r.db.Reader().QueryRowContext(ctx, `SELECT sealed_key FROM app_key WHERE id = 1`).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNotFound
	}
	if err != nil {
		return "", mapErr("get app key", err)
	}
	plain, err := Unseal(r.key, sealed, appKeyAAD)
	switch {
	case err == nil:
		return core.Secret(plain), nil
	case errors.Is(err, ErrDecrypt), errors.Is(err, ErrUnsupportedFormat):
		return "", core.ErrAppKeyUnreadable
	}
	return "", fmt.Errorf("store: get app key: %w", err)
}

// Set seals key and stores it as the one app key.
func (r *AppKeyRepo) Set(ctx context.Context, key core.Secret) error {
	sealed, err := Seal(r.key, key.Reveal(), appKeyAAD)
	if err != nil {
		return fmt.Errorf("store: seal app key: %w", err)
	}
	_, err = r.db.Writer().ExecContext(ctx,
		`INSERT INTO app_key (id, sealed_key, updated_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET sealed_key = excluded.sealed_key, updated_at = excluded.updated_at`,
		sealed, toMillis(time.Now()))
	return mapErr("set app key", err)
}
