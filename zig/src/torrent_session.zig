//! One libtorrent session for the daemon (Z7). The C wrapper is `libbitnews_lt`.
//! This file is imported only by `main.zig`, which is the module that links
//! `bnlt`. The test groups do not.
const std = @import("std");
const c = @import("bnlt");

pub const Session = struct {
    ses: *c.bnlt_session,

    /// DHT on, listen on every interface, port chosen by the OS.
    pub fn open() error{SessionFailed}!Session {
        return openFrom("0.0.0.0:0", true);
    }

    pub fn openFrom(listen: [*:0]const u8, dht: bool) error{SessionFailed}!Session {
        const ses = c.bnlt_new(listen, @intFromBool(dht)) orelse return error.SessionFailed;
        return .{ .ses = ses };
    }

    pub fn close(self: *Session) void {
        c.bnlt_free(self.ses);
    }

    pub fn addMagnet(self: *Session, magnet: [*:0]const u8, save_path: [*:0]const u8) i32 {
        return c.bnlt_add_magnet(self.ses, magnet, save_path);
    }

    pub fn pause(self: *Session, hash: *const [20]u8) i32 {
        return c.bnlt_pause(self.ses, hash);
    }

    pub fn addPeer(self: *Session, hash: *const [20]u8, addr: [*:0]const u8) i32 {
        return c.bnlt_add_peer(self.ses, hash, addr);
    }

    /// 0 when libtorrent has moved the torrent's save path. Nonzero otherwise.
    pub fn moveStorage(self: *Session, hash: *const [20]u8, path: [*:0]const u8) i32 {
        return c.bnlt_move_storage(self.ses, hash, path);
    }

    /// Null when that info-hash is not in the session. `name` points into `name_buf`.
    pub fn status(self: *Session, hash: *const [20]u8, name_buf: []u8) ?Status {
        var st: c.bnlt_status = std.mem.zeroes(c.bnlt_status);
        if (c.bnlt_status_hash(self.ses, hash, &st) != 0) return null;
        const raw = std.mem.sliceTo(&st.name, 0);
        const n = @min(raw.len, name_buf.len);
        @memcpy(name_buf[0..n], raw[0..n]);
        return .{
            .has_metadata = st.has_metadata != 0,
            .total_wanted = st.total_wanted,
            .total_done = st.total_done,
            .name = name_buf[0..n],
        };
    }

    /// The .torrent bytes, or `error.NoMetadata` when they are not ready.
    /// The caller frees the result.
    pub fn readTorrent(self: *Session, alloc: std.mem.Allocator, hash: *const [20]u8) error{ NoMetadata, OutOfMemory }![]u8 {
        var cap: usize = 256 * 1024;
        while (cap <= 10 * 1024 * 1024) {
            const buf = try alloc.alloc(u8, cap);
            var n: usize = 0;
            const rc = c.bnlt_read_torrent(self.ses, hash, buf.ptr, buf.len, &n);
            if (rc == 0) {
                const out = alloc.dupe(u8, buf[0..n]) catch {
                    alloc.free(buf);
                    return error.OutOfMemory;
                };
                alloc.free(buf);
                return out;
            }
            alloc.free(buf);
            if (rc == 3 and n > cap and n <= 10 * 1024 * 1024) {
                cap = n;
                continue;
            }
            return error.NoMetadata;
        }
        return error.NoMetadata;
    }
};

pub const Status = struct {
    has_metadata: bool,
    total_wanted: i64,
    total_done: i64,
    name: []const u8,
};

/// `torrent_add.Engine.add_magnet_fn` for this session.
pub fn addMagnet(ctx: ?*anyopaque, magnet: [*:0]const u8, save_path: [*:0]const u8) i32 {
    const self: *Session = @ptrCast(@alignCast(ctx.?));
    return self.addMagnet(magnet, save_path);
}

/// `torrent_add.Engine.add_torrent_fn` for this session.
pub fn addTorrent(ctx: ?*anyopaque, bytes: [*]const u8, len: usize, save_path: [*:0]const u8, hash_out: *[20]u8) i32 {
    const self: *Session = @ptrCast(@alignCast(ctx.?));
    return c.bnlt_add_torrent(self.ses, bytes, len, save_path, hash_out);
}

/// `torrent_add.Engine.pause_fn` for this session.
pub fn pauseHash(ctx: ?*anyopaque, hash: *const [20]u8) void {
    const self: *Session = @ptrCast(@alignCast(ctx.?));
    _ = self.pause(hash);
}
