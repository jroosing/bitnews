package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/store"
)

// The statements below mirror the fields of the core types (Download,
// Category, Settings, User, Session, APIKey) so that the repositories can be
// implemented on this schema without further migrations.
const (
	insertCategory = `INSERT INTO categories (id, name, dir_override) VALUES ('cat1', 'tv', '/data/tv')`
	insertDownload = `INSERT INTO downloads (
		id, name, source_kind, category_id, state, priority, queue_position, paused,
		failure_reason, failure_detail, total_bytes, done_bytes,
		added_at, fetched_at, ready_at, updated_at, output_path)
		VALUES ('dl1', 'Some.Show.S01E01', 'nzb', 'cat1', 'ready', 'normal', 3, 0,
		NULL, '', 1000, 1000, 1700000000000, 1700000001000, 1700000002000, 1700000002000, '/data/tv/Some.Show')`
	insertSettings = `INSERT INTO settings (key, value, updated_at) VALUES ('main', '{"queues":{"usenet":1}}', 1700000000000)`
	insertUser     = `INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'admin', x'24617267', 1700000000000)`
	insertSession  = `INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at)
		VALUES (zeroblob(32), 'u1', 1700000000000, 1700000000000)`
	insertAPIKey = `INSERT INTO api_keys (id, name, scope, hash, created_at, last_used_at)
		VALUES ('k1', 'sonarr', 'write', zeroblob(32), 1700000000000, NULL)`
)

func openDefault(t *testing.T) *store.DB {
	t.Helper()
	db, _ := openTemp(t)
	return db
}

func exec(t *testing.T, db *store.DB, query string) {
	t.Helper()
	_, err := db.Writer().ExecContext(context.Background(), query)
	require.NoError(t, err, query)
}

func TestFreshDatabaseHasTheM1Schema(t *testing.T) {
	db := openDefault(t)

	rows, err := db.Reader().Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	require.NoError(t, err)
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []string{"api_keys", "categories", "downloads", "schema_migrations", "sessions", "settings", "users"}, tables)
	assert.Equal(t, []appliedRow{{1, "initial"}}, applied(t, db.Reader()))
}

func TestSchemaStoresARowForEachTable(t *testing.T) {
	db := openDefault(t)

	for _, stmt := range []string{insertCategory, insertDownload, insertSettings, insertUser, insertSession, insertAPIKey} {
		exec(t, db, stmt)
	}

	for _, table := range []string{"categories", "downloads", "settings", "users", "sessions", "api_keys"} {
		assert.Equal(t, 1, count(t, db.Reader(), table), table)
	}
}

func TestSchemaAcceptsEveryCoreEnumValue(t *testing.T) {
	db := openDefault(t)
	i := 0
	insert := func(state, priority, kind string, reason any) error {
		i++
		_, err := db.Writer().Exec(`INSERT INTO downloads (id, name, source_kind, state, priority, failure_reason, added_at, updated_at)
			VALUES (?, 'n', ?, ?, ?, ?, 1, 1)`, i, kind, state, priority, reason)
		return err
	}

	for _, state := range []string{"added", "fetching_metadata", "queued", "fetching", "fetched", "post_processing", "ready", "failed"} {
		require.NoError(t, insert(state, "normal", "nzb", nil), state)
	}
	for _, p := range []string{"force", "high", "normal", "low"} {
		require.NoError(t, insert("added", p, "torrent", nil), p)
	}
	for _, r := range []string{"missing_articles", "repair_insufficient", "archive_unsupported", "archive_password",
		"archive_corrupt", "disk_full", "path_violation", "source_invalid", "internal"} {
		require.NoError(t, insert("failed", "normal", "nzb", r), r)
	}
}

