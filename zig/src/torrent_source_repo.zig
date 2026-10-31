//! The torrent_sources table (internal/store/repo_torrent_sources.go).
//! The magnet is sealed with the master key and the AAD
//! `torrent_sources/magnet/<download id>`. `info_hash` is 40 lowercase hex
//! characters. A second row for the same Download or the same hash is Conflict.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
const secretbox = @import("secretbox.zig");
const Key = @import("keyfile.zig").Key;

pub const Error = error{
    Conflict,
    NotFound,
} || sqlite.Error || Allocator.Error || Io.Cancelable || Io.RandomSecureError || secretbox.OpenError;

pub const Source = struct {
    info_hash: [20]u8,
    /// Empty when the Download was added from a .torrent file. Wipe with
    /// `secretbox.wipeAndFree` when it is not empty.
    magnet: []u8,
    torrent_path: []u8,
    magnet_unavailable: bool = false,

    pub fn deinit(self: *Source, alloc: Allocator) void {
        if (self.magnet.len > 0) secretbox.wipeAndFree(alloc, self.magnet) else alloc.free(self.magnet);
        alloc.free(self.torrent_path);
        self.* = undefined;
    }
};

pub const Repo = struct {
    store: *sqlite.Store,
    key: *const Key,

    /// Stores the magnet source of an existing Download.
    pub fn createMagnet(self: Repo, alloc: Allocator, download_id: []const u8, hash: [20]u8, magnet: []const u8) Error!void {
        const aad = try secretbox.aad(alloc, "torrent_sources", "magnet", download_id);
        defer alloc.free(aad);
        const sealed = try secretbox.seal(alloc, self.store.io, self.key, magnet, aad);
        defer alloc.free(sealed);
        const hex = std.fmt.bytesToHex(hash, .lower);
        const now = Io.Clock.now(.real, self.store.io).toMilliseconds();
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            "INSERT INTO torrent_sources (download_id, info_hash, torrent_path, magnet, created_at) VALUES (?, ?, NULL, ?, ?)",
            .{ download_id, &hex, sealed, now },
        ) catch |err| {
            if (err == error.Constraint and w.db.failure().isConflict()) return error.Conflict;
            return err;
        };
    }

    /// Stores a .torrent file source. The magnet column stays null.
    pub fn createFile(self: Repo, download_id: []const u8, hash: [20]u8, torrent_path: []const u8) Error!void {
        if (torrent_path.len == 0) return error.NotFound;
        const hex = std.fmt.bytesToHex(hash, .lower);
        const now = Io.Clock.now(.real, self.store.io).toMilliseconds();
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            "INSERT INTO torrent_sources (download_id, info_hash, torrent_path, magnet, created_at) VALUES (?, ?, ?, NULL, ?)",
            .{ download_id, &hex, torrent_path, now },
        ) catch |err| {
            if (err == error.Constraint and w.db.failure().isConflict()) return error.Conflict;
            return err;
        };
    }

    pub fn get(self: Repo, alloc: Allocator, download_id: []const u8) Error!Source {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT info_hash, torrent_path, magnet FROM torrent_sources WHERE download_id = ?");
        defer st.finalize();
        try st.bindAll(.{download_id});
        if (!try st.step()) return error.NotFound;
        const hex = try st.columnText(alloc, 0);
        defer alloc.free(hex);
        var hash: [20]u8 = undefined;
        _ = std.fmt.hexToBytes(&hash, hex) catch return error.NotFound;
        const path = if (st.columnIsNull(1)) try alloc.dupe(u8, "") else try st.columnText(alloc, 1);
        errdefer alloc.free(path);
        if (st.columnIsNull(2)) return .{ .info_hash = hash, .magnet = try alloc.dupe(u8, ""), .torrent_path = path };
        const sealed = try st.columnText(alloc, 2);
        defer alloc.free(sealed);
        const aad = try secretbox.aad(alloc, "torrent_sources", "magnet", download_id);
        defer alloc.free(aad);
        const plain = secretbox.open(alloc, self.key, sealed, aad) catch |err| switch (err) {
            error.Decrypt, error.UnsupportedFormat => return .{
                .info_hash = hash,
                .magnet = try alloc.dupe(u8, ""),
                .torrent_path = path,
                .magnet_unavailable = true,
            },
            else => |e| return e,
        };
        return .{ .info_hash = hash, .magnet = plain, .torrent_path = path };
    }

    /// Records the .torrent file saved for a magnet Download.
    pub fn setTorrentPath(self: Repo, download_id: []const u8, path: []const u8) Error!void {
        if (path.len == 0) return error.NotFound;
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs("UPDATE torrent_sources SET torrent_path = ? WHERE download_id = ?", .{ path, download_id }) catch |err| return err;
        if (w.db.changes() == 0) return error.NotFound;
    }

    /// Torrents still in the queue (`fetching_metadata`, `queued`, `fetching`).
    /// The caller frees each row and the slice.
    pub fn listActive(self: Repo, alloc: Allocator) Error![]Active {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db,
            \\SELECT s.download_id, s.info_hash, s.magnet, d.paused
            \\FROM torrent_sources s
            \\JOIN downloads d ON d.id = s.download_id
            \\WHERE d.source_kind = 'torrent'
            \\  AND d.state IN ('fetching_metadata', 'queued', 'fetching')
        );
        defer st.finalize();
        var list: std.ArrayList(Active) = .empty;
        errdefer {
            for (list.items) |*row| row.deinit(alloc);
            list.deinit(alloc);
        }
        while (try st.step()) {
            const id = try st.columnText(alloc, 0);
            errdefer alloc.free(id);
            const hex = try st.columnText(alloc, 1);
            defer alloc.free(hex);
            var hash: [20]u8 = undefined;
            if (std.fmt.hexToBytes(&hash, hex)) |_| {} else |_| {
                alloc.free(id);
                continue;
            }
            const magnet = if (st.columnIsNull(2))
                try alloc.dupe(u8, "")
            else
                try openMagnet(alloc, self.key, id, try st.columnText(alloc, 2));
            try list.append(alloc, .{
                .id = id,
                .hash = hash,
                .magnet = magnet,
                .paused = st.columnInt(3) != 0,
            });
        }
        return try list.toOwnedSlice(alloc);
    }

    /// Records that libtorrent finished moving the data to `location`.
    /// Leaves seeding counters already stored on the row alone.
    pub fn markMoved(self: Repo, download_id: []const u8, location: []const u8) Error!void {
        if (location.len == 0) return error.NotFound;
        const now = Io.Clock.now(.real, self.store.io).toMilliseconds();
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            \\INSERT INTO torrent_state (download_id, uploaded, seeding_seconds, moved, location, updated_at)
            \\VALUES (?, 0, 0, 1, ?, ?)
            \\ON CONFLICT (download_id) DO UPDATE SET
            \\  moved = 1, location = excluded.location, updated_at = excluded.updated_at
        , .{ download_id, location, now }) catch |err| {
            if (err == error.Constraint and !w.db.failure().isConflict()) return error.NotFound;
            return err;
        };
    }

    /// Torrents whose data already lives at `location` (moved = 1).
    pub fn listMoved(self: Repo, alloc: Allocator) Error![]Moved {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db,
            \\SELECT s.download_id, s.info_hash, s.magnet, t.location, d.paused
            \\FROM torrent_sources s
            \\JOIN downloads d ON d.id = s.download_id
            \\JOIN torrent_state t ON t.download_id = s.download_id
            \\WHERE t.moved = 1 AND t.location IS NOT NULL
            \\  AND d.state IN ('fetched', 'post_processing', 'ready', 'failed')
        );
        defer st.finalize();
        var list: std.ArrayList(Moved) = .empty;
        errdefer {
            for (list.items) |*row| row.deinit(alloc);
            list.deinit(alloc);
        }
        while (try st.step()) {
            const id = try st.columnText(alloc, 0);
            errdefer alloc.free(id);
            const hex = try st.columnText(alloc, 1);
            defer alloc.free(hex);
            var hash: [20]u8 = undefined;
            if (std.fmt.hexToBytes(&hash, hex)) |_| {} else |_| {
                alloc.free(id);
                continue;
            }
            const magnet = if (st.columnIsNull(2))
                try alloc.dupe(u8, "")
            else
                try openMagnet(alloc, self.key, id, try st.columnText(alloc, 2));
            errdefer if (magnet.len > 0) secretbox.wipeAndFree(alloc, magnet) else alloc.free(magnet);
            const location = try st.columnText(alloc, 3);
            try list.append(alloc, .{
                .id = id,
                .hash = hash,
                .magnet = magnet,
                .location = location,
                .paused = st.columnInt(4) != 0,
            });
        }
        return try list.toOwnedSlice(alloc);
    }

    /// Torrent Downloads for `torrents/info`, oldest first.
    pub fn listTorrents(self: Repo, alloc: Allocator) Error![]Listed {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db,
            \\SELECT s.info_hash, d.name, d.state, d.paused, d.total_bytes, d.done_bytes, d.added_at, COALESCE(c.name, '')
            \\FROM torrent_sources s
            \\JOIN downloads d ON d.id = s.download_id
            \\LEFT JOIN categories c ON c.id = d.category_id
            \\WHERE d.source_kind = 'torrent'
            \\ORDER BY d.added_at, s.info_hash
        );
        defer st.finalize();
        var list: std.ArrayList(Listed) = .empty;
        errdefer {
            for (list.items) |*row| row.deinit(alloc);
            list.deinit(alloc);
        }
        while (try st.step()) {
            try list.append(alloc, .{
                .hash = try st.columnText(alloc, 0),
                .name = try st.columnText(alloc, 1),
                .state = try st.columnText(alloc, 2),
                .paused = st.columnInt(3) != 0,
                .total_bytes = st.columnInt(4),
                .done_bytes = st.columnInt(5),
                .added_at = st.columnInt(6),
                .category = try st.columnText(alloc, 7),
            });
        }
        return try list.toOwnedSlice(alloc);
    }

    /// The Download id for a 40-hex info-hash, or null.
    pub fn findByHash(self: Repo, alloc: Allocator, hash_hex: []const u8) Error!?[]u8 {
        if (hash_hex.len != 40) return null;
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT download_id FROM torrent_sources WHERE info_hash = ?");
        defer st.finalize();
        var lower: [40]u8 = undefined;
        for (hash_hex, 0..) |c, i| lower[i] = std.ascii.toLower(c);
        try st.bindAll(.{&lower});
        if (!try st.step()) return null;
        return try st.columnText(alloc, 0);
    }

    /// Stores a per-Download share limit. Null means the global default.
    /// A missing torrent_state row is created; existing counters stay.
    pub fn setShareLimits(self: Repo, download_id: []const u8, ratio: ?f64, seeding_seconds: ?i64) Error!void {
        const now = Io.Clock.now(.real, self.store.io).toMilliseconds();
        var ratio_buf: [64]u8 = undefined;
        const ratio_text: ?[]const u8 = if (ratio) |r| std.fmt.bufPrint(&ratio_buf, "{d}", .{r}) catch return error.NotFound else null;
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            \\INSERT INTO torrent_state (download_id, uploaded, seeding_seconds, ratio_limit, seeding_limit_seconds, moved, updated_at)
            \\VALUES (?, 0, 0, ?, ?, 0, ?)
            \\ON CONFLICT (download_id) DO UPDATE SET
            \\  ratio_limit = excluded.ratio_limit,
            \\  seeding_limit_seconds = excluded.seeding_limit_seconds,
            \\  updated_at = excluded.updated_at
        , .{ download_id, ratio_text, seeding_seconds, now }) catch |err| {
            if (err == error.Constraint and !w.db.failure().isConflict()) return error.NotFound;
            return err;
        };
    }
};

