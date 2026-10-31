//! Tests of the listing parser (internal/postproc/unpack listing_test.go and
//! FuzzParseListing); the golden test compares it with Go on many inputs.
const std = @import("std");
const testing = std.testing;
const tu = @import("unpack_testutil.zig");
const listing = @import("unpack_listing.zig");

fn parseIn(a: std.mem.Allocator, in: []const u8) ![]listing.Entry {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    return listing.parse(&rig.env, a, in, std.math.maxInt(usize));
}

test "names, sizes and kinds are read" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const out = "\nEnter password:\n" ++
        "Path = docs\nFolder = +\nSize = 0\nAttributes = D drwxr-xr-x\nEncrypted = -\n\n" ++
        "Path = docs/a b.txt\nFolder = -\nSize = 12\nPacked Size = 5\nAttributes = A -rw-r--r--\nEncrypted = +\nCreated = \n\n" ++
        "Path = l\nSize = 3\nAttributes =  lrwxrwxrwx\n\n" ++
        "Path = win\nSize = 1\nAttributes = RAL\n\n" ++
        "Path = p\nSize = 0\nAttributes = A prw-r--r--\n\n" ++
        "Path = h\nSize = 0\nHard Link = other\n\n" ++
        "Path = plain\nSize = 7\nAttributes = A\n\n";
    const got = try parseIn(arena.allocator(), out);
    const want = [_]listing.Entry{
        .{ .name = "docs", .kind = .dir },
        .{ .name = "docs/a b.txt", .kind = .file, .size = 12, .encrypted = true },
        .{ .name = "l", .kind = .symlink, .size = 3 },
        .{ .name = "win", .kind = .symlink, .size = 1 },
        .{ .name = "p", .kind = .fifo },
        .{ .name = "h", .kind = .hardlink },
        .{ .name = "plain", .kind = .file, .size = 7 },
    };
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| {
        try testing.expectEqualStrings(w.name, g.name);
        try testing.expectEqual(w.kind, g.kind);
        try testing.expectEqual(w.size, g.size);
        try testing.expectEqual(w.encrypted, g.encrypted);
    }
}

test "a listing without a trailing blank line and CRLF output" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const got = try parseIn(a, "Path = a\nSize = 1");
    try testing.expectEqual(@as(usize, 1), got.len);
    try testing.expectEqual(@as(i64, 1), got[0].size);
    const crlf = try parseIn(a, "Path = a\r\nSize = 3\r\nSomething New = x\r\n\r\n");
    try testing.expectEqual(@as(usize, 1), crlf.len);
    try testing.expectEqualStrings("a", crlf[0].name);
}

test "nothing to list is an empty listing" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    for ([_][]const u8{ "", "\n", "\nEnter password:\n\n" }) |in| {
        try testing.expectEqual(@as(usize, 0), (try parseIn(arena.allocator(), in)).len);
    }
}

test "forged or malformed output is refused" {
    const kk: [100]u8 = @splat('k');
    const cases = [_][]const u8{
        "Path = a\nSize = 0\nAttributes = D\nFolder = -\nSize = 99999999\n\n",
        "Path = a\nb\nSize = 5\n\n",
        "Path = a\nPath = ../../x\nSize = 5\n\n",
        "Path = a\nSize = 5\nEncrypted = -\nComment = x\nEncrypted = -\n\n",
        "Path = a\nSize = 5\nSize = 6\n\n",
        "Path = a\nthis is not a field\n\n",
        "Path = a\nSize = 5\n\nERRORS:\nHeaders Error\n",
        "Path = a\nSize = -1\n\n",
        "Path = a\nSize = 12x\n\n",
        "Path = a\nSize = 99999999999999999999\n\n",
        "Path = a\nSize = 9223372036854775807\n\n",
        "Path = a\n = 5\n\n",
        "Path = a\n" ++ kk ++ " = 5\n\n",
        "Path = a\nSize = 1\nPath = b\n\n",
    };
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    for (cases) |in| try testing.expectError(error.Corrupt, parseIn(arena.allocator(), in));
}

test "entries past the keep limit are checked but not stored" {
    var rig = try tu.Rig.init(undefined, .{});
    defer rig.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const ok = "Path = a\nSize = 1\n\nPath = b\nSize = 1\n\nPath = c\nSize = 1\n\n";
    const got = try listing.parse(&rig.env, arena.allocator(), ok, 2);
    try testing.expectEqual(@as(usize, 2), got.len);
    // A malformed line after the limit still fails the listing.
    try testing.expectError(error.Corrupt, listing.parse(&rig.env, arena.allocator(), ok ++ "Path = d\nbad\n\n", 2));
}

test "fuzz: parse never crashes or leaks and results are well formed" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [2048]u8 = undefined;
            const len = smith.slice(&buf);
            const data = buf[0..len];
            var rig = try tu.Rig.init(undefined, .{});
            defer rig.deinit();
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            const entries = listing.parse(&rig.env, arena.allocator(), data, std.math.maxInt(usize)) catch |err| switch (err) {
                error.Corrupt => return,
                else => return err,
            };
            // No more entries than "Path = " lines.
            var paths: usize = std.mem.count(u8, data, "\nPath = ");
            if (std.mem.startsWith(u8, data, "Path = ")) paths += 1;
            if (entries.len > paths) return error.MoreEntriesThanPaths;
            for (entries) |e| {
                if (e.size < 0 or e.size > 1 << 62) return error.SizeOutOfRange;
                if (e.kind == .dir and e.size != 0) return error.DirWithSize;
                if (std.mem.indexOfScalar(u8, e.name, '\n') != null) return error.NameHasLineBreak;
            }
            // Deterministic.
            const again = try listing.parse(&rig.env, arena.allocator(), data, std.math.maxInt(usize));
            if (again.len != entries.len) return error.NotDeterministic;
        }
    }.one, .{ .corpus = &.{
        seed(""),
        seed("\nEnter password:\n"),
        seed("Path = a\nSize = 1\nAttributes = A -rw-r--r--\nEncrypted = -\n\n"),
        seed("Path = d\nFolder = +\nSize = 0\nAttributes = D drwxr-xr-x\n\nPath = d/f\nSize = 3\n\n"),
        seed("Path = l\nSize = 3\nAttributes =  lrwxrwxrwx\n\n"),
        seed("Path = a\nSize = 0\nAttributes = D\nFolder = -\nSize = 99999999\n\n"),
        seed("Path = a\nb\nSize = 5\n\n"),
        seed("Path = a\nPath = ../../x\n\n"),
        seed("Path = a\nSize = -1\n\n"),
        seed("Path = a\nSize = 99999999999999999999\n\n"),
        seed("Path = a\n = 5\n\n"),
        seed("Path = h\nHard Link = x\n\n"),
        seed("Path = a\r\nSize = 3\r\n\r\n"),
        seed("Path = a\nSize = 1\n\nERRORS:\nHeaders Error\n"),
    } });
}

fn seed(comptime s: []const u8) []const u8 {
    return std.mem.asBytes(&std.mem.nativeToLittle(u32, s.len)) ++ s;
}
