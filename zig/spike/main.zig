//! Z0 build spike (M15-01): proves the toolchain split of ADR-0014. A Zig
//! binary links the SQLite amalgamation (compiled by Zig), libcurl, OpenSSL
//! and libbitnews_lt (the C face of the distro's libtorrent, built with the
//! system C++ compiler), then adds a magnet and prints its peers.
const std = @import("std");
const bnlt = @import("bnlt");

extern fn sqlite3_libversion() [*:0]const u8;
extern fn sqlite3_open_v2(filename: [*:0]const u8, db: *?*anyopaque, flags: c_int, vfs: ?[*:0]const u8) c_int;
extern fn sqlite3_exec(db: ?*anyopaque, sql: [*:0]const u8, cb: ?*const anyopaque, arg: ?*anyopaque, err: ?*[*:0]u8) c_int;
extern fn sqlite3_close(db: ?*anyopaque) c_int;
extern fn curl_version() [*:0]const u8;
extern fn OpenSSL_version(t: c_int) [*:0]const u8;

pub fn main(init: std.process.Init) !void {
    const io = init.io;
    const args = try init.minimal.args.toSlice(init.arena.allocator());
    const seconds: u64 = if (args.len > 1) try std.fmt.parseInt(u64, args[1], 10) else 20;

    std.debug.print("sqlite   {s}\n", .{sqlite3_libversion()});
    std.debug.print("curl     {s}\n", .{curl_version()});
    std.debug.print("openssl  {s}\n", .{OpenSSL_version(0)});
    std.debug.print("libtorrent {s}\n", .{bnlt.bnlt_version()});

    try sqliteCheck();

    const ses = bnlt.bnlt_new("0.0.0.0:0", 1) orelse return error.SessionFailed;
    defer bnlt.bnlt_free(ses);
    // Sintel, a Blender open movie (CC BY), from the WebTorrent demo.
    const magnet = "magnet:?xt=urn:btih:08ada5a7a6183aae1e09d831df6748d566095a10&dn=Sintel";
    if (bnlt.bnlt_add_magnet(ses, magnet, "/tmp") != 0) return error.AddFailed;

    var t: u64 = 0;
    while (t < seconds) : (t += 1) {
        try io.sleep(.fromSeconds(1), .awake);
        var alert: [512]u8 = undefined;
        var shown: usize = 0;
        while (bnlt.bnlt_pop_alert(ses, &alert, alert.len) == 1 and shown < 3) : (shown += 1) {
            std.debug.print("  alert: {s}\n", .{std.mem.sliceTo(&alert, 0)});
        }
        var st: bnlt.bnlt_status = undefined;
        if (bnlt.bnlt_status_first(ses, &st) == 0) {
            std.debug.print("t={d:>3}s state={d} peers={d} seeds={d} metadata={d} name={s}\n", .{
                t + 1, st.state, st.num_peers, st.num_seeds, st.has_metadata, std.mem.sliceTo(&st.name, 0),
            });
            if (st.has_metadata == 1 and st.num_peers > 0) {
                std.debug.print("OK: metadata received from {d} peers\n", .{st.num_peers});
                return;
            }
        }
    }
    std.debug.print("no metadata within {d}s (DHT bootstrap can be slow or blocked)\n", .{seconds});
}

fn sqliteCheck() !void {
    var db: ?*anyopaque = null;
    if (sqlite3_open_v2(":memory:", &db, 0x2 | 0x4, null) != 0) return error.SqliteOpen;
    defer _ = sqlite3_close(db);
    // STRICT tables (3.37) and RENAME COLUMN (3.25) are what the Go store's migrations use.
    const sql =
        "CREATE TABLE categories (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, dir_override TEXT NOT NULL DEFAULT '') STRICT;" ++
        "ALTER TABLE categories RENAME COLUMN dir_override TO usenet_dir;" ++
        "ALTER TABLE categories ADD COLUMN torrent_dir TEXT NOT NULL DEFAULT '';" ++
        "INSERT INTO categories (id, name, usenet_dir, torrent_dir) VALUES ('c1', 'movies', '/a', '/b');";
    if (sqlite3_exec(db, sql, null, null, null) != 0) return error.SqliteExec;
    std.debug.print("sqlite   STRICT + RENAME COLUMN ok\n", .{});
}
