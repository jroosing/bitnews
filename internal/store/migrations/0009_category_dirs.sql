-- A Category's folder override is per protocol, as the complete roots are:
-- usenet_dir for Usenet Downloads, torrent_dir for torrents. The one
-- dir_override before this served both, so it becomes both and nothing
-- moves until the user splits them.
ALTER TABLE categories RENAME COLUMN dir_override TO usenet_dir;
ALTER TABLE categories ADD COLUMN torrent_dir TEXT NOT NULL DEFAULT '';
UPDATE categories SET torrent_dir = usenet_dir;
