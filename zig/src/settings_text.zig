//! The few text rules of Go that the settings messages and checks rely on:
//! strconv.Quote and QuoteRune (the `%q` of the error messages), the Unicode
//! white space of strings.TrimSpace, and "is this path clean".
//!
//! Whether a code point is printable is an approximation of Go's unicode
//! tables (the controls, the format characters, the separators, private use
//! and non-characters are not printable; unassigned code points count as
//! printable). It only decides how an exotic character is spelled inside an
//! error message.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;

fn isPrint(cp: u21) bool {
    if (cp < 0x20 or cp == 0x7f) return false;
    if (cp < 0x7f) return true;
    const ranges = [_][2]u21{
        .{ 0x80, 0xa0 },       .{ 0xad, 0xad },       .{ 0x600, 0x605 },      .{ 0x61c, 0x61c },
        .{ 0x6dd, 0x6dd },     .{ 0x70f, 0x70f },     .{ 0x1680, 0x1680 },    .{ 0x180e, 0x180e },
        .{ 0x2000, 0x200f },   .{ 0x2028, 0x202f },   .{ 0x205f, 0x206f },    .{ 0x3000, 0x3000 },
        .{ 0xd800, 0xf8ff },   .{ 0xfeff, 0xfeff },   .{ 0xfff9, 0xfffb },    .{ 0xfffe, 0xffff },
        .{ 0xe0001, 0xe0001 }, .{ 0xe0020, 0xe007f }, .{ 0xf0000, 0x10ffff },
    };
    for (ranges) |r| if (cp >= r[0] and cp <= r[1]) return false;
    return true;
}

fn writeEscapedRune(w: *Io.Writer, cp: u21, quote_char: u8) Io.Writer.Error!void {
    if (cp == quote_char or cp == '\\') {
        try w.writeByte('\\');
        try w.writeByte(@intCast(cp));
        return;
    }
    if (isPrint(cp)) {
        var buf: [4]u8 = undefined;
        const n = std.unicode.utf8Encode(cp, &buf) catch unreachable;
        try w.writeAll(buf[0..n]);
        return;
    }
    switch (cp) {
        7 => try w.writeAll("\\a"),
        8 => try w.writeAll("\\b"),
        0xc => try w.writeAll("\\f"),
        '\n' => try w.writeAll("\\n"),
        '\r' => try w.writeAll("\\r"),
        '\t' => try w.writeAll("\\t"),
        0xb => try w.writeAll("\\v"),
        else => if (cp < ' ' or cp == 0x7f) {
            try w.print("\\x{x:0>2}", .{cp});
        } else if (cp < 0x10000) {
            try w.print("\\u{x:0>4}", .{cp});
        } else {
            try w.print("\\U{x:0>8}", .{cp});
        },
    }
}

/// strconv.Quote.
pub fn writeQuoted(w: *Io.Writer, s: []const u8) Io.Writer.Error!void {
    try w.writeByte('"');
    var i: usize = 0;
    while (i < s.len) {
        const n = std.unicode.utf8ByteSequenceLength(s[i]) catch 0;
        const cp: ?u21 = if (n != 0 and i + n <= s.len) std.unicode.utf8Decode(s[i..][0..n]) catch null else null;
        if (cp) |c| {
            try writeEscapedRune(w, c, '"');
            i += n;
        } else {
            // An invalid byte is spelled as \xNN, like RuneError of width 1.
            try w.print("\\x{x:0>2}", .{s[i]});
            i += 1;
        }
    }
    try w.writeByte('"');
}

/// strconv.QuoteRune of a byte value, which is what `%q` prints for a Go byte.
pub fn writeQuotedByte(w: *Io.Writer, b: u8) Io.Writer.Error!void {
    try w.writeByte('\'');
    try writeEscapedRune(w, b, '\'');
    try w.writeByte('\'');
}

/// strconv.Quote into the arena.
pub fn quote(arena: Allocator, s: []const u8) Allocator.Error![]u8 {
    var aw: Io.Writer.Allocating = .init(arena);
    writeQuoted(&aw.writer, s) catch return error.OutOfMemory;
    return aw.written();
}

fn isSpace(cp: u21) bool {
    return switch (cp) {
        '\t', '\n', 0xb, 0xc, '\r', ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000 => true,
        0x2000...0x200a => true,
        else => false,
    };
}

/// strings.TrimSpace(s) == "".
pub fn isBlank(s: []const u8) bool {
    var it = std.unicode.Utf8View.init(s) catch return false;
    var cps = it.iterator();
    while (cps.nextCodepoint()) |cp| if (!isSpace(cp)) return false;
    return true;
}

