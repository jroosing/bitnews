package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// settingsKey is the settings row that holds the typed core.Settings.
const settingsKey = "settings"

// SettingsRepo implements core.SettingsRepo on SQLite.
type SettingsRepo struct {
	db *DB
}

// NewSettingsRepo returns a SettingsRepo that stores the Settings in db.
func NewSettingsRepo(db *DB) *SettingsRepo { return &SettingsRepo{db: db} }

// Load returns the stored Settings. Fields missing from the stored JSON, and
// everything on a database that never saved Settings, take their
// core.DefaultSettings value.
func (r *SettingsRepo) Load(ctx context.Context) (core.Settings, error) {
	var raw string
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, settingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.DefaultSettings(), nil
	}
	if err != nil {
		return core.Settings{}, mapErr("load settings", err)
	}
	s := core.DefaultSettings()
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return core.Settings{}, fmt.Errorf("store: load settings: decode: %w", err)
	}
	return s, nil
}

// Save validates s and stores it, replacing any previous Settings. Invalid
// Settings are rejected before anything is written.
func (r *SettingsRepo) Save(ctx context.Context, s core.Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("store: save settings: encode: %w", err)
	}
	_, err = r.db.Writer().ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingsKey, string(raw), toMillis(time.Now()))
	return mapErr("save settings", err)
}
