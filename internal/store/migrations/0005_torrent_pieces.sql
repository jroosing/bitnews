-- M5-02: which pieces of each torrent are verified, so a restart does not
-- rehash a downloaded torrent. One row per torrent, keyed by the info hash
-- (lowercase hex of core.InfoHash) so the data survives a move to a new
-- directory. bitmap holds one bit per piece, least significant bit first;
-- the checks keep a corrupt row from being larger than the piece cap
-- (4 Mi pieces = 512 KiB).

CREATE TABLE torrent_pieces (
    info_hash   TEXT PRIMARY KEY CHECK (length(info_hash) = 40),
    piece_count INTEGER NOT NULL CHECK (piece_count BETWEEN 0 AND 4194304),
    bitmap      BLOB NOT NULL CHECK (length(bitmap) = (piece_count + 7) / 8),
    updated_at  INTEGER NOT NULL
) STRICT;
