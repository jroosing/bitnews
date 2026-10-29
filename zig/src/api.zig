//! The own API's routes served by the skeleton (internal/api/routes.go,
//! handlers_misc.go) and the app-key rules (internal/auth/appkey.go).
//! Z2 adds the other routes to `register`.
const std = @import("std");
const builtin = @import("builtin");
const common = @import("http_common.zig");
const ex = @import("http_exchange.zig");
const respond = @import("respond.zig");

/// What GET /api/v1/version reports. The build passes the strings in; the Zig
/// version defaults to the compiler that built this binary, like Go's
/// runtime.Version().
pub const Version = struct {
    version: []const u8 = "dev",
    commit: []const u8 = "",
    zig_version: []const u8 = builtin.zig_version_string,
};

pub const Api = struct {
    version: Version,

    /// Registers the routes. `self` must outlive the router.
    pub fn register(self: *const Api, routes: *ex.Routes) !void {
        // The same limits as routes.go: small bodies everywhere.
        try routes.add("GET /api/v1/health", .{ .handler = health, .auth = .none });
        try routes.add("GET /api/v1/version", .{ .handler = versionHandler, .state = @constCast(self), .auth = .key });
    }
};

fn health(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try e.res.json(e.arena, .ok, .{ .status = "ok" });
}

fn versionHandler(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const self: *const Api = @ptrCast(@alignCast(state.?));
    // Keys in the order Go's map encoder writes them (sorted).
    try e.res.json(e.arena, .ok, .{
        .commit = self.version.commit,
        .version = self.version.version,
        .zig_version = self.version.zig_version,
    });
}

/// The key a request carries, from "Authorization: Bearer <key>" or
/// "X-Api-Key: <key>"; Bearer wins when both are present. null means the
/// request carries no key at all, valid or not. A header that is present but
/// holds nothing usable (a Basic scheme, an empty X-Api-Key) counts as absent;
/// "Bearer " with an empty key counts as present (and then fails the check).
///
/// Go trims Unicode white space around the key; ASCII white space is trimmed
/// here, which is the same for every key the daemon generates.
pub fn credential(authorization: ?[]const u8, x_api_key: ?[]const u8) ?[]const u8 {
    if (authorization) |h| {
        if (std.mem.findScalar(u8, h, ' ')) |sp| {
            if (std.ascii.eqlIgnoreCase(h[0..sp], "Bearer")) return std.mem.trim(u8, h[sp + 1 ..], &std.ascii.whitespace);
        }
    }
    if (x_api_key) |k| {
        const t = std.mem.trim(u8, k, &std.ascii.whitespace);
        if (t.len > 0) return t;
    }
    return null;
}

/// True when the request carries the current app key.
pub fn authorized(keys: common.KeyChecker, authorization: ?[]const u8, x_api_key: ?[]const u8) bool {
    const key = credential(authorization, x_api_key) orelse return false;
    return keys.ok(key);
}

const testing = std.testing;

test "credential: Bearer" {
    try testing.expectEqualStrings("k", credential("Bearer k", null).?);
    try testing.expectEqualStrings("k", credential("bearer   k  ", null).?);
    try testing.expectEqualStrings("k", credential("BEARER k", "other").?);
    try testing.expectEqualStrings("", credential("Bearer ", null).?);
}

test "credential: X-Api-Key and fall-through" {
    try testing.expectEqualStrings("k", credential(null, " k ").?);
    try testing.expectEqualStrings("k", credential("Basic abc", "k").?);
    try testing.expectEqualStrings("k", credential("Bearer", "k").?);
    try testing.expectEqual(null, credential("Basic abc", null));
    try testing.expectEqual(null, credential(null, "  "));
    try testing.expectEqual(null, credential(null, null));
    try testing.expectEqual(null, credential("", ""));
}

fn fuzzCredential(_: void, smith: *testing.Smith) !void {
    var a: [128]u8 = undefined;
    var b: [128]u8 = undefined;
    const al = smith.slice(&a);
    const bl = smith.slice(&b);
    const got = credential(a[0..al], b[0..bl]) orelse return;
    // A key never carries surrounding white space, and always comes out of
    // one of the two inputs.
    if (got.len > 0) {
        try testing.expect(!std.ascii.isWhitespace(got[0]));
        try testing.expect(!std.ascii.isWhitespace(got[got.len - 1]));
    }
    const base_a = @intFromPtr(&a);
    const base_b = @intFromPtr(&b);
    const p = @intFromPtr(got.ptr);
    try testing.expect((p >= base_a and p + got.len <= base_a + al) or (p >= base_b and p + got.len <= base_b + bl));
}

fn seed2(comptime x: []const u8, comptime y: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, x.len)) ++ x.* ++ std.mem.toBytes(@as(u32, y.len)) ++ y.*);
}

test "fuzz: key extraction from header values" {
    try testing.fuzz({}, fuzzCredential, .{ .corpus = &.{
        seed2("Bearer bitnews_abc", ""),
        seed2("bearer  \t k \r", "x"),
        seed2("Basic dXNlcjpwYXNz", " key "),
        seed2("Bearer", "k"),
        seed2("", ""),
    } });
}
