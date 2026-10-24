package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/store"
)

func openTemp(t *testing.T, opts ...store.Option) (*store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bitnews.db")
	db, err := store.Open(context.Background(), path, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenCreatesFileWithMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	_, path := openTemp(t)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestOpenWarnsButSucceedsOnBroaderExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	path := filepath.Join(t.TempDir(), "bitnews.db")
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	require.NoError(t, os.Chmod(path, 0o644))
	logger, logs := captureLogger()

	db, err := store.Open(context.Background(), path, store.WithLogger(logger))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	require.Contains(t, logs.String(), "level=WARN")
	require.Contains(t, logs.String(), "permissions")
	require.Contains(t, logs.String(), path)
}

func TestOpenDoesNotWarnForOwnerOnlyPermissions(t *testing.T) {
	logger, logs := captureLogger()
	db, _ := openTemp(t, store.WithLogger(logger))
	require.NoError(t, db.Close())

	require.NotContains(t, logs.String(), "level=WARN")
}

func pragma(t *testing.T, c *sql.Conn, name string) string {
	t.Helper()
	var v string
	require.NoError(t, c.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v))
	return v
}

func TestPragmasApplyToEveryConnection(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	// Hold several reader connections at once so the pool must open new ones.
	var conns []*sql.Conn
	for range 3 {
		c, err := db.Reader().Conn(ctx)
		require.NoError(t, err)
		defer c.Close()
		conns = append(conns, c)
	}
	w, err := db.Writer().Conn(ctx)
	require.NoError(t, err)
	defer w.Close()
	conns = append(conns, w)

	for _, c := range conns {
		require.Equal(t, "wal", pragma(t, c, "journal_mode"))
		require.Equal(t, "1", pragma(t, c, "foreign_keys"))
		require.Equal(t, "5000", pragma(t, c, "busy_timeout"))
		require.Equal(t, "1", pragma(t, c, "synchronous"), "NORMAL")
	}
}

func migrations(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

type appliedRow struct {
	Version int
	Name    string
}

func applied(t *testing.T, db *sql.DB) []appliedRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT version, name FROM schema_migrations ORDER BY version")
	require.NoError(t, err)
	defer rows.Close()
	var out []appliedRow
	for rows.Next() {
		var r appliedRow
		require.NoError(t, rows.Scan(&r.Version, &r.Name))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestOpenAppliesMigrationsInOrderAndRecordsThem(t *testing.T) {
	db, _ := openTemp(t, store.WithMigrations(migrations(map[string]string{
		"0002_second.sql": "CREATE TABLE second (id INTEGER PRIMARY KEY, first_id INTEGER REFERENCES first(id));",
		"0001_first.sql":  "CREATE TABLE first (id INTEGER PRIMARY KEY);",
	})))

	assert.Equal(t, []appliedRow{{1, "first"}, {2, "second"}}, applied(t, db.Reader()))
	var appliedAt int64
	require.NoError(t, db.Reader().QueryRow("SELECT applied_at FROM schema_migrations WHERE version = 1").Scan(&appliedAt))
	assert.InDelta(t, time.Now().UnixMilli(), appliedAt, float64(time.Minute.Milliseconds()))
}

func TestSecondOpenIsANoOp(t *testing.T) {
	ctx := context.Background()
	fsys := migrations(map[string]string{
		"0001_items.sql": "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);",
	})
	path := filepath.Join(t.TempDir(), "bitnews.db")

	first, err := store.Open(ctx, path, store.WithMigrations(fsys))
	require.NoError(t, err)
	_, err = first.Writer().ExecContext(ctx, "INSERT INTO items (name) VALUES ('kept')")
	require.NoError(t, err)
	var firstAppliedAt int64
	require.NoError(t, first.Reader().QueryRow("SELECT applied_at FROM schema_migrations").Scan(&firstAppliedAt))
	require.NoError(t, first.Close())

	second, err := store.Open(ctx, path, store.WithMigrations(fsys))
	require.NoError(t, err)
	defer second.Close()

	assert.Equal(t, []appliedRow{{1, "items"}}, applied(t, second.Reader()))
	var secondAppliedAt int64
	require.NoError(t, second.Reader().QueryRow("SELECT applied_at FROM schema_migrations").Scan(&secondAppliedAt))
	assert.Equal(t, firstAppliedAt, secondAppliedAt)
	var n int
	require.NoError(t, second.Reader().QueryRow("SELECT count(*) FROM items").Scan(&n))
	assert.Equal(t, 1, n)
}

func rawTables(t *testing.T, path string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	defer raw.Close()
	rows, err := raw.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}

func TestFailedMigrationRollsBackCompletely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bitnews.db")
	fsys := migrations(map[string]string{
		"0001_good.sql": "CREATE TABLE good (id INTEGER PRIMARY KEY);",
		// The first statement succeeds, the second fails: nothing may survive.
		"0002_bad.sql": "CREATE TABLE partial (id INTEGER PRIMARY KEY); INSERT INTO nonexistent VALUES (1);",
	})

	db, err := store.Open(context.Background(), path, store.WithMigrations(fsys))

	require.Error(t, err)
	require.Nil(t, db)
	assert.ErrorContains(t, err, "0002_bad")
	assert.Equal(t, []string{"good", "schema_migrations"}, rawTables(t, path))

	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	defer raw.Close()
	assert.Equal(t, []appliedRow{{1, "good"}}, applied(t, raw))
}

func TestOpenRejectsMalformedMigrationSets(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"gap", map[string]string{"0001_a.sql": "SELECT 1;", "0003_c.sql": "SELECT 1;"}, "0002"},
		{"must start at one", map[string]string{"0002_b.sql": "SELECT 1;"}, "0001"},
		{"duplicate version", map[string]string{"0001_a.sql": "SELECT 1;", "0001_b.sql": "SELECT 1;"}, "duplicate"},
		{"bad file name", map[string]string{"0001_a.sql": "SELECT 1;", "notes.sql": "SELECT 1;"}, "notes.sql"},
		{"empty name", map[string]string{"0001_.sql": "SELECT 1;"}, "0001_.sql"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bitnews.db")

			_, err := store.Open(context.Background(), path, store.WithMigrations(migrations(tt.files)))

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
			assert.Empty(t, rawTables(t, path), "nothing may be applied from a malformed set")
		})
	}
}

