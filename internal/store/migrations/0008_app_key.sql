-- M7-04 (ADR-0006): one API key for the whole app, replacing named API keys
-- (0001) and per-media-manager adapter credentials (0004). The key is stored
-- sealed with the master key (v1:..., see secretbox.go), so Settings can show
-- it again; the CHECK keeps the table to a single row. No release has shipped,
-- so the dropped tables hold nothing worth migrating.
CREATE TABLE app_key (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    sealed_key TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

DROP TABLE adapter_credentials;
DROP TABLE api_keys;
