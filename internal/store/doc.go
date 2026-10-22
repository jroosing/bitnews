// Package store implements the core repositories on SQLite, including migrations and secret encryption.
//
// # Connections
//
// [Open] creates the database file with mode 0600 before SQLite first sees it
// (an existing file with broader permissions is kept and reported with a
// warning on the logger from [WithLogger]), then applies pending migrations.
// The DB has two pools on the same file:
//
//   - [DB.Writer]: exactly one connection, with write transactions that take
//     the write lock when they begin. [DB.WithTx] runs a function in such a
//     transaction.
//   - [DB.Reader]: several read-only connections. In WAL mode they are never
//     blocked by the writer and see the last committed state.
//
// Every connection runs with journal_mode=WAL, foreign_keys=ON,
// busy_timeout=5000 and synchronous=NORMAL.
//
// # Migrations
//
// Migrations are the embedded files migrations/NNNN_name.sql. Versions must
// run 1, 2, 3, ... without gaps or duplicates. Each migration runs in one
// transaction together with its row in schema_migrations(version, name,
// applied_at), so a failing migration leaves nothing behind. They only move
// forward: a database that records a version this binary does not know is
// refused instead of being opened by an older binary.
//
// # Encoding
//
//   - Ids are TEXT (core.ID).
//   - Timestamps are INTEGER unix epoch milliseconds in UTC. Millisecond
//     resolution keeps the (added_at, id) paging order meaningful. The zero
//     time.Time is stored as NULL (fetched_at, ready_at, last_used_at), and
//     an empty optional value is also NULL (category_id, failure_reason).
//   - Booleans are INTEGER 0 or 1.
//   - Enumerations (source kind, state, priority, failure reason, API key
//     scope) are TEXT with CHECK constraints listing the core values.
//   - Token hashes (core.TokenHash, SHA-256) are 32-byte BLOBs.
//
// # Tables (migration 0001)
//
//   - categories(id, name UNIQUE, dir_override)
//   - downloads(id, name, source_kind, category_id, state, priority,
//     queue_position, paused, failure_reason, failure_detail, total_bytes,
//     done_bytes, added_at, fetched_at, ready_at, updated_at, output_path).
//     category_id references categories ON DELETE SET NULL. Indexes on
//     state, category_id and (added_at, id).
//   - settings(key, value, updated_at): key to JSON text.
//   - users(id, username UNIQUE, password_hash, created_at)
//   - sessions(token_hash PRIMARY KEY, user_id, created_at, last_seen_at).
//     user_id references users ON DELETE CASCADE.
//   - schema_migrations(version, name, applied_at)
//
// # Tables (migration 0003, the Usenet runner's state)
//
//   - usenet_progress(download_id, blob, updated_at): the resume blob of a
//     Download being fetched, capped at MaxUsenetProgressBlob.
//   - usenet_reports(download_id, missing_articles, missing_bytes,
//     damaged_articles, damaged_bytes, missing_files, updated_at)
//   - usenet_undeferred(download_id, file_index, fetched): PAR2 recovery
//     volumes that Repair asked for (M3-07).
//
// All three reference downloads ON DELETE CASCADE.
//
// # Tables (migration 0008, the app key)
//
//   - app_key(id CHECK (id = 1), sealed_key, updated_at): the one API key of
//     the app (ADR-0006), sealed with the master key. The same migration
//     drops api_keys (0001) and adapter_credentials (0004).
//
// All tables are STRICT.
package store
