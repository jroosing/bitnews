package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"time"
)

type migration struct {
	version int
	name    string
	sql     string
}

var migrationFile = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// loadMigrations reads files named NNNN_name.sql from the root of fsys and
// checks that their versions run 1, 2, 3, ... without gaps or duplicates.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var ms []migration
	for _, e := range entries {
		m := migrationFile.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			return nil, fmt.Errorf("store: migration file %q is not named NNNN_name.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: version, name: m[2], sql: string(body)})
	}
	slices.SortFunc(ms, func(a, b migration) int { return a.version - b.version })
	for i, m := range ms {
		switch want := i + 1; {
		case m.version == want:
		case i > 0 && m.version == ms[i-1].version:
			return nil, fmt.Errorf("store: duplicate migration version %04d (%s and %s)", m.version, ms[i-1].name, m.name)
		default:
			return nil, fmt.Errorf("store: migration %04d is missing: found %04d_%s next", want, m.version, m.name)
		}
	}
	return ms, nil
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL,
	applied_at INTEGER NOT NULL
)`

// migrate applies every migration in fsys that is not yet recorded, each in
// its own transaction together with its schema_migrations row.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	if err := refuseNewerSchema(ctx, db, len(ms)); err != nil {
		return err
	}
	for _, m := range ms {
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("store: migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	return runTx(ctx, db, func(tx *sql.Tx) error {
		var done int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE version = ?", m.version).Scan(&done); {
		case err == nil:
			return nil // already applied, possibly by another process
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
			m.version, m.name, time.Now().UnixMilli())
		return err
	})
}

// refuseNewerSchema fails when the database records migrations this binary
// does not know, which means a newer version wrote to it. Migrations are
// forward-only, so running an older binary on it is unsafe.
func refuseNewerSchema(ctx context.Context, db *sql.DB, known int) error {
	var newest sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&newest); err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	if newest.Valid && newest.Int64 > int64(known) {
		return fmt.Errorf("store: database schema is newer than this binary (database at migration %04d, binary knows up to %04d); upgrade bitnews or restore an older database",
			newest.Int64, known)
	}
	return nil
}
