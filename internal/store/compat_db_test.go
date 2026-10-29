package store_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/store"
)

// The Zig version (M15, ADR-0014) must open a database written by Go and the
// other way round. This test pins the Go side: a golden database and the
// schema dump of its sqlite_master, both read by the Zig tests. Run
// `go test ./internal/store -run Compat -update` after a new migration.
var update = flag.Bool("update", false, "regenerate the golden files in zig/testdata/compat")

const (
	goldenDir    = "../../zig/testdata/compat"
	goldenAppKey = "bitnews_golden_key_for_tests"
)

// schemaDump lists every schema object of the database as sorted
// "type|name|sql" lines, so two databases built by different code compare
// byte for byte.
func schemaDump(t *testing.T, db *store.DB) string {
	t.Helper()
	rows, err := db.Reader().Query(`SELECT type, name, coalesce(sql, '') FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	require.NoError(t, err)
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, sql string
		require.NoError(t, rows.Scan(&typ, &name, &sql))
		// Newlines inside the SQL are kept; objects are separated by a line of
		// "--" so the file stays readable and unambiguous.
		fmt.Fprintf(&b, "%s|%s|%s\n--\n", typ, name, sql)
	}
	require.NoError(t, rows.Err())
	return b.String()
}

func generateGolden(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bitnews.db")
	db, err := store.Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, store.NewAppKeyRepo(db).Set(ctx, goldenAppKey))
	for _, q := range []string{
		`INSERT INTO settings (key, value, updated_at) VALUES ('main', '{"queues":{"usenet":1}}', 1700000000000)`,
		`INSERT INTO categories (id, name, usenet_dir, torrent_dir) VALUES ('cat1', 'tv', '/data/tv', '/data/torrents/tv')`,
		`INSERT INTO servers (id, name, host, port, username, password, created_at)
			VALUES ('srv1', 'news', 'news.example.com', 563, 'user', '', 1700000000000)`,
	} {
		_, err := db.Writer().ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	dump := schemaDump(t, db)
	// Closing the last connection checkpoints the WAL and removes it, which
	// leaves one self-contained file.
	require.NoError(t, db.Close())
	for _, suffix := range []string{"-wal", "-shm"} {
		_, err := os.Stat(path + suffix)
		require.ErrorIs(t, err, os.ErrNotExist, suffix)
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(goldenDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(goldenDir, "bitnews.db"), raw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(goldenDir, "schema.txt"), []byte(dump), 0o644))
}

func TestCompatGoldenDatabase(t *testing.T) {
	if *update {
		generateGolden(t)
	}
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join(goldenDir, "bitnews.db"))
	require.NoError(t, err, "run: go test ./internal/store -run Compat -update")
	want, err := os.ReadFile(filepath.Join(goldenDir, "schema.txt"))
	require.NoError(t, err)

	// Open a copy: the committed file must never change.
	path := filepath.Join(t.TempDir(), "bitnews.db")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	db, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	key, err := store.NewAppKeyRepo(db).Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, goldenAppKey, key.Reveal())
	assert.Equal(t, string(want), schemaDump(t, db), "golden database differs from schema.txt")

	// The golden schema is the schema of every migration: a new migration
	// without `-update` fails here.
	fresh, _ := openTemp(t)
	assert.Equal(t, string(want), schemaDump(t, fresh), "migrations changed: run with -update")

	var servers, categories, settings int
	require.NoError(t, db.Reader().QueryRow(`SELECT (SELECT count(*) FROM servers), (SELECT count(*) FROM categories), (SELECT count(*) FROM settings)`).
		Scan(&servers, &categories, &settings))
	assert.Equal(t, [3]int{1, 1, 1}, [3]int{servers, categories, settings})
}
