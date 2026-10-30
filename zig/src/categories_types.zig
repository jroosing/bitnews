//! The Category (internal/core/category.go): an optional label on a Download
//! that decides where its Ready output lands and which Media manager owns it.
//! Categories are shared across Usenet and torrents; each protocol has its own
//! folder override, as each has its own complete root.
//!
//! Strings of a Category belong to whoever produced it (the arena of the
//! request or query); copy them to keep them.
const std = @import("std");
const dl = @import("download.zig");

pub const max_name_len = 64;

pub const Category = struct {
    id: []const u8,
    /// Unique and matches [a-z0-9._-]{1,64}, except "." and "..".
    name: []const u8,
    /// When set, the absolute, clean directory the Ready output of this
    /// Category's Usenet (or torrent) Downloads lands in. Empty means
    /// <complete root of the protocol>/<name>.
    usenet_dir: []const u8 = "",
    torrent_dir: []const u8 = "",

    /// The folder override for Downloads of the source kind, or "" for the
    /// default folder.
    pub fn dirOverride(c: Category, kind: dl.SourceKind) []const u8 {
        return switch (kind) {
            .torrent => c.torrent_dir,
            .nzb => c.usenet_dir,
        };
    }
};

pub const ValidationError = error{
    /// The name is outside [a-z0-9._-]{1,64}, or is "." or "..".
    InvalidName,
    /// A folder override is not an absolute, clean path.
    InvalidDirOverride,
};

/// Whether the Category may be stored. The name is checked first, as in Go.
pub fn validate(c: Category) ValidationError!void {
    if (!validName(c.name)) return error.InvalidName;
    if (c.usenet_dir.len > 0 and !isCleanAbsolute(c.usenet_dir)) return error.InvalidDirOverride;
    if (c.torrent_dir.len > 0 and !isCleanAbsolute(c.torrent_dir)) return error.InvalidDirOverride;
}

pub fn validName(name: []const u8) bool {
    // "." and ".." match the pattern but would name a directory outside
    // <complete root>/<name>.
    if (name.len < 1 or name.len > max_name_len) return false;
    if (std.mem.eql(u8, name, ".") or std.mem.eql(u8, name, "..")) return false;
    for (name) |b| switch (b) {
        'a'...'z', '0'...'9', '.', '_', '-' => {},
        else => return false,
    };
    return true;
}

/// filepath.IsAbs(p) and filepath.Clean(p) == p on a POSIX system: a leading
/// slash and no empty, "." or ".." segment (so no doubled or trailing slash).
pub fn isCleanAbsolute(path: []const u8) bool {
    if (path.len == 0 or path[0] != '/') return false;
    if (path.len == 1) return true;
    var it = std.mem.splitScalar(u8, path[1..], '/');
    while (it.next()) |seg| {
        if (seg.len == 0 or std.mem.eql(u8, seg, ".") or std.mem.eql(u8, seg, "..")) return false;
    }
    return true;
}

/// The Category named `name`, ignoring (ASCII) case. Stored names are lower
/// case ASCII, so a non-ASCII `name` never matches.
pub fn findIgnoreCase(cats: []const Category, name: []const u8) ?Category {
    for (cats) |c| if (std.ascii.eqlIgnoreCase(c.name, name)) return c;
    return null;
}

const testing = std.testing;

test "names of the allowed alphabet are valid" {
    const long: [64]u8 = @splat('x');
    for ([_][]const u8{ "tv", "movies-4k", "tv.anime", "a_b", "0", &long, "..a", "a.." }) |name| {
        try validate(.{ .id = "x", .name = name });
    }
}

test "other names are refused" {
    const long: [65]u8 = @splat('x');
    for ([_][]const u8{ "", "TV", "tv shows", "tv/anime", "../tv", "s\xc3\xa9rie", &long, ".", "..", "a\x00b", "tv\n" }) |name| {
        try testing.expectError(error.InvalidName, validate(.{ .id = "x", .name = name }));
    }
}

test "dir overrides must be absolute and clean" {
    for ([_][]const u8{ "media/tv", "./tv", "~/tv", "/data/../etc", "/data/./tv", "/data//tv", "/data/tv/", "/..", "/data/..", "//" }) |dir| {
        try testing.expectError(error.InvalidDirOverride, validate(.{ .id = "x", .name = "tv", .usenet_dir = dir }));
        try testing.expectError(error.InvalidDirOverride, validate(.{ .id = "x", .name = "tv", .torrent_dir = dir }));
    }
    try validate(.{ .id = "x", .name = "tv", .usenet_dir = "/data/usenet/complete/series", .torrent_dir = "/data/torrents/series" });
    try validate(.{ .id = "x", .name = "tv", .usenet_dir = "/", .torrent_dir = "/data/..a/b.c" });
}

test "a bad name is reported before a bad dir" {
    try testing.expectError(error.InvalidName, validate(.{ .id = "x", .name = "TV", .usenet_dir = "rel" }));
}

test "the dir override is per source kind" {
    const c: Category = .{ .id = "x", .name = "movies", .usenet_dir = "/data/usenet/movies", .torrent_dir = "/data/torrents/movies" };
    try testing.expectEqualStrings("/data/usenet/movies", c.dirOverride(.nzb));
    try testing.expectEqualStrings("/data/torrents/movies", c.dirOverride(.torrent));
    const tv: Category = .{ .id = "x", .name = "tv", .usenet_dir = "/data/tv" };
    try testing.expectEqualStrings("", tv.dirOverride(.torrent));
}

test "findIgnoreCase finds a name whatever its case" {
    const cats = [_]Category{ .{ .id = "1", .name = "movies" }, .{ .id = "2", .name = "tv" } };
    try testing.expectEqualStrings("2", findIgnoreCase(&cats, "TV").?.id);
    try testing.expectEqualStrings("1", findIgnoreCase(&cats, "movies").?.id);
    try testing.expect(findIgnoreCase(&cats, "music") == null);
    try testing.expect(findIgnoreCase(&cats, "") == null);
    try testing.expect(findIgnoreCase(&.{}, "tv") == null);
}

fn fuzzValidate(_: void, smith: *testing.Smith) !void {
    var name_buf: [96]u8 = undefined;
    var dir_buf: [128]u8 = undefined;
    const name = name_buf[0..smith.slice(&name_buf)];
    const dir = dir_buf[0..smith.slice(&dir_buf)];

    if (validName(name)) {
        try testing.expect(name.len >= 1 and name.len <= max_name_len);
        try testing.expect(std.mem.findAny(u8, name, "/\\\x00 ") == null);
        try testing.expect(!std.mem.eql(u8, name, ".") and !std.mem.eql(u8, name, ".."));
    }
    if (isCleanAbsolute(dir)) {
        // What Go's Clean(dir) == dir means: normalizing changes nothing.
        const resolved = try std.fs.path.resolvePosix(testing.allocator, &.{dir});
        defer testing.allocator.free(resolved);
        try testing.expectEqualStrings(dir, resolved);
    }
}

fn seed2(comptime x: []const u8, comptime y: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, x.len)) ++ x.* ++ std.mem.toBytes(@as(u32, y.len)) ++ y.*);
}

test "fuzz: accepted names and dirs are what they claim to be" {
    try testing.fuzz({}, fuzzValidate, .{ .corpus = &.{
        seed2("tv", "/data/tv"),
        seed2("..", "/data/../x"),
        seed2("a b", "/data//x/"),
        seed2("", "/"),
        seed2("x\x00", "/a/./b"),
    } });
}
