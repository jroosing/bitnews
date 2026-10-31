//! Tests of the name cleaning: the cases of internal/postproc/tidy
//! (clean_cases_test.go, clean_test.go, fuzz_test.go), a differential run
//! against Go's own regex chain (tidy_cases.zig), and the fuzz targets.
const std = @import("std");
const testing = std.testing;
const tidy = @import("tidy.zig");
const cases = @import("tidy_cases.zig");

/// The inputs of Clean.py with the names its regex chain gives
/// (clean_cases_test.go).
const clean_cases = [_][2][]const u8{
    .{ "Show.S01E01.1080p.WEB-DL-GROUP-4P", "Show.S01E01.1080p.WEB-DL-GROUP" },
    .{ "Show.S01E01-GROUP-4Planet", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-AlternativeToRequested", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-AlteZachen", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-AsRequested", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-AsRequested-xpost", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-BUYMORE", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-Chamele0n", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-GEROV", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-iNC0GNiTO", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-NZBGeek", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-Obfuscated", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-Obfuscation", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-postbot", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-Rakuv", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-Rakuvfdh0", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-RePACKPOST", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-Scrambled", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-WhiteRev", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-WRTEAM", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-CAPTCHA", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-Z0iDS3N", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-xpost", "Show.S01E01-GROUP" },
    .{ "Show.S01E01.1080p.mkv-xpost", "Show.S01E01.1080p" },
    .{ "Show.S01E01-GROUP [eztv]", "Show.S01E01-GROUP " },
    .{ "Show.S01E01-GROUP[eztv.re]", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP[eztv-re]", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP[eztv re]", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP[TGx]", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP[ettv]", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP[TGx]-xpost", "Show.S01E01-GROUP" },
    .{ "Show.S01E01-GROUP-NZBGEEK", "Show.S01E01-GROUP" },
    .{ "Movie.2020.1080p.BluRay.x264-D-Z0N3", "Movie.2020.1080p.BluRay.x264-D-Z0N3" },
    .{ "Movie.2020.1080p.BluRay.x264-GRP-AB12", "Movie.2020.1080p.BluRay.x264-GRP" },
    .{ "Movie.2020.1080p.BluRay.x264-D-Z0N3-AB12", "Movie.2020.1080p.BluRay.x264-D-Z0N3" },
    .{ "NoDashesAtAll", "NoDashesAtAll" },
    .{ "Name.With.Dots-GROUP", "Name.With.Dots-GROUP" },
    .{ "Show.S01E01-GROUP-NZBGeek-xpost", "Show.S01E01-GROUP-NZBGeek" },
    .{ "Show.S01E01.720p.HDTV.x264-GROUP-BUYMORE-AB12", "Show.S01E01.720p.HDTV.x264-GROUP-BUYMORE" },
    .{ "-NZBGeek", "-NZBGeek" }, // the script would print an empty name; a Download needs one
    .{ "", "" },
    .{ "Title-With-Many-Dashes-in-name", "Title-With-Many-Dashes-in" },
    .{ "Show.S01E01.1080p-GRP-ABCD-xpost", "Show.S01E01.1080p-GRP" },
    .{ "\xc3\x9cber.Show.S01E01-GR\xc3\x9cP-NZBGeek", "\xc3\x9cber.Show.S01E01-GR\xc3\x9cP" },
    .{ "Show S01E01 -GROUP- ABCD", "Show S01E01 -GROUP- ABCD" },
    .{ "A-B-CDEF", "A-B" },
    .{ "Release.Name-GROUP-ABCDE", "Release.Name-GROUP-ABCDE" },
    .{ "x-ab-cd-ef-gh", "x-ab-cd-ef-gh" },
};

test "Clean gives what Clean.py gives" {
    for (clean_cases) |c| try testing.expectEqualStrings(c[1], tidy.clean(c[0]));
}

test "Clean keeps the original when nothing is left" {
    try testing.expectEqualStrings("-NZBGeek", tidy.clean("-NZBGeek"));
    try testing.expectEqualStrings("", tidy.clean(""));
}

test "Clean agrees with Go's regex chain on a generated corpus" {
    try testing.expect(cases.clean_cases.len > 1000);
    for (cases.clean_cases, 0..) |c, n| {
        testing.expectEqualStrings(c.want, tidy.clean(c.input)) catch |err| {
            std.debug.print("case {d} {f}\n", .{ n, std.zig.fmtString(c.input) });
            return err;
        };
    }
}

test "Clean knows the Unicode case folds RE2 applies to (?i)" {
    // U+212A (Kelvin) folds to k and U+017F (long s) to s, as in Go's regexp.
    try testing.expectEqualStrings("Show-GROUP", tidy.clean("Show-GROUP-NZBGee\u{212a}"));
    try testing.expectEqualStrings("Show-GROUP-NZBGee\u{212a}", tidy.clean("Show-GROUP-NZBGee\u{212a}-xpo\u{17f}t")); // the chain is in order
    try testing.expectEqualStrings("Show-GROUP", tidy.clean("Show-GROUP-po\u{17f}tbot"));
    try testing.expectEqualStrings("Show-GROUP", tidy.clean("Show-GROUP-Ra\u{212a}uvABC"));
}

test "Clean does not cut inside a character or on a newline" {
    // The group tail is four characters, not four bytes.
    try testing.expectEqualStrings("a-b", tidy.clean("a-b-\xc3\xa9\xc3\xa9\xc3\xa9\xc3\xa9"));
    try testing.expectEqualStrings("a-b-\xc3\xa9\xc3\xa9\xc3\xa9\xc3\xa9\xc3\xa9", tidy.clean("a-b-\xc3\xa9\xc3\xa9\xc3\xa9\xc3\xa9\xc3\xa9"));
    // An invalid byte counts as one character.
    try testing.expectEqualStrings("a-b", tidy.clean("a-b-\xff\xfe\xfd\xfc"));
    // '.' does not match a newline.
    try testing.expectEqualStrings("a-b-AB\nD", tidy.clean("a-b-AB\nD"));
}

/// A name never becomes empty, and never longer: the cleaner is a prefix.
fn cleanProperties(_: void, smith: *testing.Smith) !void {
    var buf: [512]u8 = undefined;
    const n = smith.slice(&buf);
    const name = buf[0..n];
    const got = tidy.clean(name);
    if (name.len != 0 and got.len == 0) return error.EmptyResult;
    if (got.len > name.len) return error.Longer;
    if (!std.mem.startsWith(u8, name, got)) return error.NotAPrefix;
    // Cleaning twice is not required to be a fixed point (Clean.py's chain is
    // not), but it must not grow either.
    const again = tidy.clean(got);
    if (again.len > got.len) return error.Longer;
}

// The first fuzz test of the group is the one `zig build test-tidy --fuzz`
// runs.
test "fuzz: Clean never fails and never returns an empty name for a non-empty one" {
    const seeds = comptime blk: {
        var s: [clean_cases.len + 3][]const u8 = undefined;
        for (clean_cases, 0..) |c, i| s[i] = smithBytes(c[0]);
        s[clean_cases.len] = smithBytes("\xff\xfe-NZBGeek");
        s[clean_cases.len + 1] = smithBytes("a-b-\xff\xfe\xfd\xfc");
        s[clean_cases.len + 2] = smithBytes("x\xe2\x84\xaa-Ra\xe2\x84\xaauv");
        break :blk s;
    };
    try testing.fuzz({}, cleanProperties, .{ .corpus = &seeds });
}

/// The Smith stream that gives `s` back from `slice`: a u32 length and the
/// bytes (see strict_json.zig).
fn smithBytes(comptime s: []const u8) []const u8 {
    const len: [4]u8 = @bitCast(@as(u32, s.len));
    const out = len ++ s[0..s.len].*;
    return &out;
}

fn videoExtProperty(_: void, smith: *testing.Smith) !void {
    var buf: [512]u8 = undefined;
    const n = smith.slice(&buf);
    const ext = tidy.videoExt(buf[0..n]);
    const known = [_][]const u8{ "", ".mkv", ".mp4", ".mov", ".avi", ".wmv", ".mpg", ".ts", ".m2ts" };
    for (known) |k| if (std.mem.eql(u8, k, ext)) return;
    return error.UnknownExtension;
}

test "fuzz: VideoExt only returns a known extension" {
    const seeds = [_][]const u8{
        smithBytes("\x1a\x45\xdf\xa3"),
        smithBytes("\x00\x00\x00\x20ftypqt  "),
        smithBytes("RIFF\x00\x00\x00\x00AVI "),
        smithBytes("\x47"),
        smithBytes(""),
    };
    try testing.fuzz({}, videoExtProperty, .{ .corpus = &seeds });
}

test "VideoExt agrees with Go on the generated heads" {
    for (cases.video_cases, 0..) |c, n| {
        testing.expectEqualStrings(c.want, tidy.videoExt(c.head)) catch |err| {
            std.debug.print("case {d}\n", .{n});
            return err;
        };
    }
}

test "nameCleaner is the add service's NameCleaner and copies into the arena" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const nc = tidy.nameCleaner();
    const got = try nc.clean_fn(nc.ctx, arena.allocator(), "Show.S01E01-GROUP-NZBGeek");
    try testing.expectEqualStrings("Show.S01E01-GROUP", got);
    const same = try nc.clean_fn(nc.ctx, arena.allocator(), "-NZBGeek");
    try testing.expectEqualStrings("-NZBGeek", same);
}