fn openMagnet(alloc: Allocator, key: *const Key, id: []const u8, sealed: []u8) Error![]u8 {
    defer alloc.free(sealed);
    const aad = try secretbox.aad(alloc, "torrent_sources", "magnet", id);
    defer alloc.free(aad);
    return secretbox.open(alloc, key, sealed, aad) catch |err| switch (err) {
        error.Decrypt, error.UnsupportedFormat => alloc.dupe(u8, ""),
        else => |e| return e,
    };
}

pub const Active = struct {
    id: []u8,
    hash: [20]u8,
    /// Empty when there is no magnet or it cannot be opened. Wipe on deinit.
    magnet: []u8,
    paused: bool,

    pub fn deinit(self: *Active, alloc: Allocator) void {
        alloc.free(self.id);
        if (self.magnet.len > 0) secretbox.wipeAndFree(alloc, self.magnet) else alloc.free(self.magnet);
        self.* = undefined;
    }
};

pub const Listed = struct {
    hash: []u8,
    name: []u8,
    state: []u8,
    category: []u8,
    paused: bool,
    total_bytes: i64,
    done_bytes: i64,
    added_at: i64,

    pub fn deinit(self: *Listed, alloc: Allocator) void {
        alloc.free(self.hash);
        alloc.free(self.name);
        alloc.free(self.state);
        alloc.free(self.category);
        self.* = undefined;
    }
};

