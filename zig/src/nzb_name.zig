//! The file name of a File, guessed from its subject, and its kind
//! (internal/usenet/nzb/name.go). Only the NAME is looked at: a PAR2 file is
//! never opened (par2 does the verifying and repairing, ADR-0007), so the
//! block count of a recovery volume is read from "name.volNN+MM.par2".
//! Every result is a slice of the argument: nothing is allocated.
const std = @import("std");
const Kind = @import("nzb_types.zig").Kind;

/// Go's strings.TrimSpace: leading and trailing Unicode white space.
pub fn trimSpace(s: []const u8) []const u8 {
    var start: usize = 0;
    while (start < s.len) {
        const n = spaceLen(s, start);
        if (n == 0) break;
        start += n;
    }
    var end = start;
    var i = start;
    while (i < s.len) {
        const n = spaceLen(s, i);
        if (n == 0) {
            i += runeLen(s, i);
            end = i;
        } else i += n;
    }
    return s[start..end];
}

/// The length of the white space character at `s[i]` (Go's unicode.IsSpace),
/// or 0 when it is not one.
fn spaceLen(s: []const u8, i: usize) usize {
    const b = s[i];
    switch (b) {
        '\t', '\n', 0x0B, 0x0C, '\r', ' ' => return 1,
        0xC2 => if (i + 1 < s.len and (s[i + 1] == 0x85 or s[i + 1] == 0xA0)) return 2,
        0xE1 => if (std.mem.startsWith(u8, s[i..], "\u{1680}")) return 3,
        0xE2 => {
            const r = s[i..];
            if (r.len >= 3 and r[1] == 0x80 and ((r[2] >= 0x80 and r[2] <= 0x8A) or r[2] == 0xA8 or r[2] == 0xA9 or r[2] == 0xAF)) return 3;
            if (std.mem.startsWith(u8, r, "\u{205F}")) return 3;
        },
        0xE3 => if (std.mem.startsWith(u8, s[i..], "\u{3000}")) return 3,
        else => {},
    }
    return 0;
}

/// The length of the character at `s[i]`; 1 for a byte that starts none.
fn runeLen(s: []const u8, i: usize) usize {
    const n = std.unicode.utf8ByteSequenceLength(s[i]) catch return 1;
    if (i + n > s.len) return 1;
    _ = std.unicode.utf8Decode(s[i..][0..n]) catch return 1;
    return n;
}

/// A provisional file name from a subject line: the first non-empty
/// double-quoted substring, else the last whitespace-separated token that
/// looks like a file name, else the trimmed subject.
pub fn guessName(subject: []const u8) []const u8 {
    var rest = subject;
    while (std.mem.indexOfScalar(u8, rest, '"')) |open| {
        const closing = std.mem.indexOfScalar(u8, rest[open + 1 ..], '"') orelse break;
        const quoted = rest[open + 1 ..][0..closing];
        if (quoted.len > 0) return quoted;
        rest = rest[open + 1 + closing + 1 ..];
    }
    // The last token that fits wins, which one forward pass finds.
    var found: ?[]const u8 = null;
    var i: usize = 0;
    while (i < subject.len) {
        const n = spaceLen(subject, i);
        if (n != 0) {
            i += n;
            continue;
        }
        const start = i;
        while (i < subject.len and spaceLen(subject, i) == 0) i += 1;
        const tok = std.mem.trim(u8, subject[start..i], "[](){}<>'");
        if (looksLikeFileName(tok)) found = tok;
    }
    return found orelse trimSpace(subject);
}

/// What counts as a file extension: this many ASCII letters or digits.
const max_ext_len = 10;

/// Whether `tok` is base.ext with ext 1..10 ASCII letters or digits and a
/// letter in base (so a size such as "1.2" is not taken for a name).
fn looksLikeFileName(tok: []const u8) bool {
    const dot = std.mem.lastIndexOfScalar(u8, tok, '.') orelse return false;
    if (dot == 0) return false;
    const base = tok[0..dot];
    const ext = tok[dot + 1 ..];
    if (ext.len == 0 or ext.len > max_ext_len) return false;
    for (ext) |c| if (!std.ascii.isAlphanumeric(c)) return false;
    for (base) |c| if (std.ascii.isAlphabetic(c)) return true;
    return false;
}

pub const Classified = struct { kind: Kind, recovery_blocks: u32 };

/// The kind of a file name and, for a PAR2 volume, the recovery block count
/// MM of "name.volNN+MM.par2" (0 when it does not fit a u32).
pub fn classify(name: []const u8) Classified {
    const ext = ".par2";
    if (name.len < ext.len or !std.ascii.eqlIgnoreCase(name[name.len - ext.len ..], ext)) {
        return .{ .kind = .data, .recovery_blocks = 0 };
    }
    const head = name[0 .. name.len - ext.len];
    // head must end in ".vol" digits+ "+" digits+ (case-insensitive).
    var mm_start = head.len;
    while (mm_start > 0 and std.ascii.isDigit(head[mm_start - 1])) mm_start -= 1;
    const mm = head[mm_start..];
    if (mm.len > 0 and mm_start > 0 and head[mm_start - 1] == '+') {
        const before = head[0 .. mm_start - 1];
        var nn_start = before.len;
        while (nn_start > 0 and std.ascii.isDigit(before[nn_start - 1])) nn_start -= 1;
        const vol = ".vol";
        if (nn_start < before.len and nn_start >= vol.len and
            std.ascii.eqlIgnoreCase(before[nn_start - vol.len .. nn_start], vol))
        {
            const blocks = std.fmt.parseInt(u32, mm, 10) catch 0;
            return .{ .kind = .par2_volume, .recovery_blocks = blocks };
        }
    }
    return .{ .kind = .par2_index, .recovery_blocks = 0 };
}

