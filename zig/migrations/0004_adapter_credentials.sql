-- Adapter credentials (M4-02): one per media manager, valid on the adapter
-- routes only. The key is a 256-bit token of which only the SHA-256 hash is
-- stored. adapter_qbit credentials also have a username (the key is the
-- password); adapter_sab ones have none.
CREATE TABLE adapter_credentials (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('adapter_sab', 'adapter_qbit')),
    name         TEXT NOT NULL,
    username     TEXT,
    secret_hash  BLOB NOT NULL UNIQUE CHECK (length(secret_hash) = 32),
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    CHECK ((kind = 'adapter_qbit') = (username IS NOT NULL))
) STRICT;

CREATE UNIQUE INDEX adapter_credentials_kind_username
    ON adapter_credentials (kind, username) WHERE username IS NOT NULL;
