-- M1 schema. Timestamps are INTEGER unix epoch milliseconds (UTC); a NULL
-- timestamp means "not yet" (the zero time.Time in core). Ids are TEXT.
-- Token hashes (SHA-256) are 32-byte BLOBs.

CREATE TABLE categories (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    dir_override TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE downloads (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    source_kind    TEXT NOT NULL CHECK (source_kind IN ('nzb', 'torrent')),
    -- NULL when the Download has no Category.
    category_id    TEXT REFERENCES categories (id) ON DELETE SET NULL,
    state          TEXT NOT NULL CHECK (state IN (
                       'added', 'fetching_metadata', 'queued', 'fetching',
                       'fetched', 'post_processing', 'ready', 'failed')),
    priority       TEXT NOT NULL DEFAULT 'normal'
                       CHECK (priority IN ('force', 'high', 'normal', 'low')),
    queue_position INTEGER NOT NULL DEFAULT 0,
    paused         INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1)),
    -- NULL unless the Download failed.
    failure_reason TEXT CHECK (failure_reason IS NULL OR failure_reason IN (
                       'missing_articles', 'repair_insufficient', 'archive_unsupported',
                       'archive_password', 'archive_corrupt', 'disk_full',
                       'path_violation', 'source_invalid', 'internal')),
    failure_detail TEXT NOT NULL DEFAULT '',
    total_bytes    INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    done_bytes     INTEGER NOT NULL DEFAULT 0 CHECK (done_bytes >= 0),
    added_at       INTEGER NOT NULL,
    fetched_at     INTEGER,
    ready_at       INTEGER,
    updated_at     INTEGER NOT NULL,
    output_path    TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX downloads_state ON downloads (state);
CREATE INDEX downloads_category_id ON downloads (category_id);
-- Cursor paging orders by (added_at, id).
CREATE INDEX downloads_added_at_id ON downloads (added_at, id);

-- One row per settings key; the value is the JSON encoding of the typed
-- Go value.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL CHECK (json_valid(value)),
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash BLOB NOT NULL,
    created_at    INTEGER NOT NULL
) STRICT;

-- token_hash is the primary key, so lookups by token hash use its unique
-- index.
CREATE TABLE sessions (
    token_hash   BLOB PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX sessions_user_id ON sessions (user_id);

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    scope        TEXT NOT NULL CHECK (scope IN ('read', 'write')),
    hash         BLOB NOT NULL UNIQUE CHECK (length(hash) = 32),
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
) STRICT;
