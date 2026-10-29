-- M2-12 and M3-07: the Usenet runner's state, so a restart resumes where it
-- left off. Same conventions as 0001: INTEGER unix epoch milliseconds (UTC),
-- TEXT ids. All rows go away with their Download.

-- The resume blob of a Download being fetched: the file writer's snapshot
-- plus the Articles already resolved (an opaque, versioned and CRC-protected
-- value owned by internal/usenet/runner, capped at 16 MiB by the repository).
CREATE TABLE usenet_progress (
    download_id TEXT PRIMARY KEY REFERENCES downloads (id) ON DELETE CASCADE,
    blob        BLOB NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

-- What a finished fetch knew about the Articles of a Download, so
-- Post-processing still finds it after a restart (runner.FetchDetail).
CREATE TABLE usenet_reports (
    download_id      TEXT PRIMARY KEY REFERENCES downloads (id) ON DELETE CASCADE,
    missing_articles INTEGER NOT NULL CHECK (missing_articles >= 0),
    missing_bytes    INTEGER NOT NULL CHECK (missing_bytes >= 0),
    damaged_articles INTEGER NOT NULL CHECK (damaged_articles >= 0),
    damaged_bytes    INTEGER NOT NULL CHECK (damaged_bytes >= 0),
    missing_files    INTEGER NOT NULL CHECK (missing_files >= 0),
    updated_at       INTEGER NOT NULL
) STRICT;

-- PAR2 recovery volumes that Repair asked for (M3-07). Volumes are deferred
-- by default; a row here means the volume at file_index of the stored NZB is
-- wanted. fetched is 0 until a fetch has resolved all its Articles.
CREATE TABLE usenet_undeferred (
    download_id TEXT NOT NULL REFERENCES downloads (id) ON DELETE CASCADE,
    file_index  INTEGER NOT NULL CHECK (file_index >= 0),
    fetched     INTEGER NOT NULL DEFAULT 0 CHECK (fetched IN (0, 1)),
    PRIMARY KEY (download_id, file_index)
) STRICT, WITHOUT ROWID;
