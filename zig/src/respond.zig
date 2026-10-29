//! The response a handler builds, and the helpers for the API's JSON shapes
//! (internal/api/respond.go). A response is buffered in the request's arena
//! and written by the server once the handler returns, so a handler that fails
//! halfway can still turn into a clean error response.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Header = std.http.Header;

pub const Response = struct {
    status: std.http.Status = .ok,
    headers: std.ArrayList(Header) = .empty,
    body: []const u8 = "",

    /// Sets `name` to `value`, replacing an earlier value case-insensitively.
    /// Neither may contain CR or LF (response splitting); the server asserts it.
    pub fn setHeader(r: *Response, arena: Allocator, name: []const u8, value: []const u8) Allocator.Error!void {
        for (r.headers.items) |*h| {
            if (std.ascii.eqlIgnoreCase(h.name, name)) {
                h.value = value;
                return;
            }
        }
        try r.headers.append(arena, .{ .name = name, .value = value });
    }

    pub fn header(r: *const Response, name: []const u8) ?[]const u8 {
        for (r.headers.items) |h| {
            if (std.ascii.eqlIgnoreCase(h.name, name)) return h.value;
        }
        return null;
    }

    /// Sends `v` as JSON, like writeJSON in Go: never cached, because API
    /// responses hold user data and session state. The encoding ends in a
    /// newline, as json.Encoder's does.
    pub fn json(r: *Response, arena: Allocator, status: std.http.Status, v: anytype) Allocator.Error!void {
        var aw: std.Io.Writer.Allocating = .init(arena);
        std.json.Stringify.value(v, .{}, &aw.writer) catch return error.OutOfMemory;
        aw.writer.writeByte('\n') catch return error.OutOfMemory;
        r.status = status;
        r.body = aw.written();
        try r.setHeader(arena, "Content-Type", "application/json");
        try r.setHeader(arena, "Cache-Control", "no-store");
    }

    /// The error shape of the whole API: {"error":{"code":..,"message":..}}.
    pub fn fail(r: *Response, arena: Allocator, status: std.http.Status, code: []const u8, message: []const u8) Allocator.Error!void {
        return r.json(arena, status, .{ .@"error" = .{ .code = code, .message = message } });
    }
};

/// Body size limits, as in respond.go. A route that needs more or less than
/// the default says so when it is registered.
pub const default_body_limit: u64 = 1 << 20;
pub const small_body_limit: u64 = 16 << 10;

/// RFC 9110 IMF-fixdate ("Sun, 06 Nov 1994 08:49:37 GMT"), which Go's server
/// adds as the Date header of every response.
pub fn httpDate(buf: *[29]u8, unix_seconds: i64) []const u8 {
    const secs: u64 = @intCast(@max(unix_seconds, 0));
    const es: std.time.epoch.EpochSeconds = .{ .secs = secs };
    const day = es.getEpochDay();
    const yd = day.calculateYearDay();
    const md = yd.calculateMonthDay();
    const ds = es.getDaySeconds();
    // 1970-01-01 was a Thursday.
    const weekday = [_][]const u8{ "Thu", "Fri", "Sat", "Sun", "Mon", "Tue", "Wed" };
    const months = [_][]const u8{ "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec" };
    return std.fmt.bufPrint(buf, "{s}, {d:0>2} {s} {d:0>4} {d:0>2}:{d:0>2}:{d:0>2} GMT", .{
        weekday[@intCast(day.day % 7)],
        md.day_index + 1,
        months[md.month.numeric() - 1],
        yd.year,
        ds.getHoursIntoDay(),
        ds.getMinutesIntoHour(),
        ds.getSecondsIntoMinute(),
    }) catch unreachable;
}

const testing = std.testing;

test "json sets the API headers and ends in a newline" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var r: Response = .{};
    try r.json(arena.allocator(), .ok, .{ .status = "ok" });
    try testing.expectEqual(.ok, r.status);
    try testing.expectEqualStrings("{\"status\":\"ok\"}\n", r.body);
    try testing.expectEqualStrings("application/json", r.header("content-type").?);
    try testing.expectEqualStrings("no-store", r.header("Cache-Control").?);
}

test "fail writes the error shape" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var r: Response = .{};
    try r.fail(arena.allocator(), .not_found, "not_found", "no such route");
    try testing.expectEqual(.not_found, r.status);
    try testing.expectEqualStrings("{\"error\":{\"code\":\"not_found\",\"message\":\"no such route\"}}\n", r.body);
}

test "json escapes what must be escaped" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var r: Response = .{};
    try r.fail(arena.allocator(), .bad_request, "x", "quote \" back \\ nl \n");
    try testing.expectEqualStrings("{\"error\":{\"code\":\"x\",\"message\":\"quote \\\" back \\\\ nl \\n\"}}\n", r.body);
}

test "setHeader replaces case-insensitively" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var r: Response = .{};
    try r.setHeader(arena.allocator(), "Allow", "GET");
    try r.setHeader(arena.allocator(), "allow", "POST");
    try testing.expectEqual(1, r.headers.items.len);
    try testing.expectEqualStrings("POST", r.header("ALLOW").?);
}

test "httpDate" {
    var buf: [29]u8 = undefined;
    try testing.expectEqualStrings("Sun, 06 Nov 1994 08:49:37 GMT", httpDate(&buf, 784111777));
    try testing.expectEqualStrings("Thu, 01 Jan 1970 00:00:00 GMT", httpDate(&buf, 0));
    try testing.expectEqualStrings("Sat, 29 Feb 2020 23:59:59 GMT", httpDate(&buf, 1583020799));
}
