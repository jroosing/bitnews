//! Tests of Detect (internal/postproc/unpack detect_test.go, detect_unix_test.go).
const std = @import("std");
const testing = std.testing;
const tu = @import("unpack_testutil.zig");
const detect = @import("unpack_detect.zig");

const magic_rar4 = "Rar!\x1a\x07\x00";
const magic_rar5 = "Rar!\x1a\x07\x01\x00";
const magic_7z = "7z\xbc\xaf\x27\x1c";

/// A file that carries only a magic header and some padding.
fn fakeVolume(comptime magic: []const u8) []const u8 {
    const zeros: [32]u8 = @splat(0);
    return magic ++ zeros;
}

fn detectIn(rig: *tu.Rig, rel: []const u8) ![]detect.ArchiveSet {
    return detect.detect(&rig.env, if (rel.len == 0) rig.root else rig.path(rel));
}

fn expectPaths(rig: *tu.Rig, want: []const []const u8, got: []const []const u8) !void {
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expectEqualStrings(rig.path(w), g);
}

test "a single RAR5 archive is found by magic" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("movie.rar", fakeVolume(magic_rar5));
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expectEqual(detect.Format.rar, sets[0].format);
    try testing.expectEqualStrings("movie", sets[0].name);
    try expectPaths(rig, &.{"movie.rar"}, sets[0].volumes);
    try testing.expect(sets[0].err == null);
}

test "files without archive magic are ignored whatever the extension" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("fake.rar", "this is not a rar");
    try rig.write("fake.7z", "");
    try rig.write("notes.txt", "hello");
    try rig.write("short.rar", "Rar!");
    try testing.expectEqual(@as(usize, 0), (try detectIn(rig, "")).len);
}

test "an archive with an obfuscated name is found" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("a1b2c3d4", fakeVolume(magic_rar4));
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expectEqual(detect.Format.rar, sets[0].format);
    try expectPaths(rig, &.{"a1b2c3d4"}, sets[0].volumes);
}

test "new-style RAR volumes are grouped in order" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    var want: std.ArrayList([]const u8) = .empty;
    for (1..13) |i| try want.append(rig.arena(), try std.fmt.allocPrint(rig.arena(), "show.part{d:0>2}.rar", .{i}));
    // Created in reverse, with a mix of RAR4 and RAR5 headers.
    var i: usize = want.items.len;
    while (i > 0) {
        i -= 1;
        try rig.write(want.items[i], if (i % 2 == 0) fakeVolume(magic_rar4) else fakeVolume(magic_rar5));
    }
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expect(sets[0].err == null);
    try testing.expectEqualStrings("show", sets[0].name);
    try expectPaths(rig, want.items, sets[0].volumes);
}

test "old-style RAR volumes are grouped" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    const names = [_][]const u8{ "old.rar", "old.r00", "old.r01", "old.r02" };
    for (names) |n| try rig.write(n, fakeVolume(magic_rar4));
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expect(sets[0].err == null);
    try testing.expectEqualStrings("old", sets[0].name);
    try expectPaths(rig, &names, sets[0].volumes);
}

test "old-style volumes roll over from r99 to s00" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    var want: std.ArrayList([]const u8) = .empty;
    try want.append(rig.arena(), "big.rar");
    for (0..100) |i| try want.append(rig.arena(), try std.fmt.allocPrint(rig.arena(), "big.r{d:0>2}", .{i}));
    try want.append(rig.arena(), "big.s00");
    for (want.items) |n| try rig.write(n, fakeVolume(magic_rar4));
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expect(sets[0].err == null);
    try expectPaths(rig, want.items, sets[0].volumes);
}

test "single and multi-volume 7z sets are found, ordered by name" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("one.7z", fakeVolume(magic_7z));
    try rig.write("multi.7z.001", fakeVolume(magic_7z));
    try rig.write("multi.7z.002", "continuation bytes without any magic");
    try rig.write("multi.7z.003", "more");
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 2), sets.len);
    try testing.expectEqualStrings("multi", sets[0].name);
    try expectPaths(rig, &.{ "multi.7z.001", "multi.7z.002", "multi.7z.003" }, sets[0].volumes);
    try testing.expectEqualStrings("one", sets[1].name);
    try expectPaths(rig, &.{"one.7z"}, sets[1].volumes);
}

test "orphan 7z volumes without a magic first volume are ignored" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("lost.7z.002", "continuation");
    try rig.write("fake.7z.001", "not a 7z first volume");
    try rig.write("fake.7z.002", "continuation");
    try testing.expectEqual(@as(usize, 0), (try detectIn(rig, "")).len);
}

test "a missing middle volume is reported on the set" {
    const Case = struct { files: []const []const u8, seven: bool };
    const cases = [_]Case{
        .{ .files = &.{ "a.part01.rar", "a.part02.rar", "a.part04.rar" }, .seven = false },
        .{ .files = &.{ "a.rar", "a.r00", "a.r02" }, .seven = false },
        .{ .files = &.{ "a.7z.001", "a.7z.002", "a.7z.004" }, .seven = true },
    };
    for (cases) |c| {
        var rig = try tu.Rig.init(undefined, .{});
        defer rig.deinit();
        for (c.files, 0..) |f, i| {
            const content: []const u8 = if (c.seven) (if (i == 0) fakeVolume(magic_7z) else "continuation") else fakeVolume(magic_rar5);
            try rig.write(f, content);
        }
        const sets = try detectIn(rig, "");
        try testing.expectEqual(@as(usize, 1), sets.len);
        try testing.expect(sets[0].err != null);
        try testing.expect(std.mem.indexOf(u8, sets[0].err.?, "missing archive volume") != null);
        try testing.expectEqual(@as(usize, 3), sets[0].volumes.len);
    }
}

