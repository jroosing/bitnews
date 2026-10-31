//! Tests of the directory move (the TestMoveDir_* cases of
//! internal/fsutil/movedir_test.go), with the cross-device path forced by a
//! renamer that fails the way a rename across file systems does.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const move = @import("finalize_move.zig");
const util = @import("tidy_testutil.zig");
const Tree = util.Tree;

const io = util.io;

extern "c" fn mkfifo(path: [*:0]const u8, mode: std.c.mode_t) c_int;

fn crossDevice(_: Io, _: []const u8, _: []const u8) anyerror!void {
    return error.CrossDevice;
}

fn otherRenameError(_: Io, _: []const u8, _: []const u8) anyerror!void {
    return error.AccessDenied;
}

/// A release-like tree at src/release, and a place to move it to.
const Rig = struct {
    t: Tree,
    arena: std.heap.ArenaAllocator,

    fn init(r: *Rig) !void {
        r.t = try Tree.init();
        errdefer r.t.deinit();
        r.arena = .init(testing.allocator);
        try r.t.mkdir("src/release/Subs/empty");
        try r.t.write("src/release/movie.mkv", "video-bytes");
        try r.t.write("src/release/Subs/en.srt", "subs");
    }

    fn deinit(r: *Rig) void {
        r.arena.deinit();
        r.t.deinit();
    }

    fn a(r: *Rig) std.mem.Allocator {
        return r.arena.allocator();
    }

    fn p(r: *Rig, rel: []const u8) []const u8 {
        return r.t.abs(r.a(), rel) catch @panic("oom");
    }

    fn read(r: *Rig, rel: []const u8) ![]u8 {
        return r.t.read(r.a(), rel);
    }

    fn mv(r: *Rig, src: []const u8, dst: []const u8, renamer: ?move.Renamer) !move.MoveResult {
        var res: move.MoveResult = .{};
        try move.moveDir(testing.allocator, io, r.p(src), r.p(dst), &res, renamer);
        return res;
    }
};

test "a move on the same file system renames and makes the parents" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    const res = try r.mv("src/release", "complete/tv/release", null);
    try testing.expect(!res.cross_device);
    try testing.expect(!r.t.has("src/release"));
    try testing.expectEqualStrings("video-bytes", try r.read("complete/tv/release/movie.mkv"));
    try testing.expectEqualStrings("subs", try r.read("complete/tv/release/Subs/en.srt"));
    try testing.expect(r.t.has("complete/tv/release/Subs/empty"));
}

test "a rename keeps the inodes" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    const before = try r.t.tmp.dir.statFile(io, "src/release/movie.mkv", .{});
    _ = try r.mv("src/release", "out", null);
    const after = try r.t.tmp.dir.statFile(io, "out/movie.mkv", .{});
    try testing.expectEqual(before.inode, after.inode);
}

test "an existing destination is refused, on both paths" {
    for ([_]?move.Renamer{ null, crossDevice }) |renamer| {
        var r: Rig = undefined;
        try r.init();
        defer r.deinit();
        try r.t.write("out/keep", "mine");
        try testing.expectError(error.Exists, r.mv("src/release", "out", renamer));
        try testing.expectEqualStrings("mine", try r.read("out/keep"));
        try testing.expect(r.t.has("src/release/movie.mkv"));
        // A dangling link counts as existing too.
        try r.t.symlink("nowhere", "dangling");
        try testing.expectError(error.Exists, r.mv("src/release", "dangling", renamer));
    }
}

test "a missing or non-directory source is refused" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    try testing.expectError(error.FileNotFound, r.mv("src/nope", "out", null));
    try testing.expectError(error.NotDirectory, r.mv("src/release/movie.mkv", "out", null));
}

test "a destination inside the source is refused, also through a symlink" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    try testing.expectError(error.Nested, r.mv("src/release", "src/release/Subs/out", null));
    try r.t.symlink("src/release", "alias");
    try testing.expectError(error.Nested, r.mv("src/release", "alias/Subs/out", null));
    try testing.expect(!r.t.has("src/release/Subs/out"));
}

test "other rename errors are returned without copying" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    try testing.expectError(error.AccessDenied, r.mv("src/release", "out", otherRenameError));
    try testing.expect(!r.t.has("out"));
    try testing.expect(r.t.has("src/release/movie.mkv"));
}

test "a cross-device move copies, then removes the source" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    const res = try r.mv("src/release", "dst/release", crossDevice);
    try testing.expect(res.cross_device);
    try testing.expect(!r.t.has("src/release"));
    try testing.expectEqualStrings("video-bytes", try r.read("dst/release/movie.mkv"));
    try testing.expectEqualStrings("subs", try r.read("dst/release/Subs/en.srt"));
    try testing.expect(r.t.has("dst/release/Subs/empty"));
}