pub const Moved = struct {
    id: []u8,
    hash: [20]u8,
    magnet: []u8,
    location: []u8,
    paused: bool,

    pub fn deinit(self: *Moved, alloc: Allocator) void {
        alloc.free(self.id);
        alloc.free(self.location);
        if (self.magnet.len > 0) secretbox.wipeAndFree(alloc, self.magnet) else alloc.free(self.magnet);
        self.* = undefined;
    }
};

const testing = std.testing;
const tu = @import("downloads_testutil.zig");

test "a magnet source round-trips sealed, and a second hash conflicts" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    try rig.repo.create(.{
        .id = "d1",
        .name = "d1",
        .source_kind = .torrent,
        .state = .fetching_metadata,
        .added_at = tu.epoch,
        .updated_at = tu.epoch,
    });
    const key: Key = .{ .bytes = @splat(7) };
    const repo: Repo = .{ .store = rig.store, .key = &key };
    const hash: [20]u8 = @splat(0xab);
    const magnet = "magnet:?xt=urn:btih:abababababababababababababababababababab";
    try repo.createMagnet(testing.allocator, "d1", hash, magnet);
    var got = try repo.get(testing.allocator, "d1");
    defer got.deinit(testing.allocator);
    try testing.expectEqualSlices(u8, &hash, &got.info_hash);
    try testing.expectEqualStrings(magnet, got.magnet);
    try testing.expect(!got.magnet_unavailable);
    try repo.setTorrentPath("d1", "/data/torrents/d1/Show.torrent");
    got.deinit(testing.allocator);
    got = try repo.get(testing.allocator, "d1");
    try testing.expectEqualStrings("/data/torrents/d1/Show.torrent", got.torrent_path);

    try rig.repo.create(.{
        .id = "d2",
        .name = "d2",
        .source_kind = .torrent,
        .state = .fetching_metadata,
        .added_at = tu.epoch,
        .updated_at = tu.epoch,
    });
    try testing.expectError(error.Conflict, repo.createMagnet(testing.allocator, "d2", hash, magnet));
}