/// filepath.IsAbs and filepath.Clean(p) == p for a Unix path: the path starts
/// with '/', has no empty, "." or ".." component and no trailing separator.
pub fn isAbsolute(p: []const u8) bool {
    return p.len > 0 and p[0] == '/';
}

pub fn isClean(p: []const u8) bool {
    if (p.len == 0) return false;
    if (std.mem.eql(u8, p, "/")) return true;
    if (p[p.len - 1] == '/') return false;
    var it = std.mem.splitScalar(u8, p, '/');
    var first = true;
    while (it.next()) |comp| {
        defer first = false;
        // An absolute path starts with an empty component before the first '/'.
        if (first and p[0] == '/') continue;
        if (comp.len == 0 or std.mem.eql(u8, comp, ".") or std.mem.eql(u8, comp, "..")) return false;
    }
    return true;
}

/// `parent` strictly contains `child`; both are clean absolute paths.
pub fn isWithin(parent: []const u8, child: []const u8) bool {
    if (std.mem.eql(u8, parent, child)) return false;
    if (std.mem.eql(u8, parent, "/")) return true;
    return child.len > parent.len and std.mem.startsWith(u8, child, parent) and child[parent.len] == '/';
}

const testing = std.testing;

test "quote spells strings like strconv.Quote" {
    const cases = [_][2][]const u8{
        .{ "plain", "\"plain\"" },
        .{ "a\"b\\c\nd\te", "\"a\\\"b\\\\c\\nd\\te\"" },
        .{ "\x00\x01\x7f", "\"\\x00\\x01\\x7f\"" },
        .{ "\x07\x08\x0b\x0c\r", "\"\\a\\b\\v\\f\\r\"" },
        .{ "caf\xc3\xa9 \xf0\x9f\x98\x80", "\"caf\xc3\xa9 \xf0\x9f\x98\x80\"" },
        .{ "\xc2\xa0|\xc2\xad|\xc2\x85", "\"\\u00a0|\\u00ad|\\u0085\"" },
        .{ "\xe2\x80\x8b\xe2\x80\xa8\xef\xbb\xbf", "\"\\u200b\\u2028\\ufeff\"" },
        .{ "\xff\xc3", "\"\\xff\\xc3\"" },
        .{ "it's", "\"it's\"" },
    };
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    for (cases) |c| try testing.expectEqualStrings(c[1], try quote(arena.allocator(), c[0]));
}

test "a byte is quoted as a rune" {
    var buf: [32]u8 = undefined;
    const cases = [_]struct { b: u8, want: []const u8 }{
        .{ .b = ' ', .want = "' '" },
        .{ .b = '\'', .want = "'\\''" },
        .{ .b = '"', .want = "'\"'" },
        .{ .b = '\n', .want = "'\\n'" },
        .{ .b = 0, .want = "'\\x00'" },
        .{ .b = 0xc3, .want = "'\xc3\x83'" },
        .{ .b = 0xa0, .want = "'\\u00a0'" },
        .{ .b = 0x85, .want = "'\\u0085'" },
    };
    for (cases) |c| {
        var w: Io.Writer = .fixed(&buf);
        try writeQuotedByte(&w, c.b);
        try testing.expectEqualStrings(c.want, w.buffered());
    }
}

test "blank is Unicode white space only" {
    try testing.expect(isBlank(""));
    try testing.expect(isBlank(" \t\r\n"));
    try testing.expect(isBlank("\xc2\xa0\xe3\x80\x80\xe2\x80\x83"));
    try testing.expect(!isBlank("a"));
    try testing.expect(!isBlank(" \xe2\x80\x8b "));
    try testing.expect(!isBlank("\xff"));
}

test "clean absolute paths" {
    for ([_][]const u8{ "/", "/a", "/a/b", "/data/usenet-done", "/.a", "/a..b" }) |p| {
        try testing.expect(isAbsolute(p));
        try testing.expect(isClean(p));
    }
    for ([_][]const u8{ "", "./a", "/a/", "/a//b", "//a", "/a/./b", "/a/../b", "/..", "/a/.", "/." }) |p| {
        try testing.expect(!isClean(p));
    }
    try testing.expect(!isAbsolute(""));
    try testing.expect(!isAbsolute("data"));
}

test "within is strict and respects whole components" {
    try testing.expect(isWithin("/a", "/a/b"));
    try testing.expect(isWithin("/", "/a"));
    try testing.expect(!isWithin("/", "/"));
    try testing.expect(!isWithin("/a", "/a"));
    try testing.expect(!isWithin("/data/usenet", "/data/usenet-done"));
    try testing.expect(!isWithin("/a/b", "/a"));
}