const testing = std.testing;

test "the name comes from the first quoted substring" {
    const cases = [_][2][]const u8{
        .{ "[01/10] - \"Some.Show.S01E01.part01.rar\" yEnc (1/50)", "Some.Show.S01E01.part01.rar" },
        .{ "\"first.nfo\" and \"second.rar\"", "first.nfo" },
        .{ "[3/7] - \"a8f3e9c2b1d4e5f6a7b8c9d0e1f2a3b4\" yEnc (1/1)", "a8f3e9c2b1d4e5f6a7b8c9d0e1f2a3b4" },
        .{ "Release [1/2] - \"../../etc/passwd\" yEnc (1/1)", "../../etc/passwd" },
        .{ "Some Poster - \"with spaces inside.mkv\" yEnc (01/99) 1.4 GB", "with spaces inside.mkv" },
    };
    for (cases) |c| try testing.expectEqualStrings(c[1], guessName(c[0]));
}

test "the name falls back to the trimmed subject" {
    const cases = [_][2][]const u8{
        .{ "  a8f3e9c2b1d4e5f6a7b8c9d0e1f2a3b4  ", "a8f3e9c2b1d4e5f6a7b8c9d0e1f2a3b4" },
        .{ "unterminated \"quote", "unterminated \"quote" },
        .{ "empty \"\" quotes", "empty \"\" quotes" },
    };
    for (cases) |c| try testing.expectEqualStrings(c[1], guessName(c[0]));
}

test "an unquoted subject gives the last token that looks like a file name" {
    const cases = [_][2][]const u8{
        .{ "Some.Show.S01E01.720p.mkv yEnc (1/50)", "Some.Show.S01E01.720p.mkv" },
        .{ "[1/3] - movie.part01.rar - 1.2 GB yEnc (1/5)", "movie.part01.rar" },
        .{ "(movie.nfo) yEnc (1/1)", "movie.nfo" },
        .{ "[archive.7z.001] yEnc (1/1)", "archive.7z.001" },
        .{ "first.txt then second.r00 yEnc (1/1)", "second.r00" },
        .{ "Some.Release.Name [01/24] - Some.Release.Name.vol00+01.par2 yEnc (1/1)", "Some.Release.Name.vol00+01.par2" },
    };
    for (cases) |c| try testing.expectEqualStrings(c[1], guessName(c[0]));
}

test "Unicode white space separates tokens and is trimmed" {
    try testing.expectEqualStrings("b.txt", guessName("a.txt\u{00A0}b.txt\u{3000}1.2"));
    try testing.expectEqualStrings("x y", trimSpace("\u{2003} x y\u{85}\t"));
    try testing.expectEqualStrings("", trimSpace(" \u{2028}\n"));
    try testing.expectEqualStrings("a\u{2192}", trimSpace("a\u{2192} "));
}

test "files are classified by name" {
    const Case = struct { name: []const u8, kind: Kind, blocks: u32 };
    const cases = [_]Case{
        .{ .name = "movie.part01.rar", .kind = .data, .blocks = 0 },
        .{ .name = "movie.nfo", .kind = .data, .blocks = 0 },
        .{ .name = "movie.par2", .kind = .par2_index, .blocks = 0 },
        .{ .name = "MOVIE.PAR2", .kind = .par2_index, .blocks = 0 },
        .{ .name = "movie.vol00+01.par2", .kind = .par2_volume, .blocks = 1 },
        .{ .name = "movie.vol03+04.par2", .kind = .par2_volume, .blocks = 4 },
        .{ .name = "movie.vol127+128.par2", .kind = .par2_volume, .blocks = 128 },
        .{ .name = "Movie.VOL07+08.PAR2", .kind = .par2_volume, .blocks = 8 },
        .{ .name = "movie.vol00+99999999999999999999.par2", .kind = .par2_volume, .blocks = 0 },
        .{ .name = "movie.vol00+4294967296.par2", .kind = .par2_volume, .blocks = 0 },
        .{ .name = "movie.vol.par2", .kind = .par2_index, .blocks = 0 },
        .{ .name = "movie.vol1+.par2", .kind = .par2_index, .blocks = 0 },
        .{ .name = "movie.vol+1.par2", .kind = .par2_index, .blocks = 0 },
        .{ .name = "movievol1+2.par2", .kind = .par2_index, .blocks = 0 },
        .{ .name = ".par2", .kind = .par2_index, .blocks = 0 },
        .{ .name = "movie.par2.rar", .kind = .data, .blocks = 0 },
        .{ .name = "a8f3e9c2b1d4e5f6", .kind = .data, .blocks = 0 },
        .{ .name = "", .kind = .data, .blocks = 0 },
    };
    for (cases) |c| {
        const got = classify(c.name);
        errdefer std.debug.print("name {s}\n", .{c.name});
        try testing.expectEqual(c.kind, got.kind);
        try testing.expectEqual(c.blocks, got.recovery_blocks);
    }
}

test "fuzz: names of any bytes" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [256]u8 = undefined;
            const n = smith.slice(&buf);
            const name = guessName(buf[0..n]);
            // A slice of the subject, never longer than it.
            try testing.expect(name.len <= n);
            _ = classify(name);
            _ = trimSpace(buf[0..n]);
        }
    }.one, .{});
}
