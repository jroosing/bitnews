//! Naming a new Download (internal/app/add.go): which of the request's name,
//! the NZB's own name, the upload's file name and the first file's name wins,
//! the "{{password}}" suffix of a name, the display-name cleaning and the name
//! of the stored NZB file. All of it is pure and runs on untrusted text.
//!
//! Strings of results live in the arena the caller passes.
const std = @import("std");
const Allocator = std.mem.Allocator;
const mime = @import("add_mime.zig");
const fsutil = @import("fsutil.zig");
const names = @import("fsutil_name.zig");
const nzb_types = @import("nzb_types.zig");

/// The display name is capped at this many bytes (maxDisplayNameBytes).
pub const max_display_name_bytes = 255;

/// unicode.Cf (format characters: bidi controls, zero width characters, the
/// BOM), from Go 1.27's tables. The control characters (Cc) are tested by
/// range.
const format_ranges = [_][2]u21{
    .{ 0xad, 0xad },       .{ 0x600, 0x605 },     .{ 0x61c, 0x61c },     .{ 0x6dd, 0x6dd },
    .{ 0x70f, 0x70f },     .{ 0x890, 0x891 },     .{ 0x8e2, 0x8e2 },     .{ 0x180e, 0x180e },
    .{ 0x200b, 0x200f },   .{ 0x202a, 0x202e },   .{ 0x2060, 0x2064 },   .{ 0x2066, 0x206f },
    .{ 0xfeff, 0xfeff },   .{ 0xfff9, 0xfffb },   .{ 0x110bd, 0x110bd }, .{ 0x110cd, 0x110cd },
    .{ 0x13430, 0x1343f }, .{ 0x1bca0, 0x1bca3 }, .{ 0x1d173, 0x1d17a }, .{ 0xe0001, 0xe0001 },
    .{ 0xe0020, 0xe007f },
};

/// Control (Cc), format (Cf) and the line and paragraph separators: what a
/// display name loses.
fn isDropped(r: u21) bool {
    if (r < 0x20 or (r >= 0x7f and r <= 0x9f) or r == 0x2028 or r == 0x2029) return true;
    for (format_ranges) |range| {
        if (r >= range[0] and r <= range[1]) return true;
    }
    return false;
}

/// Makes a name safe to show: invalid UTF-8, control and format characters
/// (such as bidi overrides) and line separators are removed, surrounding space
/// is trimmed and the result is capped at 255 bytes on a rune boundary. It is a
/// display name only: files are named with `fsutil.safeName`.
pub fn cleanDisplayName(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    while (i < s.len) {
        const d = names.decodeRune(s, i);
        defer i += d.size;
        const cp = d.cp orelse continue; // an invalid byte goes
        if (isDropped(cp)) continue;
        try out.appendSlice(arena, s[i..][0..d.size]);
    }
    var t = mime.trimSpace(out.items);
    if (t.len > max_display_name_bytes) {
        t = t[0..max_display_name_bytes];
        // Cut a rune in half: drop the stub.
        while (!std.unicode.utf8ValidateSlice(t)) t = t[0 .. t.len - 1];
    }
    return mime.trimSpace(t);
}

/// A name and the password written after it as "{{password}}".
pub const Split = struct { name: []const u8, password: []const u8 };

/// Cuts a trailing "{{password}}" off `s`: Go's `^(.*)\{\{(.+)\}\}\s*$` on the
/// trimmed text, where `.` does not match a line break. The last "{{" that
/// leaves a non-empty password wins.
pub fn splitPasswordSuffix(s: []const u8) Split {
    const t = mime.trimSpace(s);
    var end = t.len;
    while (end > 0 and isRe2Space(t[end - 1])) end -= 1;
    const body = t[0..end];
    if (body.len < 5 or !std.mem.endsWith(u8, body, "}}")) return .{ .name = s, .password = "" };
    if (std.mem.findScalar(u8, body, '\n') != null) return .{ .name = s, .password = "" };
    // Candidates: "{{" at i, password body[i+2 .. len-2] of at least one byte.
    var i: usize = body.len - 5; // the last i that leaves a byte of password
    while (true) {
        if (std.mem.startsWith(u8, body[i..], "{{")) {
            return .{ .name = body[0..i], .password = body[i + 2 .. body.len - 2] };
        }
        if (i == 0) break;
        i -= 1;
    }
    return .{ .name = s, .password = "" };
}

