//! Moves a magnet Download from `fetching_metadata` to `queued` once the
//! engine reports metadata (Z7). The info-hash map lives in memory: the
//! daemon noted it at add time. Tests pass a fake status function, so this
//! file does not link libtorrent.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;

pub const Live = struct {
    has_metadata: bool,
    total_wanted: i64,
    total_done: i64 = 0,
    /// Valid only until the next `status_fn` call.
    name: []const u8,
};

pub const Probe = struct {
    ctx: ?*anyopaque,
    /// Null when the torrent is not in the session.
    status_fn: *const fn (ctx: ?*anyopaque, hash: *const [20]u8) ?Live,
};

pub const Apply = struct {
    ctx: ?*anyopaque,
    display_fn: *const fn (ctx: ?*anyopaque, id: []const u8, name: []const u8, total_bytes: i64) anyerror!void,
    queue_fn: *const fn (ctx: ?*anyopaque, id: []const u8) anyerror!void,
    /// Writes the .torrent file. Failure leaves the Download in fetching_metadata.
    save_fn: ?*const fn (ctx: ?*anyopaque, id: []const u8, hash: *const [20]u8, name: []const u8) anyerror!void = null,
};

pub const Watch = struct {
    gpa: Allocator,
    hashes: std.StringHashMapUnmanaged([20]u8) = .empty,
    /// Save path after a successful relocate. Keys and values are owned.
    locations: std.StringHashMapUnmanaged([]u8) = .empty,
    mu: Io.Mutex = .init,

    pub fn init(gpa: Allocator) Watch {
        return .{ .gpa = gpa };
    }

    pub fn deinit(self: *Watch) void {
        var hashes = self.hashes.keyIterator();
        while (hashes.next()) |k| self.gpa.free(k.*);
        self.hashes.deinit(self.gpa);
        var locs = self.locations.iterator();
        while (locs.next()) |e| {
            self.gpa.free(e.key_ptr.*);
            self.gpa.free(e.value_ptr.*);
        }
        self.locations.deinit(self.gpa);
        self.* = undefined;
    }

    /// Remembers `hash` for `id`. The id is copied.
    pub fn note(self: *Watch, id: []const u8, hash: [20]u8) !void {
        if (self.hashes.getPtr(id)) |slot| {
            slot.* = hash;
            return;
        }
        const owned = try self.gpa.dupe(u8, id);
        errdefer self.gpa.free(owned);
        try self.hashes.put(self.gpa, owned, hash);
    }

    /// Remembers the save path after libtorrent moved the torrent.
    pub fn place(self: *Watch, io: Io, id: []const u8, path: []const u8) !void {
        self.mu.lockUncancelable(io);
        defer self.mu.unlock(io);
        if (self.locations.getPtr(id)) |slot| {
            const next = try self.gpa.dupe(u8, path);
            self.gpa.free(slot.*);
            slot.* = next;
            return;
        }
        const owned_id = try self.gpa.dupe(u8, id);
        errdefer self.gpa.free(owned_id);
        const owned_path = try self.gpa.dupe(u8, path);
        errdefer self.gpa.free(owned_path);
        try self.locations.put(self.gpa, owned_id, owned_path);
    }

    /// A copy of the relocated save path, or null when the torrent is still
    /// in the incomplete directory.
    pub fn location(self: *Watch, io: Io, alloc: Allocator, id: []const u8) !?[]u8 {
        self.mu.lockUncancelable(io);
        defer self.mu.unlock(io);
        const path = self.locations.get(id) orelse return null;
        return try alloc.dupe(u8, path);
    }

    /// For each id still waiting on metadata: when the engine knows the size,
    /// store the name and size, then queue the Download.
    pub fn promote(self: *Watch, ids: []const []const u8, probe: Probe, apply: Apply) void {
        for (ids) |id| {
            const hash = self.hashes.get(id) orelse continue;
            const live = probe.status_fn(probe.ctx, &hash) orelse continue;
            if (!live.has_metadata or live.total_wanted <= 0) continue;
            if (apply.save_fn) |save| save(apply.ctx, id, &hash, live.name) catch continue;
            apply.display_fn(apply.ctx, id, live.name, live.total_wanted) catch continue;
            apply.queue_fn(apply.ctx, id) catch continue;
        }
    }

    /// Notes every hash. Re-adds a magnet when one is present, then pauses it
    /// when the Download is paused.
    pub fn reload(self: *Watch, rows: []const Resume, readd: Readd) void {
        for (rows) |row| {
            self.note(row.id, row.hash) catch continue;
            if (row.magnet.len == 0) continue;
            readd.add_fn(readd.ctx, row.magnet, row.id, row.save_path) catch continue;
            if (row.paused) readd.pause_fn(readd.ctx, &row.hash);
        }
    }

    /// One pass after metadata. At most `limit` Downloads are fetching: a
    /// paused Queued Download stays put, and a full queue activates nobody.
    /// A Fetching Download whose bytes are all in becomes fetched.
    pub fn drive(self: *Watch, jobs: []const Job, probe: Probe, drv: Drive, limit: u32) void {
        var fetching: u32 = 0;
        for (jobs) |job| {
            if (job.phase == .fetching) fetching += 1;
        }
        var room: u32 = if (limit > fetching) limit - fetching else 0;
        for (jobs) |job| switch (job.phase) {
            .queued => {
                if (job.paused or room == 0) continue;
                drv.activate_fn(drv.ctx, job.id) catch continue;
                room -= 1;
            },
            .fetching => {
                const hash = self.hashes.get(job.id) orelse continue;
                const live = probe.status_fn(probe.ctx, &hash) orelse continue;
                if (!live.has_metadata or live.total_wanted <= 0 or live.total_done < live.total_wanted) continue;
                drv.move_fn(drv.ctx, job.id, &hash) catch {};
                drv.fetch_fn(drv.ctx, job.id, live.total_done, live.total_wanted) catch continue;
            },
        };
    }
};

