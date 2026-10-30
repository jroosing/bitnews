//! JSON in and out of the handlers, with the behaviour of internal/api
//! (respond.go): strict request bodies and the error responses that go with
//! the services' failures.
//!
//! Request types are plain structs and EVERY field needs a default: Go leaves
//! absent fields at their zero value. Use `Nullable(T)` for a field that
//! distinguishes absent from null (PATCH). Decoding is strict_json.zig's.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Exchange = @import("http_exchange.zig").Exchange;
const app_error = @import("app_error.zig");
const strict_json = @import("strict_json.zig");

pub const Nullable = strict_json.Nullable;

/// Every integer field of a request is strict (see strict_json.zig); `Int`
/// is the same decoder as a type, for code that names it.
pub const Int = strict_json.Int;

/// Whether a Content-Type header names application/json (parameters allowed,
/// the type is case-insensitive), like mime.ParseMediaType in Go.
pub fn isJsonContentType(header: ?[]const u8) bool {
    const h = header orelse return false;
    const end = std.mem.indexOfScalar(u8, h, ';') orelse h.len;
    return std.ascii.eqlIgnoreCase(std.mem.trim(u8, h[0..end], " \t"), "application/json");
}

pub fn ParseResult(comptime T: type) type {
    return union(enum) {
        ok: T,
        /// A message safe to show to the client; the code is always invalid_json.
        invalid: []const u8,
    };
}

/// Parses a request body strictly (strict_json.zig): one JSON value of the
/// expected shape, no unknown fields, nothing after it. Strings of the result
/// live in `arena`.
pub fn parseBody(comptime T: type, arena: Allocator, body: []const u8) Allocator.Error!ParseResult(T) {
    if (std.mem.trim(u8, body, " \t\r\n").len == 0) return .{ .invalid = "the request body is empty" };
    var diag: strict_json.Diag = .{};
    const value = strict_json.parse(T, arena, body, &diag) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.Invalid => return .{ .invalid = try std.fmt.allocPrint(arena, "invalid request body: {s}", .{diag.message}) },
    };
    return .{ .ok = value };
}

/// Reads the request body as T. When the request is not acceptable the error
/// response is already written and null is returned: the handler just
/// returns. A body above the route's limit propagates as PayloadTooLarge, which the
/// server answers with 413.
pub fn decode(comptime T: type, e: *Exchange) !?T {
    if (!isJsonContentType(e.header("Content-Type"))) {
        try e.res.fail(e.arena, .unsupported_media_type, "unsupported_media_type", "send the body as Content-Type: application/json");
        return null;
    }
    const body = try e.bodyAlloc();
    switch (try parseBody(T, e.arena, body)) {
        .ok => |v| return v,
        .invalid => |msg| {
            try e.res.fail(e.arena, .bad_request, "invalid_json", msg);
            return null;
        },
    }
}

/// Answers with the response that matches a service's error: `Rejected` with
/// the details in `f`, `NotFound` and `Conflict` with their generic
/// messages. Anything else is returned, and the server answers 500 and logs it.
pub fn fail(e: *Exchange, err: anyerror, f: *const app_error.Failure) !void {
    switch (err) {
        error.Rejected => try e.res.fail(e.arena, f.kind.status(), f.code, f.message),
        error.NotFound => try e.res.fail(e.arena, .not_found, "not_found", "not found"),
        error.Conflict => try e.res.fail(e.arena, .conflict, "conflict", "the request conflicts with the current state"),
        else => return err,
    }
}

const testing = std.testing;

const Req = struct {
    name: []const u8 = "",
    n: ?Int = null,
    dir: Nullable([]const u8) = .{},
    paused: bool = false,
};

fn parse(body: []const u8) !ParseResult(Req) {
    // The parse result lives in the arena the caller frees; tests leak-check
    // through an arena that is freed at the end.
    return parseBody(Req, arenaForTest(), body);
}

var test_arena: std.heap.ArenaAllocator = undefined;
fn arenaForTest() Allocator {
    return test_arena.allocator();
}

test "a body is parsed strictly" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();

    const ok = try parse("{\"name\":\"tv\",\"paused\":true}");
    try testing.expectEqualStrings("tv", ok.ok.name);
    try testing.expect(ok.ok.paused);

    for ([_][]const u8{
        "", "   ", "{\"nam\":1}", "{\"name\":5}", "{\"name\":\"a\"} {}", "{\"name\":\"a\"}x", "[1]", "{", "null extra",
    }) |bad| {
        const r = try parse(bad);
        try testing.expect(r == .invalid);
    }
    try testing.expectEqualStrings("the request body is empty", (try parse("")).invalid);
    try testing.expectEqualStrings("invalid request body: json: unknown field \"id\"", (try parse("{\"id\":\"x\"}")).invalid);
}

test "a repeated key takes the last value, as Go's decoder does" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    const r = (try parse("{\"name\":\"first\",\"name\":\"second\"}")).ok;
    try testing.expectEqualStrings("second", r.name);
}

test "a Nullable field tells absent, null and a value apart" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();

    const absent = (try parse("{}")).ok;
    try testing.expect(!absent.dir.set);

    const null_ = (try parse("{\"dir\":null}")).ok;
    try testing.expect(null_.dir.set);
    try testing.expect(null_.dir.value == null);

    const value = (try parse("{\"dir\":\"/data\"}")).ok;
    try testing.expect(value.dir.set);
    try testing.expectEqualStrings("/data", value.dir.value.?);

    try testing.expect((try parse("{\"dir\":5}")) == .invalid);
}

test "an Int takes only plain integers" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();

    try testing.expectEqual(@as(i64, 563), (try parse("{\"n\":563}")).ok.n.?.value);
    try testing.expectEqual(@as(i64, -1), (try parse("{\"n\":-1}")).ok.n.?.value);
    try testing.expect((try parse("{\"n\":null}")).ok.n == null);
    for ([_][]const u8{ "{\"n\":\"563\"}", "{\"n\":563.0}", "{\"n\":5e2}", "{\"n\":true}", "{\"n\":99999999999999999999}", "{\"n\":[1]}" }) |bad| {
        try testing.expect((try parse(bad)) == .invalid);
    }
}

test "only application/json is accepted, parameters and case aside" {
    try testing.expect(isJsonContentType("application/json"));
    try testing.expect(isJsonContentType("Application/JSON; charset=utf-8"));
    try testing.expect(isJsonContentType(" application/json ;x=y"));
    try testing.expect(!isJsonContentType(null));
    try testing.expect(!isJsonContentType(""));
    try testing.expect(!isJsonContentType("text/json"));
    try testing.expect(!isJsonContentType("application/jsonp"));
    try testing.expect(!isJsonContentType("multipart/form-data; boundary=x"));
}

test "fuzz: parseBody never crashes or leaks on any bytes" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [256]u8 = undefined;
            const n = smith.slice(&buf);
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            _ = try parseBody(Req, arena.allocator(), buf[0..n]);
        }
    }.one, .{ .corpus = &.{ "\x0f\x00\x00\x00{\"name\":\"a\"}  ", "\x04\x00\x00\x00null" } });
}
