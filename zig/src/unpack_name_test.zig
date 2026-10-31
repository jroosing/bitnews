//! Tests of the name helpers beyond the golden file, and the fuzz target of
//! cleanName (internal/postproc/unpack fuzz_test.go, FuzzEntryName).
const std = @import("std");
const testing = std.testing;
const name_mod = @import("unpack_name.zig");
const fsutil = @import("fsutil.zig");

test "truncateForError cuts, masks controls and replaces invalid bytes" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try testing.expectEqualStrings("a?b", try name_mod.truncateForError(a, "a\x01b"));
    try testing.expectEqualStrings("a\u{fffd}b", try name_mod.truncateForError(a, "a\xffb"));
    const long: [100]u8 = @splat('x');
    const got = try name_mod.truncateForError(a, &long);
    try testing.expectEqual(@as(usize, 83), got.len);
    try testing.expect(std.mem.endsWith(u8, got, "..."));
    // A cut in the middle of a multi-byte sequence leaves invalid bytes.
    const cut = try name_mod.truncateForError(a, xs(79) ++ "日" ++ xs(10));
    try testing.expect(std.unicode.utf8ValidateSlice(cut));
}

test "trimSpace is Go's strings.TrimSpace" {
    try testing.expectEqualStrings("a b", name_mod.trimSpace(" \t a b\r\n"));
    try testing.expectEqualStrings("", name_mod.trimSpace(" \u{a0}\u{2003} "));
    try testing.expectEqualStrings("\xff", name_mod.trimSpace(" \xff "));
}

test "fuzz: cleanName results are plain relative paths that join safely" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [1024]u8 = undefined;
            const len = smith.slice(&buf);
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            const a = arena.allocator();
            const got = try name_mod.cleanName(a, buf[0..len]);
            const rel = switch (got) {
                .rel => |r| r,
                .violation => return,
            };
            if (rel.len == 0) return;
            if (std.mem.indexOfAny(u8, rel, "\x00\\") != null or rel[0] == '/') return error.NotRelative;
            var it = std.mem.splitScalar(u8, rel, '/');
            var depth: usize = 0;
            while (it.next()) |c| {
                if (c.len == 0 or std.mem.eql(u8, c, ".") or std.mem.eql(u8, c, "..")) return error.BadComponent;
                if (c.len > fsutil.max_name_bytes) return error.LongComponent;
                depth += 1;
            }
            if (depth > name_mod.max_path_depth or rel.len > name_mod.max_path_bytes) return error.TooBig;
            const joined = fsutil.join(a, &.{ "/stage", rel }) catch return error.OutOfMemory;
            const back = try fsutil.relBelow(a, "/stage", joined);
            if (back == null or std.mem.eql(u8, back.?, "..") or std.mem.startsWith(u8, back.?, "../")) return error.Escapes;
        }
    }.one, .{ .corpus = &.{
        seed("a.txt"),    seed("dir/a.txt"), seed("../x"),       seed("/etc/passwd"),
        seed("C:\\x"),    seed("a/../../b"), seed("a\x00b"),     seed("./"),
        seed("con.txt"),  seed(" . "),       seed("a\u{202e}b"), seed("..\u{2215}x"),
        seed("%2e%2e/x"), seed("....//x"),   seed("\xff\xfe"),   seed("a//b/"),
    } });
}

fn xs(comptime n: usize) [n]u8 {
    return @splat('x');
}

fn seed(comptime s: []const u8) []const u8 {
    return std.mem.asBytes(&std.mem.nativeToLittle(u32, s.len)) ++ s;
}