test "a cross-device copy keeps permission bits and modification times" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    const stamp: Io.Timestamp = .{ .nanoseconds = 1_600_000_000 * std.time.ns_per_s };
    try r.t.tmp.dir.setFilePermissions(io, "src/release/Subs/en.srt", .fromMode(0o640), .{});
    try r.t.tmp.dir.setFilePermissions(io, "src/release/movie.mkv", .fromMode(0o4755), .{}); // setuid is dropped
    try r.t.tmp.dir.setTimestamps(io, "src/release/movie.mkv", .{ .modify_timestamp = .{ .new = stamp }, .access_timestamp = .{ .new = stamp } });
    try r.t.tmp.dir.setFilePermissions(io, "src/release/Subs", .fromMode(0o750), .{});
    try r.t.tmp.dir.setTimestamps(io, "src/release/Subs", .{ .modify_timestamp = .{ .new = stamp }, .access_timestamp = .{ .new = stamp } });

    _ = try r.mv("src/release", "out", crossDevice);

    const srt = try r.t.tmp.dir.statFile(io, "out/Subs/en.srt", .{});
    try testing.expectEqual(@as(u32, 0o640), srt.permissions.toMode() & 0o7777);
    const mkv = try r.t.tmp.dir.statFile(io, "out/movie.mkv", .{});
    try testing.expectEqual(@as(u32, 0o755), mkv.permissions.toMode() & 0o7777);
    try testing.expectEqual(stamp.nanoseconds, mkv.mtime.nanoseconds);
    const subs = try r.t.tmp.dir.statFile(io, "out/Subs", .{});
    try testing.expectEqual(@as(u32, 0o750), subs.permissions.toMode() & 0o7777);
    try testing.expectEqual(stamp.nanoseconds, subs.mtime.nanoseconds);
}

test "a cross-device copy handles read-only directories" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    try r.t.tmp.dir.setFilePermissions(io, "src/release/Subs", .fromMode(0o500), .{});

    _ = try r.mv("src/release", "out", crossDevice);

    try testing.expect(!r.t.has("src/release"));
    try testing.expectEqualStrings("subs", try r.read("out/Subs/en.srt"));
    const subs = try r.t.tmp.dir.statFile(io, "out/Subs", .{});
    try testing.expectEqual(@as(u32, 0o500), subs.permissions.toMode() & 0o777);
    try r.t.tmp.dir.setFilePermissions(io, "out/Subs", .fromMode(0o700), .{}); // so cleanup works
}

test "a cross-device copy refuses symlinks and leaves the source intact" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    try r.t.write("outside/secret", "s");
    try r.t.symlink(r.p("outside"), "src/release/Subs/link");

    try testing.expectError(error.Symlink, r.mv("src/release", "out", crossDevice));

    try testing.expect(!r.t.has("out"));
    try testing.expectEqualStrings("video-bytes", try r.read("src/release/movie.mkv"));
    try testing.expectEqualStrings("s", try r.read("outside/secret"));
}

test "a symlinked source root is refused" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    try r.t.symlink(r.p("src/release"), "root-link");
    for ([_]?move.Renamer{ null, crossDevice }) |renamer| {
        try testing.expectError(error.Symlink, r.mv("root-link", "out", renamer));
        try testing.expect(!r.t.has("out"));
    }
}

test "a failed cross-device copy removes the partial copy and keeps the source" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    // A FIFO sorts after "Subs" and "movie.mkv", so the copy is under way.
    const path = try std.fmt.allocPrintSentinel(r.a(), "{s}/src/release/zfifo", .{r.t.path}, 0);
    try testing.expectEqual(@as(c_int, 0), mkfifo(path, 0o600));

    try testing.expectError(error.SpecialFile, r.mv("src/release", "out", crossDevice));

    try testing.expect(!r.t.has("out"));
    try testing.expectEqualStrings("video-bytes", try r.read("src/release/movie.mkv"));
    try testing.expect(r.t.has("src/release/zfifo"));
}

test "a cross-device copy makes hardlinked files separate files" {
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    try r.t.tmp.dir.hardLink("src/release/movie.mkv", r.t.tmp.dir, "src/release/movie-link.mkv", io, .{});

    _ = try r.mv("src/release", "out", crossDevice);

    const a = try r.t.tmp.dir.statFile(io, "out/movie.mkv", .{});
    const b = try r.t.tmp.dir.statFile(io, "out/movie-link.mkv", .{});
    try testing.expect(a.inode != b.inode);
    try testing.expectEqualStrings("video-bytes", try r.read("out/movie-link.mkv"));
}

test "a cross-device copy keeps the complete copy when the source cannot be removed" {
    if (@import("fsutil.zig").effectiveUid() == 0) return error.SkipZigTest;
    var r: Rig = undefined;
    try r.init();
    defer r.deinit();
    // Removing release/ needs write access to src/.
    try r.t.tmp.dir.setFilePermissions(io, "src", .fromMode(0o500), .{});
    defer r.t.tmp.dir.setFilePermissions(io, "src", .fromMode(0o700), .{}) catch {};

    var res: move.MoveResult = .{};
    const result = move.moveDir(testing.allocator, io, r.p("src/release"), r.p("out"), &res, crossDevice);

    try testing.expect(std.meta.isError(result));
    try testing.expect(res.cross_device);
    try testing.expectEqualStrings("video-bytes", try r.read("out/movie.mkv"));
    try testing.expectEqualStrings("subs", try r.read("out/Subs/en.srt"));
}
