-- M5-03: what a torrent Download was created from, written once when the
-- Download is added. The .torrent file lives on disk (mode 0600); only its
-- path is stored. A magnet link can carry tracker URLs with a personal
-- passkey, so it is stored sealed (AAD torrent_sources/magnet/<download id>).
-- info_hash is the lowercase hex of core.InfoHash and is unique: that is how
-- a duplicate torrent is detected.

CREATE TABLE torrent_sources (
    download_id  TEXT PRIMARY KEY REFERENCES downloads(id) ON DELETE CASCADE,
    info_hash    TEXT NOT NULL UNIQUE CHECK (length(info_hash) = 40),
    torrent_path TEXT NULL,
    magnet       TEXT NULL,
    created_at   INTEGER NOT NULL,
    CHECK (torrent_path IS NOT NULL OR magnet IS NOT NULL)
) STRICT;
