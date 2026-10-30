//! What a fetch has to do for one NZB (internal/usenet/runner plan.go): which
//! Articles to fetch, and the small file-system helpers that clear the leftovers
//! of an interrupted run.
//!
//! Recovery volumes (`name.volNN+MM.par2`) are deferred: Repair asks for them
//! later (`runner_volumes`). The plan decides that from the file kind the NZB
//! parser guessed from the NAME; it never looks inside a PAR2 file.
//!
//! The file key of an Article is the index of its file in `Nzb.files`.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const nzb = @import("nzb.zig");
const sched = @import("scheduler.zig");
const blob = @import("runner_blob.zig");

pub const Status = blob.Status;

pub const Plan = struct {
    /// Every Article to fetch, sorted by (file key, segment number): the
    /// dispatch order.
    articles: []const sched.Article,
    /// The file keys that have Articles, ascending.
    file_keys: []const u32,
    /// The provisional names of the fetched files, parallel to `file_keys`.
    names: []const []const u8,
    /// The sum of the NZB segment sizes of the fetched files.
    encoded: i64,

    /// Turns `n` into Articles; everything is allocated in `a`, and the
    /// message IDs point into `n`.
    ///
    /// With `only` null the plan covers every file except the recovery
    /// volumes. With `only` (the volumes that were un-deferred and not
    /// fetched yet, as file indexes, ascending) it covers exactly those
    /// volumes: the Download was fetched before and only needs the volumes
    /// Repair asked for.
    pub fn build(a: Allocator, n: nzb.Nzb, only: ?[]const u32) Allocator.Error!Plan {
        var arts: std.ArrayList(sched.Article) = .empty;
        var keys: std.ArrayList(u32) = .empty;
        var names: std.ArrayList([]const u8) = .empty;
        var encoded: i64 = 0;
        for (n.files, 0..) |f, fi| {
            if (f.segments.len == 0) continue;
            if (only) |set| {
                if (!f.deferred() or std.mem.indexOfScalar(u32, set, @intCast(fi)) == null) continue;
            } else if (f.deferred()) continue;
            try keys.append(a, @intCast(fi));
            try names.append(a, f.name);
            try arts.ensureUnusedCapacity(a, f.segments.len);
            for (f.segments) |s| {
                arts.appendAssumeCapacity(.{
                    .file_key = @intCast(fi),
                    .segment_number = s.number,
                    .message_id = s.message_id,
                    .expected_bytes = s.bytes,
                    .date = f.date,
                });
                encoded = std.math.add(i64, encoded, @max(s.bytes, 0)) catch std.math.maxInt(i64);
            }
        }
        std.mem.sort(sched.Article, arts.items, {}, lessArticle);
        return .{ .articles = arts.items, .file_keys = keys.items, .names = names.items, .encoded = encoded };
    }

    fn lessArticle(_: void, x: sched.Article, y: sched.Article) bool {
        if (x.file_key != y.file_key) return x.file_key < y.file_key;
        return x.segment_number < y.segment_number;
    }

    /// The index in `articles` of (file, seg), or null.
    pub fn find(p: Plan, file: u32, segment: u32) ?usize {
        const Key = struct { file: u32, seg: u32 };
        const key: Key = .{ .file = file, .seg = segment };
        const idx = std.sort.binarySearch(sched.Article, p.articles, key, struct {
            fn order(k: Key, art: sched.Article) std.math.Order {
                if (k.file != art.file_key) return std.math.order(k.file, art.file_key);
                return std.math.order(k.seg, art.segment_number);
            }
        }.order);
        return idx;
    }

    /// The Articles whose status is still `unresolved`, in `a`.
    pub fn remaining(p: Plan, a: Allocator, status: []const Status) Allocator.Error![]const sched.Article {
        std.debug.assert(status.len == p.articles.len);
        var out: std.ArrayList(sched.Article) = .empty;
        for (p.articles, status) |art, st| {
            if (st == .unresolved) try out.append(a, art);
        }
        return out.items;
    }
};

fn isNzbName(name: []const u8) bool {
    return std.ascii.endsWithIgnoreCase(name, ".nzb");
}

/// Deletes the files a previous, unrecoverable run left directly in `dir`,
/// keeping the stored NZB. Directories are left alone, and a missing `dir` is
/// fine (the writer creates it).
pub fn wipeDir(gpa: Allocator, io: Io, dir: []const u8) !void {
    var d = Io.Dir.cwd().openDir(io, dir, .{ .iterate = true, .access_sub_paths = true }) catch |err| switch (err) {
        error.FileNotFound => return,
        else => |e| return e,
    };
    defer d.close(io);
    // Collect first: deleting while iterating may skip entries.
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    var names: std.ArrayList([]const u8) = .empty;
    var it = d.iterate();
    while (try it.next(io)) |e| {
        if (e.kind == .directory or isNzbName(e.name)) continue;
        try names.append(arena.allocator(), try arena.allocator().dupe(u8, e.name));
    }
    for (names.items) |n| d.deleteFile(io, n) catch |err| switch (err) {
        error.FileNotFound => {},
        else => |e| return e,
    };
}