/// RE2's `\s`: [\t\n\f\r ].
fn isRe2Space(c: u8) bool {
    return c == '\t' or c == '\n' or c == 0x0c or c == '\r' or c == ' ';
}

/// A trailing ".nzb" in any case is ignored.
pub fn trimNzbExt(s: []const u8) []const u8 {
    if (s.len >= 4 and std.ascii.eqlIgnoreCase(s[s.len - 4 ..], ".nzb")) return s[0 .. s.len - 4];
    return s;
}

pub const Resolved = struct {
    name: []const u8,
    /// The "{{password}}" suffix of the chosen name, else "".
    suffix_password: []const u8,
};

/// Picks the display name: the request's, the NZB's, the upload's file name,
/// the first file's guessed name, else "nzb-<id>". A "{{password}}" suffix is
/// cut off the chosen name and returned. The first candidate whose cleaned
/// name is not empty wins.
pub fn resolveName(arena: Allocator, request_name: []const u8, meta_name: []const u8, file_name: []const u8, files: []const nzb_types.File, id: []const u8) Allocator.Error!Resolved {
    const candidates = [_][]const u8{ request_name, meta_name, trimNzbExt(file_name), firstFileName(files) };
    for (candidates) |c| {
        const split = splitPasswordSuffix(c);
        const clean = try cleanDisplayName(arena, split.name);
        if (clean.len > 0) return .{ .name = clean, .suffix_password = split.password };
    }
    return .{ .name = try std.fmt.allocPrint(arena, "nzb-{s}", .{id}), .suffix_password = "" };
}

/// The first data file's name, else the first file's, else "".
fn firstFileName(files: []const nzb_types.File) []const u8 {
    for (files) |f| if (f.kind == .data) return f.name;
    if (files.len > 0) return files[0].name;
    return "";
}

/// The name of the stored NZB: the display name made safe plus ".nzb".
pub fn nzbFileName(arena: Allocator, display_name: []const u8) Allocator.Error![]const u8 {
    const with_ext = try std.mem.concat(arena, u8, &.{ display_name, ".nzb" });
    return fsutil.safeName(arena, with_ext) catch |err| switch (err) {
        error.InvalidName => "download.nzb",
        error.OutOfMemory => error.OutOfMemory,
    };
}

const testing = std.testing;

fn expectClean(want: []const u8, in: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqualStrings(want, try cleanDisplayName(arena.allocator(), in));
}

test "the display name loses control characters, bidi overrides and invalid bytes" {
    try expectClean("abc", "a\x00b\x07c\n");
    try expectClean("gnp.exe", "gnp.\u{202e}exe");
    try expectClean("ab", "a\xffb");
    try expectClean("padded", "  padded  ");
    try expectClean("padded", "\u{a0}\u{3000}padded\u{2003}");
    try expectClean("a b", "a b"); // an inner space stays
    try expectClean("ab", "a\u{2028}\u{2029}\u{feff}\u{200b}b");
    try expectClean("", "\x00\n \t");
    // Private use and unassigned characters are not control characters.
    try expectClean("\u{e000}", "\u{e000}");
}

test "the display name is capped at 255 bytes on a rune boundary" {
    const x300: [300]u8 = @splat('x');
    const x255: [255]u8 = @splat('x');
    try expectClean(&x255, &x300);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try a.alloc(u8, 400);
    for (0..200) |i| @memcpy(e[i * 2 ..][0..2], "é");
    const got = try cleanDisplayName(a, e);
    try testing.expectEqual(@as(usize, 254), got.len);
    try testing.expect(std.unicode.utf8ValidateSlice(got));
    // The cut leaves trailing space to trim.
    var spaced: [300]u8 = @splat('y');
    spaced[254] = ' ';
    try expectClean(spaced[0..254], &spaced);
}

