//! What an access-log line may show of a request target. The Go server logs
//! redact.URL(RequestURI); this is a narrower port: it masks the same secret
//! parameter names and passkey path segments, and additionally masks any
//! parameter whose value is a whole http(s) URL (instead of looking inside
//! it), so a nested indexer key can never reach the log.
const std = @import("std");
const Allocator = std.mem.Allocator;

pub const mask = "REDACTED";

const secret_params = [_][]const u8{
    "apikey",   "api_key",      "key",     "token",   "password",     "pass",
    "nzbkey",   "r",            "passkey", "authkey", "torrent_pass", "rsskey",
    "apitoken", "access_token",
};

/// The request target as it may be logged. The qBittorrent adapter logs the
/// path only (its queries hold hashes and its bodies magnet links), the
/// SABnzbd adapter the path and a sanitised mode (its query holds the adapter
/// key and, for mode=addurl, another URL with a key of its own).
pub fn loggedTarget(arena: Allocator, path: []const u8, query: []const u8) Allocator.Error![]const u8 {
    if (std.mem.startsWith(u8, path, "/api/v2/")) return path;
    if (std.mem.eql(u8, path, "/sabnzbd/api") or std.mem.startsWith(u8, path, "/sabnzbd/api/")) {
        const mode = try sanitizedMode(arena, query);
        if (mode.len == 0) return path;
        return std.mem.concat(arena, u8, &.{ path, "?mode=", mode });
    }
    const p = try redactPath(arena, path);
    if (query.len == 0) return p;
    return std.mem.concat(arena, u8, &.{ p, "?", try redactQuery(arena, query) });
}

fn sanitizedMode(arena: Allocator, query: []const u8) Allocator.Error![]const u8 {
    var it = std.mem.splitAny(u8, query, "&;");
    while (it.next()) |part| {
        const eq = std.mem.findScalar(u8, part, '=') orelse continue;
        if (!std.mem.eql(u8, part[0..eq], "mode")) continue;
        const value = try queryUnescape(arena, part[eq + 1 ..]);
        var out: std.ArrayList(u8) = .empty;
        for (value) |c| {
            if (out.items.len == 32) break;
            if (std.ascii.isAlphanumeric(c) or c == '_') try out.append(arena, c);
        }
        return out.items;
    }
    return "";
}

/// A hex segment of 32 or more characters is masked, and so is a token-looking
/// segment (20 or more of [A-Za-z0-9_-]) next to an "announce" or "passkey"
/// segment: some trackers put the passkey in the announce URL.
fn redactPath(arena: Allocator, path: []const u8) Allocator.Error![]const u8 {
    var segs: std.ArrayList([]const u8) = .empty;
    var it = std.mem.splitScalar(u8, path, '/');
    while (it.next()) |s| try segs.append(arena, s);
    var changed = false;
    for (segs.items, 0..) |seg, i| {
        const hex = seg.len >= 32 and allBytes(seg, std.ascii.isHex);
        const token = seg.len >= 20 and allBytes(seg, isTokenChar) and
            (isMarker(segs.items, i, -1) or isMarker(segs.items, i, 1));
        if (hex or token) {
            segs.items[i] = mask;
            changed = true;
        }
    }
    if (!changed) return path;
    return std.mem.join(arena, "/", segs.items);
}

fn isMarker(segs: []const []const u8, i: usize, delta: isize) bool {
    const j = @as(isize, @intCast(i)) + delta;
    if (j < 0 or j >= segs.len) return false;
    const s = segs[@intCast(j)];
    return std.ascii.eqlIgnoreCase(s, "announce") or std.ascii.eqlIgnoreCase(s, "passkey");
}

fn allBytes(s: []const u8, f: *const fn (u8) bool) bool {
    for (s) |c| if (!f(c)) return false;
    return true;
}

fn isTokenChar(c: u8) bool {
    return std.ascii.isAlphanumeric(c) or c == '_' or c == '-';
}