test "a missing first volume is reported" {
    for ([_][2][]const u8{ .{ "a.part02.rar", "a.part03.rar" }, .{ "a.r00", "a.r01" } }) |files| {
        var rig = try tu.Rig.init(undefined, .{});
        defer rig.deinit();
        for (files) |f| try rig.write(f, fakeVolume(magic_rar4));
        const sets = try detectIn(rig, "");
        try testing.expectEqual(@as(usize, 1), sets.len);
        try testing.expect(sets[0].err != null);
    }
}

test "two files for the same volume number are reported" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("a.part1.rar", fakeVolume(magic_rar5));
    try rig.write("a.part01.rar", fakeVolume(magic_rar5));
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expectEqualStrings("unpack: missing archive volume: archive corrupt: a has several files for volume 1", sets[0].err.?);
}

test "extensions are matched case-insensitively" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("SHOW.PART1.RAR", fakeVolume(magic_rar5));
    try rig.write("SHOW.PART2.RAR", fakeVolume(magic_rar5));
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expect(sets[0].err == null);
    try expectPaths(rig, &.{ "SHOW.PART1.RAR", "SHOW.PART2.RAR" }, sets[0].volumes);
}

test "symlinks, directories and special files are ignored" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("outside/real.rar", fakeVolume(magic_rar5));
    try rig.mkdir("dir");
    try rig.write("dir/dir.rar/inner.rar", fakeVolume(magic_rar5));
    try rig.tmp.dir.symLink(tu.io, rig.path("outside/real.rar"), "dir/link.rar", .{});
    try rig.tmp.dir.createDirPath(tu.io, "dir/sub.rar");
    // A FIFO must not block the sniff.
    try tu.makeFifo(rig, "dir/pipe.rar");
    try testing.expectEqual(@as(usize, 0), (try detectIn(rig, "dir")).len);
}

test "a symlinked volume counts as missing" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("a.part1.rar", fakeVolume(magic_rar5));
    try rig.write("outside/real", fakeVolume(magic_rar5));
    try rig.tmp.dir.symLink(tu.io, rig.path("outside/real"), "a.part2.rar", .{});
    try rig.write("a.part3.rar", fakeVolume(magic_rar5));
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expect(sets[0].err != null);
}

test "thousands of files are handled deterministically" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    for (0..1500) |i| try rig.write(try std.fmt.allocPrint(rig.arena(), "junk-{d:0>4}.bin", .{i}), "x");
    for (0..50) |i| try rig.write(try std.fmt.allocPrint(rig.arena(), "set{d:0>2}.rar", .{i}), fakeVolume(magic_rar5));
    const first = try detectIn(rig, "");
    const second = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 50), first.len);
    try testing.expectEqual(first.len, second.len);
    for (first, second) |x, y| try testing.expectEqualStrings(x.name, y.name);
    for (1..first.len) |i| try testing.expect(std.mem.order(u8, first[i - 1].name, first[i].name) == .lt);
}

test "a truncated file with a valid magic is kept: Unpack reports the corruption" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("cut.rar", magic_rar5[0..6]);
    try rig.write("cut2.rar", magic_rar5);
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try expectPaths(rig, &.{"cut2.rar"}, sets[0].volumes);
}

test "a missing directory is an error" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try testing.expectError(error.Internal, detectIn(rig, "nope"));
}

test "same-named sets of different families stay apart, case-insensitively grouped" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    try rig.write("Show.part1.rar", fakeVolume(magic_rar5));
    try rig.write("show.PART2.rar", fakeVolume(magic_rar5));
    const sets = try detectIn(rig, "");
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expectEqual(@as(usize, 2), sets[0].volumes.len);
}

test "fuzz: volume-name matching and grouping never crash" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [200]u8 = undefined;
            const len = smith.slice(&buf);
            const name = buf[0..len];
            if (detect.matchSevenZ(name)) |m| {
                if (m.digits.len < 3 or m.digits.len > 9) return error.BadDigits;
                if (!std.mem.startsWith(u8, name, m.base)) return error.BadBase;
            }
            if (detect.matchNewStyle(name)) |m| {
                if (m.digits.len == 0) return error.BadDigits;
                if (m.base.len == 0) return error.EmptyBase;
            }
            if (detect.matchOldFirst(name)) |b| if (b.len == 0) return error.EmptyBase;
            if (detect.matchOldCont(name)) |m| if (m.base.len == 0 or m.digits.len != 2) return error.BadMatch;
        }
    }.one, .{ .corpus = &.{
        seed("a.7z.001"),   seed("show.part01.rar"), seed("old.r00"),
        seed("x.rar"),      seed("A.PART1.RAR"),     seed("a.s99"),
        seed("a.part.rar"), seed("a\n.rar"),         seed("a.\xc5\xbf00"),
    } });
}

fn seed(comptime s: []const u8) []const u8 {
    return std.mem.asBytes(&std.mem.nativeToLittle(u32, s.len)) ++ s;
}
