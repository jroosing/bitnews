-- M2 schema: Usenet Servers and the Source of NZB Downloads. Same
-- conventions as 0001: INTEGER unix epoch milliseconds (UTC), TEXT ids.

-- A Usenet Server. TLS is always on, so there is no column for it.
-- password holds the sealed form "v1:<base64>" (AAD servers/password/<id>);
-- the empty string means no password.
CREATE TABLE servers (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    host            TEXT NOT NULL,
    port            INTEGER NOT NULL DEFAULT 563 CHECK (port BETWEEN 1 AND 65535),
    username        TEXT NOT NULL DEFAULT '',
    password        TEXT NOT NULL DEFAULT '',
    max_connections INTEGER NOT NULL DEFAULT 8 CHECK (max_connections BETWEEN 1 AND 100),
    retention_days  INTEGER NOT NULL DEFAULT 0 CHECK (retention_days >= 0),
    priority        INTEGER NOT NULL DEFAULT 0 CHECK (priority >= 0),
    enabled         INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX servers_priority_name ON servers (priority, name);

-- What the Usenet runner needs to fetch an NZB Download again after a
-- restart: the stored .nzb file and the archive password (sealed, AAD
-- download_sources/password/<download id>; NULL when there is none). The
-- NZB itself stays on disk.
CREATE TABLE download_sources (
    download_id TEXT PRIMARY KEY REFERENCES downloads (id) ON DELETE CASCADE,
    nzb_path    TEXT NOT NULL,
    password    TEXT
) STRICT;