fn redactQuery(arena: Allocator, query: []const u8) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var rest = query;
    while (rest.len > 0) {
        const end = std.mem.findAny(u8, rest, "&;") orelse rest.len;
        const part = rest[0..end];
        if (std.mem.findScalar(u8, part, '=')) |eq| {
            const name = part[0..eq];
            if (try isSecretName(arena, name) or try isURLValue(arena, part[eq + 1 ..])) {
                try out.appendSlice(arena, name);
                try out.append(arena, '=');
                try out.appendSlice(arena, mask);
            } else {
                try out.appendSlice(arena, part);
            }
        } else {
            try out.appendSlice(arena, part);
        }
        if (end < rest.len) {
            try out.append(arena, rest[end]);
            rest = rest[end + 1 ..];
        } else rest = rest[end..];
    }
    return out.items;
}

fn isSecretName(arena: Allocator, raw: []const u8) Allocator.Error!bool {
    const name = try queryUnescape(arena, raw);
    for (secret_params) |s| if (std.ascii.eqlIgnoreCase(name, s)) return true;
    return false;
}

fn isURLValue(arena: Allocator, raw: []const u8) Allocator.Error!bool {
    const v = try queryUnescape(arena, raw);
    return std.ascii.startsWithIgnoreCase(v, "http://") or std.ascii.startsWithIgnoreCase(v, "https://");
}

/// Query unescaping that never fails: a malformed escape stays as it is.
fn queryUnescape(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    if (std.mem.findAny(u8, s, "%+") == null) return s;
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    while (i < s.len) {
        const c = s[i];
        if (c == '%' and i + 2 < s.len and std.ascii.isHex(s[i + 1]) and std.ascii.isHex(s[i + 2])) {
            try out.append(arena, std.fmt.parseInt(u8, s[i + 1 .. i + 3], 16) catch unreachable);
            i += 3;
        } else {
            try out.append(arena, if (c == '+') ' ' else c);
            i += 1;
        }
    }
    return out.items;
}

const testing = std.testing;

fn expectTarget(want: []const u8, path: []const u8, query: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqualStrings(want, try loggedTarget(arena.allocator(), path, query));
}

test "secret parameters are masked and others kept" {
    try expectTarget("/api/v1/health?apikey=REDACTED&x=1", "/api/v1/health", "apikey=hunter2&x=1");
    try expectTarget("/a?ApiKey=REDACTED", "/a", "ApiKey=hunter2");
    try expectTarget("/a?%61pikey=REDACTED;x=1", "/a", "%61pikey=hunter2;x=1");
    try expectTarget("/a?flag&t=REDACTED", "/a", "flag&t=http%3A%2F%2Fidx%2Fapi%3Fkey%3Dsecret");
    try expectTarget("/a", "/a", "");
}

test "passkeys in the path are masked" {
    try expectTarget("/announce/REDACTED", "/announce/AAAAAAAAAAAAAAAAAAAA1234", "");
    try expectTarget("/x/REDACTED/y", "/x/0123456789abcdef0123456789abcdef/y", "");
    try expectTarget("/api/v1/downloads/short", "/api/v1/downloads/short", "");
}

test "the adapters log less" {
    try expectTarget("/api/v2/torrents/info", "/api/v2/torrents/info", "hashes=abc&sid=s3cr3t");
    try expectTarget("/sabnzbd/api?mode=queue", "/sabnzbd/api", "apikey=k&mode=queue");
    try expectTarget("/sabnzbd/api?mode=addurl", "/sabnzbd/api", "mode=add%2Furl!&name=http%3A%2F%2Fx%3Fkey%3D1");
    try expectTarget("/sabnzbd/api", "/sabnzbd/api", "apikey=k");
}

fn fuzzTarget(_: void, smith: *testing.Smith) !void {
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const input = buf[0..n];
    // The secret never survives in a secret parameter, wherever it is.
    const query = try std.mem.concat(arena.allocator(), u8, &.{ "apikey=s3cr3t&", input });
    const out = try loggedTarget(arena.allocator(), "/api/v1/x", query);
    try testing.expect(std.mem.startsWith(u8, out, "/api/v1/x?apikey=REDACTED"));
    _ = try loggedTarget(arena.allocator(), input, input);
}

test "fuzz: logged target" {
    try testing.fuzz({}, fuzzTarget, .{});
}