func TestSchemaRejectsInvalidEnumValues(t *testing.T) {
	db := openDefault(t)
	exec(t, db, insertUser)
	tests := map[string]string{
		"download state":          strings.Replace(insertDownload, "'ready'", "'bogus'", 1),
		"download priority":       strings.Replace(insertDownload, "'normal'", "'urgent'", 1),
		"download source kind":    strings.Replace(insertDownload, "'nzb'", "'ftp'", 1),
		"download failure reason": strings.Replace(insertDownload, "NULL,", "'bogus',", 1),
		"download paused":         strings.Replace(insertDownload, "3, 0,", "3, 2,", 1),
		"api key scope":           strings.Replace(insertAPIKey, "'write'", "'admin'", 1),
		"session token length":    strings.Replace(insertSession, "zeroblob(32)", "zeroblob(16)", 1),
		"api key hash length":     strings.Replace(insertAPIKey, "zeroblob(32)", "zeroblob(16)", 1),
		"settings not json":       strings.Replace(insertSettings, `'{"queues":{"usenet":1}}'`, "'not json'", 1),
	}
	exec(t, db, insertCategory)

	for name, stmt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := db.Writer().Exec(stmt)
			require.Error(t, err)
			assert.ErrorContains(t, err, "constraint")
		})
	}
}

func TestDeletingACategoryDetachesItsDownloads(t *testing.T) {
	db := openDefault(t)
	exec(t, db, insertCategory)
	exec(t, db, insertDownload)

	exec(t, db, `DELETE FROM categories WHERE id = 'cat1'`)

	var categoryID *string
	require.NoError(t, db.Reader().QueryRow(`SELECT category_id FROM downloads WHERE id = 'dl1'`).Scan(&categoryID))
	assert.Nil(t, categoryID)
}

func TestDeletingAUserDeletesTheirSessions(t *testing.T) {
	db := openDefault(t)
	exec(t, db, insertUser)
	exec(t, db, insertSession)

	exec(t, db, `DELETE FROM users WHERE id = 'u1'`)

	assert.Equal(t, 0, count(t, db.Reader(), "sessions"))
}

func TestSchemaRejectsOrphansAndDuplicates(t *testing.T) {
	db := openDefault(t)
	exec(t, db, insertCategory)
	exec(t, db, insertUser)
	exec(t, db, insertSession)
	exec(t, db, insertAPIKey)
	tests := map[string]string{
		"download with unknown category": strings.Replace(insertDownload, "'cat1', 'ready'", "'nope', 'ready'", 1),
		"session with unknown user":      strings.Replace(insertSession, "'u1'", "'nope'", 1),
		"duplicate category name":        strings.Replace(insertCategory, "'cat1'", "'cat2'", 1),
		"duplicate username":             strings.Replace(insertUser, "'u1'", "'u2'", 1),
		"duplicate session token":        insertSession,
		"duplicate api key hash":         strings.Replace(insertAPIKey, "'k1'", "'k2'", 1),
	}

	for name, stmt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := db.Writer().Exec(stmt)
			require.Error(t, err)
		})
	}
}

func TestSchemaHasTheQueryIndexes(t *testing.T) {
	db := openDefault(t)
	explain := func(query string, args ...any) string {
		rows, err := db.Reader().Query("EXPLAIN QUERY PLAN "+query, args...)
		require.NoError(t, err)
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan.WriteString(detail + "\n")
		}
		require.NoError(t, rows.Err())
		return plan.String()
	}

	assert.Contains(t, explain(`SELECT id FROM downloads WHERE state = ?`, "ready"), "downloads_state")
	assert.Contains(t, explain(`SELECT id FROM downloads WHERE category_id = ?`, "c"), "downloads_category_id")
	assert.Contains(t, explain(`SELECT id FROM downloads WHERE added_at > ? OR (added_at = ? AND id > ?) ORDER BY added_at, id`, 1, 1, "x"), "downloads_added_at_id")
	assert.Contains(t, explain(`SELECT user_id FROM sessions WHERE token_hash = ?`, make([]byte, 32)), "USING PRIMARY KEY")
}
