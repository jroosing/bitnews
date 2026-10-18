package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"runtime"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func embeddedMigrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

// Option configures Open.
type Option func(*options)

type options struct {
	logger     *slog.Logger
	migrations fs.FS
}

// WithMigrations replaces the embedded migrations with the NNNN_name.sql
// files at the root of fsys. It exists so tests can inject broken or extra
// migrations.
func WithMigrations(fsys fs.FS) Option {
	return func(o *options) { o.migrations = fsys }
}

// WithLogger sets the logger used for warnings. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// DB is an open bitnews database: one writer connection and a read pool.
type DB struct {
	w *sql.DB
	r *sql.DB
}

// Writer returns the single-connection pool used for all writes. A
// transaction holding it blocks other writers, never readers.
func (d *DB) Writer() *sql.DB { return d.w }

// Reader returns the read-only pool. In WAL mode its connections are never
// blocked by the writer.
func (d *DB) Reader() *sql.DB { return d.r }

// Open creates (if needed) and opens the database at path.
func Open(ctx context.Context, path string, opts ...Option) (*DB, error) {
	o := options{logger: slog.Default(), migrations: embeddedMigrations()}
	for _, opt := range opts {
		opt(&o)
	}
	if err := ensureFile(path, o.logger); err != nil {
		return nil, err
	}
	w, err := openPool(ctx, path, true)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, w, o.migrations); err != nil {
		_ = w.Close()
		return nil, err
	}
	r, err := openPool(ctx, path, false)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	return &DB{w: w, r: r}, nil
}

// pragmas run on every new connection via the driver's _pragma DSN parameter.
var pragmas = []string{
	"journal_mode(WAL)",
	"foreign_keys(1)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
}

func openPool(ctx context.Context, path string, writer bool) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	if writer {
		// Take the write lock when a transaction begins, not on its first write.
		q.Set("_txlock", "immediate")
	} else {
		q.Add("_pragma", "query_only(1)")
	}
	u := url.URL{Path: path}
	db, err := sql.Open("sqlite", "file:"+u.EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if writer {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(max(4, runtime.NumCPU()))
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return db, nil
}

// WithTx runs fn in a write transaction on the writer connection. It commits
// if fn returns nil and rolls back otherwise. The write lock is taken when
// the transaction begins.
func (d *DB) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return runTx(ctx, d.w, fn)
}

func runTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, rollback(tx))
	}
	return tx.Commit()
}

func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}
	return nil
}

// Close closes the writer and the read pool.
func (d *DB) Close() error {
	return errors.Join(d.r.Close(), d.w.Close())
}

// ensureFile creates an empty database file with mode 0600 before SQLite
// ever sees it, so the file is never world-readable. An existing file with
// broader permissions is left alone and reported with a warning.
func ensureFile(path string, logger *slog.Logger) error {
	if info, err := os.Stat(path); err == nil {
		// Windows has no group/other permission bits to check.
		if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm&0o077 != 0 {
			logger.Warn("database file permissions are broader than 0600",
				"path", path, "mode", fmt.Sprintf("%#o", perm))
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", path, err)
	}
	return nil
}