func TestOpenRefusesDatabaseNewerThanThisBinary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bitnews.db")
	newer := migrations(map[string]string{
		"0001_a.sql": "CREATE TABLE a (id INTEGER PRIMARY KEY);",
		"0002_b.sql": "CREATE TABLE b (id INTEGER PRIMARY KEY);",
	})
	db, err := store.Open(ctx, path, store.WithMigrations(newer))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	older := migrations(map[string]string{
		"0001_a.sql": "CREATE TABLE a (id INTEGER PRIMARY KEY);",
	})
	_, err = store.Open(ctx, path, store.WithMigrations(older))

	require.Error(t, err)
	assert.ErrorContains(t, err, "newer")
	assert.ErrorContains(t, err, "0002")
}

var parentChild = map[string]string{
	"0001_tables.sql": `
		CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id));`,
}

func count(t *testing.T, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, table string) int {
	t.Helper()
	var n int
	require.NoError(t, q.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n))
	return n
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db, _ := openTemp(t, store.WithMigrations(migrations(parentChild)))

	_, err := db.Writer().ExecContext(context.Background(), "INSERT INTO child (parent_id) VALUES (42)")

	require.Error(t, err)
	assert.ErrorContains(t, err, "FOREIGN KEY")
}

func TestReaderRejectsWrites(t *testing.T) {
	db, _ := openTemp(t, store.WithMigrations(migrations(parentChild)))

	_, err := db.Reader().ExecContext(context.Background(), "INSERT INTO parent (id) VALUES (1)")

	require.Error(t, err)
}

func TestWithTxCommitsOnSuccessAndRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t, store.WithMigrations(migrations(parentChild)))

	require.NoError(t, db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO parent (id) VALUES (1)")
		return err
	}))
	boom := errors.New("boom")
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO parent (id) VALUES (2)"); err != nil {
			return err
		}
		return boom
	})

	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1, count(t, db.Reader(), "parent"))
}

func TestReadersAreNotBlockedWhileWriterHoldsATransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, _ := openTemp(t, store.WithMigrations(migrations(parentChild)))
	require.NoError(t, db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO parent (id) VALUES (1)")
		return err
	}))

	var duringTx int
	require.NoError(t, db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO parent (id) VALUES (2)"); err != nil {
			return err
		}
		var wg sync.WaitGroup
		results := make([]int, 4)
		for i := range results {
			wg.Go(func() {
				results[i] = count(t, db.Reader(), "parent")
			})
		}
		wg.Wait()
		for _, n := range results {
			duringTx = n
			if n != 1 {
				return fmt.Errorf("reader saw %d rows, want the committed 1", n)
			}
		}
		return nil
	}))

	assert.Equal(t, 1, duringTx, "readers see the last committed state")
	assert.Equal(t, 2, count(t, db.Reader(), "parent"), "and the new state after commit")
}