pub const Resume = struct {
    id: []const u8,
    hash: [20]u8,
    magnet: []const u8,
    paused: bool,
    /// When set, the torrent is re-added at this directory instead of
    /// `{incomplete}/{id}`.
    save_path: []const u8 = "",
};

pub const Readd = struct {
    ctx: ?*anyopaque,
    add_fn: *const fn (ctx: ?*anyopaque, magnet: []const u8, id: []const u8, save_path: []const u8) anyerror!void,
    pause_fn: *const fn (ctx: ?*anyopaque, hash: *const [20]u8) void,
};

pub const Phase = enum { queued, fetching };

pub const Job = struct {
    id: []const u8,
    phase: Phase,
    paused: bool = false,
};

pub const Drive = struct {
    ctx: ?*anyopaque,
    /// Relocate before fetched. An error is ignored: the Download is still fetched.
    move_fn: *const fn (ctx: ?*anyopaque, id: []const u8, hash: *const [20]u8) anyerror!void,
    activate_fn: *const fn (ctx: ?*anyopaque, id: []const u8) anyerror!void,
    fetch_fn: *const fn (ctx: ?*anyopaque, id: []const u8, done: i64, total: i64) anyerror!void,
};

test "promote queues once metadata and a size are known" {
    const testing = std.testing;
    var watch = Watch.init(testing.allocator);
    defer watch.deinit();

    const Fake = struct {
        var queued: []const u8 = "";
        var shown_name: []const u8 = "";
        var shown_bytes: i64 = 0;
        var ready: bool = true;
        var want: [20]u8 = @splat(0xab);
        const Self = @This();
        fn status(_: ?*anyopaque, got: *const [20]u8) ?Live {
            if (!std.mem.eql(u8, got, &Self.want)) return null;
            if (!Self.ready) return .{ .has_metadata = false, .total_wanted = 0, .name = "" };
            return .{ .has_metadata = true, .total_wanted = 99, .name = "Show" };
        }
        fn display(_: ?*anyopaque, _: []const u8, name: []const u8, total_bytes: i64) !void {
            Self.shown_name = name;
            Self.shown_bytes = total_bytes;
        }
        fn queue(_: ?*anyopaque, id: []const u8) !void {
            Self.queued = id;
        }
    };
    try watch.note("dl1", Fake.want);
    Fake.ready = false;
    Fake.queued = "";
    watch.promote(&.{"dl1"}, .{ .ctx = null, .status_fn = Fake.status }, .{
        .ctx = null,
        .display_fn = Fake.display,
        .queue_fn = Fake.queue,
    });
    try testing.expectEqualStrings("", Fake.queued);

    Fake.ready = true;
    watch.promote(&.{"dl1"}, .{ .ctx = null, .status_fn = Fake.status }, .{
        .ctx = null,
        .display_fn = Fake.display,
        .queue_fn = Fake.queue,
    });
    try testing.expectEqualStrings("dl1", Fake.queued);
    try testing.expectEqualStrings("Show", Fake.shown_name);
    try testing.expectEqual(@as(i64, 99), Fake.shown_bytes);
}