/// Deletes the regular files directly inside `dir` whose names are in `names`,
/// the provisional names of the files about to be fetched. It removes what an
/// interrupted earlier run of the same files left, so the writer does not
/// create duplicates next to them. Names that are not plain file names are
/// skipped.
pub fn removeNamed(io: Io, dir: []const u8, names: []const []const u8) !void {
    var d = Io.Dir.cwd().openDir(io, dir, .{ .access_sub_paths = true }) catch |err| switch (err) {
        error.FileNotFound => return,
        else => |e| return e,
    };
    defer d.close(io);
    for (names) |name| {
        if (name.len == 0 or std.mem.eql(u8, name, ".") or std.mem.eql(u8, name, "..") or
            std.mem.indexOfScalar(u8, name, '/') != null or std.mem.indexOfScalar(u8, name, 0) != null or isNzbName(name)) continue;
        const st = d.statFile(io, name, .{ .follow_symlinks = false }) catch continue;
        if (st.kind != .file) continue;
        d.deleteFile(io, name) catch |err| switch (err) {
            error.FileNotFound => {},
            else => |e| return e,
        };
    }
}

const testing = std.testing;

fn seg(number: u32, bytes: i64, id: []const u8) nzb.Segment {
    return .{ .number = number, .bytes = bytes, .message_id = id };
}

test "the plan holds every file but the recovery volumes, and sizes them" {
    const data = [_]nzb.Segment{ seg(1, 100, "a1"), seg(2, 50, "a2") };
    const idx = [_]nzb.Segment{seg(1, 10, "i1")};
    const vol = [_]nzb.Segment{seg(1, 7, "v1")};
    const files = [_]nzb.File{
        .{ .name = "a.bin", .segments = &data },
        .{ .name = "a.par2", .kind = .par2_index, .segments = &idx },
        .{ .name = "a.vol0+2.par2", .kind = .par2_volume, .recovery_blocks = 2, .segments = &vol },
        .{ .name = "empty", .segments = &.{} },
    };
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const p = try Plan.build(arena.allocator(), .{ .files = &files }, null);
    try testing.expectEqual(3, p.articles.len);
    try testing.expectEqualSlices(u32, &.{ 0, 1 }, p.file_keys);
    try testing.expectEqual(160, p.encoded);
    try testing.expectEqualStrings("a.par2", p.names[1]);
    try testing.expectEqual(2, p.find(1, 1).?);
    try testing.expectEqual(null, p.find(2, 1));
    try testing.expectEqual(null, p.find(0, 3));
}

test "a supplement plan holds exactly the chosen volumes" {
    const one = [_]nzb.Segment{seg(1, 10, "x")};
    const files = [_]nzb.File{
        .{ .name = "a.bin", .segments = &one },
        .{ .name = "v1", .kind = .par2_volume, .recovery_blocks = 1, .segments = &one },
        .{ .name = "v2", .kind = .par2_volume, .recovery_blocks = 1, .segments = &one },
    };
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const p = try Plan.build(arena.allocator(), .{ .files = &files }, &.{2});
    try testing.expectEqualSlices(u32, &.{2}, p.file_keys);
    // A data file is never part of a supplement, even when named.
    const q = try Plan.build(arena.allocator(), .{ .files = &files }, &.{0});
    try testing.expectEqual(0, q.articles.len);
}

test "remaining skips the resolved Articles" {
    const segs = [_]nzb.Segment{ seg(1, 1, "a"), seg(2, 1, "b"), seg(3, 1, "c") };
    const files = [_]nzb.File{.{ .name = "a", .segments = &segs }};
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const p = try Plan.build(a, .{ .files = &files }, null);
    var st = [_]Status{ .done, .unresolved, .missing };
    const rest = try p.remaining(a, &st);
    try testing.expectEqual(1, rest.len);
    try testing.expectEqualStrings("b", rest[0].message_id);
}

test "wipeDir keeps the NZB and the directories, removeNamed only what it is told" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const io = testing.io;
    try tmp.dir.writeFile(io, .{ .sub_path = "keep.NZB", .data = "x" });
    try tmp.dir.writeFile(io, .{ .sub_path = "a.bin", .data = "x" });
    try tmp.dir.writeFile(io, .{ .sub_path = "b.bin", .data = "x" });
    try tmp.dir.createDir(io, "sub", .default_dir);
    try tmp.dir.writeFile(io, .{ .sub_path = "sub/inner", .data = "x" });
    var buf: [Io.Dir.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(io, &buf);
    const path = buf[0..n];

    try removeNamed(io, path, &.{ "a.bin", "../escape", "keep.NZB", "missing", "sub" });
    try testing.expectError(error.FileNotFound, tmp.dir.statFile(io, "a.bin", .{}));
    _ = try tmp.dir.statFile(io, "b.bin", .{});
    _ = try tmp.dir.statFile(io, "keep.NZB", .{});
    _ = try tmp.dir.statFile(io, "sub", .{});

    try wipeDir(testing.allocator, io, path);
    try testing.expectError(error.FileNotFound, tmp.dir.statFile(io, "b.bin", .{}));
    _ = try tmp.dir.statFile(io, "keep.NZB", .{});
    _ = try tmp.dir.statFile(io, "sub/inner", .{});
    try wipeDir(testing.allocator, io, "/nonexistent/for/sure");
}
