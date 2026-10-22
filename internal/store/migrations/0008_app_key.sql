-- M7-04 (ADR-0006): one API key for the whole app, replacing named API keys
-- (0001) and per-media-manager adapter credentials (0004). The key is stored
-- as plain text, like the key in Sonarr's config.xml: Settings shows it and
-- the daemon compares requests against a copy in memory. The CHECK keeps the
-- table to a single row. No release has shipped, so the dropped tables hold
-- nothing worth migrating.
CREATE TABLE app_key (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    key        TEXT NOT NULL CHECK (key <> ''),
    updated_at INTEGER NOT NULL
) STRICT;

DROP TABLE adapter_credentials;
DROP TABLE api_keys;