test "reload notes the hash, re-adds the magnet, and pauses a paused Download" {
    const testing = std.testing;
    var watch = Watch.init(testing.allocator);
    defer watch.deinit();
    const Seen = struct {
        var magnet: []const u8 = "";
        var id: []const u8 = "";
        var paused: bool = false;
        var save_path: []const u8 = "";
        const Self = @This();
        fn add(_: ?*anyopaque, uri: []const u8, download_id: []const u8, path: []const u8) !void {
            Self.magnet = uri;
            Self.id = download_id;
            Self.save_path = path;
        }
        fn pause(_: ?*anyopaque, _: *const [20]u8) void {
            Self.paused = true;
        }
    };
    const hash: [20]u8 = @splat(0xcd);
    watch.reload(&.{.{ .id = "dl9", .hash = hash, .magnet = "magnet:?xt=urn:btih:cd", .paused = true, .save_path = "/data/done/Show" }}, .{
        .ctx = null,
        .add_fn = Seen.add,
        .pause_fn = Seen.pause,
    });
    try testing.expect(watch.hashes.get("dl9") != null);
    try testing.expectEqualStrings("magnet:?xt=urn:btih:cd", Seen.magnet);
    try testing.expectEqualStrings("dl9", Seen.id);
    try testing.expectEqualStrings("/data/done/Show", Seen.save_path);
    try testing.expect(Seen.paused);
}

test "drive activates an unpaused queue item and fetches a complete one" {
    const testing = std.testing;
    var watch = Watch.init(testing.allocator);
    defer watch.deinit();
    const hash: [20]u8 = @splat(0x11);
    try watch.note("run", hash);
    try watch.note("hold", hash);
    try watch.note("full", hash);
    const Seen = struct {
        var activated: []const u8 = "";
        var fetched: []const u8 = "";
        var total: i64 = 0;
        var complete: bool = false;
        var moved: usize = 0;
        const Self = @This();
        fn status(_: ?*anyopaque, _: *const [20]u8) ?Live {
            if (!Self.complete) return .{ .has_metadata = true, .total_wanted = 10, .total_done = 4, .name = "n" };
            return .{ .has_metadata = true, .total_wanted = 10, .total_done = 10, .name = "n" };
        }
        fn activate(_: ?*anyopaque, id: []const u8) !void {
            Self.activated = id;
        }
        fn move(_: ?*anyopaque, _: []const u8, _: *const [20]u8) !void {
            Self.moved += 1;
            return error.MoveFailed;
        }
        fn fetch(_: ?*anyopaque, id: []const u8, _: i64, wanted: i64) !void {
            Self.fetched = id;
            Self.total = wanted;
        }
    };
    const jobs = [_]Job{
        .{ .id = "run", .phase = .queued },
        .{ .id = "hold", .phase = .queued, .paused = true },
        .{ .id = "full", .phase = .fetching },
    };
    const drv: Drive = .{
        .ctx = null,
        .move_fn = Seen.move,
        .activate_fn = Seen.activate,
        .fetch_fn = Seen.fetch,
    };
    watch.drive(&jobs, .{ .ctx = null, .status_fn = Seen.status }, drv, 4);
    try testing.expectEqualStrings("run", Seen.activated);
    try testing.expectEqualStrings("", Seen.fetched);
    try testing.expectEqual(@as(usize, 0), Seen.moved);

    Seen.complete = true;
    Seen.activated = "";
    watch.drive(&jobs, .{ .ctx = null, .status_fn = Seen.status }, drv, 4);
    try testing.expectEqualStrings("run", Seen.activated);
    try testing.expectEqualStrings("full", Seen.fetched);
    try testing.expectEqual(@as(i64, 10), Seen.total);
    try testing.expectEqual(@as(usize, 1), Seen.moved);
}

test "drive activates only as many queued downloads as the limit allows" {
    const testing = std.testing;
    var watch = Watch.init(testing.allocator);
    defer watch.deinit();
    const Seen = struct {
        var n: u32 = 0;
        const Self = @This();
        fn status(_: ?*anyopaque, _: *const [20]u8) ?Live {
            return null;
        }
        fn activate(_: ?*anyopaque, _: []const u8) !void {
            Self.n += 1;
        }
        fn move(_: ?*anyopaque, _: []const u8, _: *const [20]u8) !void {}
        fn fetch(_: ?*anyopaque, _: []const u8, _: i64, _: i64) !void {}
    };
    const jobs = [_]Job{
        .{ .id = "a", .phase = .queued },
        .{ .id = "b", .phase = .queued },
        .{ .id = "c", .phase = .fetching },
    };
    watch.drive(&jobs, .{ .ctx = null, .status_fn = Seen.status }, .{
        .ctx = null,
        .move_fn = Seen.move,
        .activate_fn = Seen.activate,
        .fetch_fn = Seen.fetch,
    }, 1);
    try testing.expectEqual(@as(u32, 0), Seen.n);
    watch.drive(&jobs, .{ .ctx = null, .status_fn = Seen.status }, .{
        .ctx = null,
        .move_fn = Seen.move,
        .activate_fn = Seen.activate,
        .fetch_fn = Seen.fetch,
    }, 2);
    try testing.expectEqual(@as(u32, 1), Seen.n);
}
