//! Tests of the torrent group. The libtorrent session is not linked here.
test {
    _ = @import("torrent_add.zig");
    _ = @import("torrent_watch.zig");
    _ = @import("torrent_source_repo.zig");
    _ = @import("torrent_meta.zig");
    _ = @import("torrent_peers.zig");
    _ = @import("torrent_listen.zig");
}