test "listActive returns queue torrents and skips a ready one" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const key: Key = .{ .bytes = @splat(7) };
    const repo: Repo = .{ .store = rig.store, .key = &key };
    try rig.repo.create(.{ .id = "q1", .name = "q1", .source_kind = .torrent, .state = .queued, .paused = true, .added_at = tu.epoch, .updated_at = tu.epoch });
    try rig.repo.create(.{ .id = "done", .name = "done", .source_kind = .torrent, .state = .ready, .added_at = tu.epoch, .updated_at = tu.epoch });
    const h1: [20]u8 = @splat(0x11);
    const h2: [20]u8 = @splat(0x22);
    try repo.createMagnet(testing.allocator, "q1", h1, "magnet:?xt=urn:btih:1111111111111111111111111111111111111111");
    try repo.createMagnet(testing.allocator, "done", h2, "magnet:?xt=urn:btih:2222222222222222222222222222222222222222");
    const rows = try repo.listActive(testing.allocator);
    defer {
        for (rows) |*row| {
            var item = row.*;
            item.deinit(testing.allocator);
        }
        testing.allocator.free(rows);
    }
    try testing.expectEqual(@as(usize, 1), rows.len);
    try testing.expectEqualStrings("q1", rows[0].id);
    try testing.expect(rows[0].paused);
    try testing.expect(std.mem.startsWith(u8, rows[0].magnet, "magnet:?"));
}

test "listMoved returns a ready torrent at its stored location" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const key: Key = .{ .bytes = @splat(7) };
    const repo: Repo = .{ .store = rig.store, .key = &key };
    try rig.repo.create(.{ .id = "done", .name = "Show", .source_kind = .torrent, .state = .ready, .added_at = tu.epoch, .updated_at = tu.epoch });
    const hash: [20]u8 = @splat(0x33);
    try repo.createMagnet(testing.allocator, "done", hash, "magnet:?xt=urn:btih:3333333333333333333333333333333333333333");
    try repo.markMoved("done", "/data/torrents/Show");
    try repo.markMoved("done", "/data/torrents/Show.2");
    const rows = try repo.listMoved(testing.allocator);
    defer {
        for (rows) |*row| {
            var item = row.*;
            item.deinit(testing.allocator);
        }
        testing.allocator.free(rows);
    }
    try testing.expectEqual(@as(usize, 1), rows.len);
    try testing.expectEqualStrings("done", rows[0].id);
    try testing.expectEqualStrings("/data/torrents/Show.2", rows[0].location);
    try testing.expectEqualSlices(u8, &hash, &rows[0].hash);
}