test "the password suffix is cut off the name" {
    const cases = [_]struct { in: []const u8, name: []const u8, pw: []const u8 }{
        .{ .in = "Release{{secret}}", .name = "Release", .pw = "secret" },
        .{ .in = "  Release {{secret}}  ", .name = "Release ", .pw = "secret" },
        .{ .in = "a{{b}}{{c}}", .name = "a{{b}}", .pw = "c" },
        .{ .in = "a{{b}}{{}}", .name = "a", .pw = "b}}{{" },
        .{ .in = "{{only}}", .name = "", .pw = "only" },
        .{ .in = "Release{{}}", .name = "Release{{}}", .pw = "" },
        .{ .in = "Release{{x}", .name = "Release{{x}", .pw = "" },
        .{ .in = "no suffix", .name = "no suffix", .pw = "" },
        .{ .in = "a\n{{pw}}", .name = "a\n{{pw}}", .pw = "" },
        .{ .in = "a{{p\nw}}", .name = "a{{p\nw}}", .pw = "" },
        .{ .in = "", .name = "", .pw = "" },
    };
    for (cases) |c| {
        const got = splitPasswordSuffix(c.in);
        try testing.expectEqualStrings(c.name, got.name);
        try testing.expectEqualStrings(c.pw, got.password);
    }
}

test "the name comes from the request, the NZB, the file name or the first file" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const files = [_]nzb_types.File{
        .{ .name = "set.par2", .kind = .par2_index },
        .{ .name = "first.bin" },
    };
    const id = "aaaaaaaaaaaaaaaaaaaaaaaaaa";

    const r1 = try resolveName(a, "Chosen", "Meta", "file.nzb", &files, id);
    try testing.expectEqualStrings("Chosen", r1.name);
    const r2 = try resolveName(a, "  ", "Meta", "file.nzb", &files, id);
    try testing.expectEqualStrings("Meta", r2.name);
    const r3 = try resolveName(a, "", "", "Some.Show.S01E02.nzb", &files, id);
    try testing.expectEqualStrings("Some.Show.S01E02", r3.name);
    const r4 = try resolveName(a, "", "", "Loud.NZB", &files, id);
    try testing.expectEqualStrings("Loud", r4.name);
    const r5 = try resolveName(a, "", "", ".nzb", &files, id);
    try testing.expectEqualStrings("first.bin", r5.name);
    const r6 = try resolveName(a, "", "", "", &files, id);
    try testing.expectEqualStrings("first.bin", r6.name);
    const only_par = [_]nzb_types.File{.{ .name = "set.par2", .kind = .par2_index }};
    try testing.expectEqualStrings("set.par2", (try resolveName(a, "", "", "", &only_par, id)).name);
    try testing.expectEqualStrings("nzb-aaaaaaaaaaaaaaaaaaaaaaaaaa", (try resolveName(a, "", "", "", &.{}, id)).name);
    // A name that is only a password suffix falls through to the next one.
    const r7 = try resolveName(a, "{{pw}}", "Meta", "", &files, id);
    try testing.expectEqualStrings("Meta", r7.name);
    try testing.expectEqualStrings("", r7.suffix_password);
    const r8 = try resolveName(a, "Named {{s3cr3t}}", "", "", &files, id);
    try testing.expectEqualStrings("Named", r8.name);
    try testing.expectEqualStrings("s3cr3t", r8.suffix_password);
}

test "the stored file is the safe display name plus .nzb" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try testing.expectEqualStrings("My.Release.nzb", try nzbFileName(a, "My.Release"));
    try testing.expectEqualStrings("a_b.nzb", try nzbFileName(a, "a/b"));
    try testing.expectEqualStrings("a_b_.nzb", try nzbFileName(a, "a:b?"));
}

test "fuzz: cleaning and splitting never crash, and the result is valid and capped" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [600]u8 = undefined;
            const n = smith.slice(&buf);
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            const a = arena.allocator();
            const clean = try cleanDisplayName(a, buf[0..n]);
            try testing.expect(clean.len <= max_display_name_bytes);
            try testing.expect(std.unicode.utf8ValidateSlice(clean));
            const split = splitPasswordSuffix(buf[0..n]);
            try testing.expect(split.name.len + split.password.len <= n);
            _ = try nzbFileName(a, clean);
        }
    }.one, .{});
}
