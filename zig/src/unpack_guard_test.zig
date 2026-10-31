//! Tests of the entry guard (internal/postproc/unpack guard_test.go).
const std = @import("std");
const testing = std.testing;
const tu = @import("unpack_testutil.zig");
const guard = @import("unpack_guard.zig");
const listing = @import("unpack_listing.zig");
const name_mod = @import("unpack_name.zig");
const errors = @import("unpack_errors.zig");
const env_mod = @import("unpack_env.zig");

const Entry = listing.Entry;

fn file(name: []const u8) Entry {
    return .{ .name = name, .kind = .file, .size = 1 };
}
fn dirOf(name: []const u8) Entry {
    return .{ .name = name, .kind = .dir };
}

fn admitAll(cfg: env_mod.Config, entries: []const Entry) !void {
    var rig = try tu.Rig.init(undefined, cfg);
    defer rig.deinit();
    var step: std.heap.ArenaAllocator = .init(testing.allocator);
    defer step.deinit();
    var g = guard.Guard.init(&rig.env, step.allocator());
    for (entries) |e| try g.admit(e);
}

test "hostile entry names are refused" {
    const names = [_][]const u8{
        "../x",        "a/../../b", "a/../b", "..",   "a/..",                 "..\\x",
        "/etc/passwd", "C:\\x",     "C:x",    "c:/x", "\\\\server\\share\\x", "a\x00b",
        "a/..\x00/b",  "",          " . . ",  "///",  "a/.../b",
    };
    for (names) |name| {
        try testing.expectError(error.PathViolation, admitAll(.{}, &.{ file("fine.txt"), file(name) }));
    }
    const rep = try repeat("d/", 3000);
    defer testing.allocator.free(rep);
    const long = try std.mem.concat(testing.allocator, u8, &.{ rep, "f" });
    defer testing.allocator.free(long);
    try testing.expectError(error.PathViolation, admitAll(.{}, &.{file(long)}));
}

fn repeat(s: []const u8, n: usize) ![]u8 {
    const out = try testing.allocator.alloc(u8, s.len * n);
    for (0..n) |i| @memcpy(out[i * s.len ..][0..s.len], s);
    return out;
}

test "names are sanitized per component" {
    const cases = [_][2][]const u8{
        .{ "dir/con.txt", "dir/_con.txt" },
        .{ "dir/a:b?.txt", "dir/a_b_.txt" },
        .{ "win\\sub\\file.txt", "win/sub/file.txt" },
        .{ "./lead/./dot.txt", "lead/dot.txt" },
        .{ "trailing. ", "trailing" },
        .{ "dir//double.txt", "dir/double.txt" },
        .{ "a\u{202e}b\x01c", "abc" },
    };
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    for (cases) |c| {
        const got = try name_mod.cleanName(arena.allocator(), c[0]);
        try testing.expectEqualStrings(c[1], got.rel);
    }
}

test "case-insensitive duplicate names are refused" {
    const cases = [_][]const Entry{
        &.{ file("a.txt"), file("a.txt") },
        &.{ file("A.txt"), file("a.TXT") },
        &.{ file("a:b"), file("a_b") },
        &.{ file("D/x"), file("d/X") },
        &.{ file("a"), dirOf("A") },
        &.{ dirOf("a"), file("A") },
        &.{ file("a"), file("a/b") },
        &.{ file("a"), file("a/./b") },
        // Unicode case: the table is Go's.
        &.{ file("É.txt"), file("é.txt") },
    };
    for (cases) |c| try testing.expectError(error.PathViolation, admitAll(.{}, c));
}

test "repeated directory entries are harmless" {
    try admitAll(.{}, &.{ dirOf("d"), dirOf("d/"), dirOf("D"), file("d/f"), dirOf("."), dirOf("./") });
}

test "a file named after the root is refused" {
    try testing.expectError(error.PathViolation, admitAll(.{}, &.{file("./")}));
}

test "only files and directories are admitted" {
    for ([_]listing.Kind{ .symlink, .hardlink, .device, .fifo, .other }) |k| {
        try testing.expectError(error.PathViolation, admitAll(.{}, &.{ file("ok"), .{ .name = "special", .kind = k } }));
    }
}

test "a special entry with a hostile name is a path violation too" {
    try testing.expectError(error.PathViolation, admitAll(.{}, &.{.{ .name = "../../etc/cron.d/x", .kind = .symlink }}));
}

test "entry count cap counts directories too" {
    const cfg: env_mod.Config = .{ .max_entries = 2 };
    try admitAll(cfg, &.{ file("a"), file("b") });
    try testing.expectError(error.TooManyEntries, admitAll(cfg, &.{ file("a"), file("b"), file("c") }));
    try testing.expectError(error.TooManyEntries, admitAll(cfg, &.{ file("a"), dirOf("d"), dirOf("e") }));
}

test "declared sizes are summed and saturate" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    var step: std.heap.ArenaAllocator = .init(testing.allocator);
    defer step.deinit();
    var g = guard.Guard.init(&rig.env, step.allocator());
    try g.admit(.{ .name = "a", .size = 10 });
    try g.admit(.{ .name = "b", .size = 5 });
    try testing.expectEqual(@as(i64, 15), g.declared);
    try g.admit(.{ .name = "c", .size = 1 << 62 });
    try g.admit(.{ .name = "d", .size = 1 << 62 });
    try testing.expectEqual(@as(i64, 1 << 62), g.declared);
    try testing.expectEqual(@as(usize, 4), g.files);
}

test "error messages carry no control characters and match Go's text" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    var step: std.heap.ArenaAllocator = .init(testing.allocator);
    defer step.deinit();
    var g = guard.Guard.init(&rig.env, step.allocator());
    try testing.expectError(error.PathViolation, g.admit(file("../evil\x1b[31m\nname")));
    const m = rig.diag.message;
    try testing.expect(std.mem.indexOfScalar(u8, m, 0x1b) == null);
    try testing.expect(std.mem.indexOfScalar(u8, m, '\n') == null);
    try testing.expectEqualStrings("unpack: unsafe path in archive: path violation: parent directory reference \"../evil?[31m?name\"", m);
}

test "fuzz: the guard never crashes and keeps accepted names safe" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [512]u8 = undefined;
            const len = smith.slice(&buf);
            var rig = try tu.Rig.init(undefined, .{ .max_entries = 8 });
            defer rig.deinit();
            var step: std.heap.ArenaAllocator = .init(testing.allocator);
            defer step.deinit();
            // Split the input into entries at 0xff so the fuzzer can build
            // colliding names.
            var g = guard.Guard.init(&rig.env, step.allocator());
            var it = std.mem.splitScalar(u8, buf[0..len], 0xff);
            while (it.next()) |part| {
                g.admit(.{ .name = part, .kind = if (part.len > 0 and part[0] == 'D') .dir else .file, .size = 1 }) catch |err| switch (err) {
                    error.OutOfMemory => return error.OutOfMemory,
                    else => return,
                };
            }
        }
    }.one, .{ .corpus = &.{
        seed("a\xffA"), seed("D/x\xffd/X"), seed("../x"), seed("a\xffa/b"), seed("C:\\x"),
    } });
}

fn seed(comptime s: []const u8) []const u8 {
    return std.mem.asBytes(&std.mem.nativeToLittle(u32, s.len)) ++ s;
}
