-- M5-05, M5-06: what bitnews keeps about a torrent Download besides its
-- source: the seeding counters of earlier runs (the engine counts from zero
-- after a restart), the per-Download Share Limit and where the data lives
-- after the move to complete. Same conventions as 0001: INTEGER unix epoch
-- milliseconds (UTC), TEXT ids. The row goes away with its Download.
--
-- ratio_limit and seeding_limit_seconds are NULL for "use the global
-- default" and -1 for "unlimited". moved = 0 with a location is the target
-- of a move that was started and not confirmed.
CREATE TABLE torrent_state (
    download_id           TEXT PRIMARY KEY REFERENCES downloads (id) ON DELETE CASCADE,
    uploaded              INTEGER NOT NULL DEFAULT 0 CHECK (uploaded >= 0),
    seeding_seconds       INTEGER NOT NULL DEFAULT 0 CHECK (seeding_seconds >= 0),
    ratio_limit           REAL NULL,
    seeding_limit_seconds INTEGER NULL,
    moved                 INTEGER NOT NULL DEFAULT 0 CHECK (moved IN (0, 1)),
    location              TEXT NULL,
    updated_at            INTEGER NOT NULL
) STRICT;
