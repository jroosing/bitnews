//! Strict checks on a request head (request line and header lines) before the
//! server trusts it. std.http parses leniently: it ignores a header whose name
//! has a space before the colon, accepts a bare LF inside a value and lets the
//! request line carry spaces. Those are the ingredients of request smuggling
//! when a proxy reads the same bytes differently, so the daemon refuses them.
const std = @import("std");

pub const Error = error{
    /// More header lines than the server allows (a 431).
    TooManyHeaders,
    /// A control character, a bare CR or LF, an obsolete line fold, a header
    /// without a colon or with an invalid name (a 400).
    MalformedHead,
    /// Not an origin-form target ("/path?query"), or a malformed request
    /// line (a 400).
    BadTarget,
};

pub const Info = struct {
    header_count: usize,
    /// Request target as received, e.g. "/api/v1/health?x=1".
    target: []const u8,
    /// The part of the target before '?', still percent-encoded.
    path: []const u8,
    /// The part after '?', without it; empty when there is none.
    query: []const u8,
};

/// `head` is what std.http.Server.receiveHead returned: the lines up to and
/// including the empty line, so it ends in "\r\n\r\n".
pub fn check(head: []const u8, max_headers: usize) Error!Info {
    if (!std.mem.endsWith(u8, head, "\r\n\r\n")) return error.MalformedHead;
    for (head, 0..) |c, i| {
        switch (c) {
            '\r' => if (i + 1 >= head.len or head[i + 1] != '\n') return error.MalformedHead,
            '\n' => if (i == 0 or head[i - 1] != '\r') return error.MalformedHead,
            '\t' => {},
            else => if (c < 0x20 or c == 0x7f) return error.MalformedHead,
        }
    }

    var lines = std.mem.splitSequence(u8, head, "\r\n");
    const request_line = lines.next().?;
    // "METHOD SP target SP HTTP/x.y": exactly two spaces.
    const first_sp = std.mem.findScalar(u8, request_line, ' ') orelse return error.BadTarget;
    const last_sp = std.mem.findScalarLast(u8, request_line, ' ').?;
    if (first_sp == last_sp or first_sp == 0) return error.BadTarget;
    const target = request_line[first_sp + 1 .. last_sp];
    if (target.len == 0 or target[0] != '/' or std.mem.findScalar(u8, target, ' ') != null) return error.BadTarget;
    // Only the target may carry bytes above 0x7f (Go accepts and escapes
    // them); the method and version may not.
    if (std.mem.findScalar(u8, target, 0x7f) != null) return error.BadTarget;

    var count: usize = 0;
    while (lines.next()) |line| {
        if (line.len == 0) break;
        const colon = std.mem.findScalar(u8, line, ':') orelse return error.MalformedHead;
        if (colon == 0) return error.MalformedHead;
        for (line[0..colon]) |c| if (!isTokenChar(c)) return error.MalformedHead;
        count += 1;
        if (count > max_headers) return error.TooManyHeaders;
    }

    const q = std.mem.findScalar(u8, target, '?');
    return .{
        .header_count = count,
        .target = target,
        .path = if (q) |i| target[0..i] else target,
        .query = if (q) |i| target[i + 1 ..] else "",
    };
}

/// RFC 9110 tchar.
fn isTokenChar(c: u8) bool {
    return switch (c) {
        '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~' => true,
        else => std.ascii.isAlphanumeric(c),
    };
}

const testing = std.testing;

test "a normal head" {
    const info = try check("GET /api/v1/health?x=1&y=2 HTTP/1.1\r\nHost: a\r\nX-Api-Key: k\r\n\r\n", 10);
    try testing.expectEqual(2, info.header_count);
    try testing.expectEqualStrings("/api/v1/health", info.path);
    try testing.expectEqualStrings("x=1&y=2", info.query);
    try testing.expectEqualStrings("/api/v1/health?x=1&y=2", info.target);
}

test "no query" {
    const info = try check("GET / HTTP/1.1\r\n\r\n", 10);
    try testing.expectEqualStrings("/", info.path);
    try testing.expectEqualStrings("", info.query);
}

test "too many headers" {
    try testing.expectError(error.TooManyHeaders, check("GET / HTTP/1.1\r\na: 1\r\nb: 2\r\nc: 3\r\n\r\n", 2));
    _ = try check("GET / HTTP/1.1\r\na: 1\r\nb: 2\r\n\r\n", 2);
}

test "malformed heads are refused" {
    const bad = [_][]const u8{
        "GET / HTTP/1.1\r\nHost: a\nX: b\r\n\r\n", // bare LF
        "GET / HTTP/1.1\r\nHost: a\rX: b\r\n\r\n", // bare CR
        "GET / HTTP/1.1\r\nHost : a\r\n\r\n", // space before the colon
        "GET / HTTP/1.1\r\n folded: a\r\n\r\n", // obsolete line fold
        "GET / HTTP/1.1\r\nNoColon\r\n\r\n",
        "GET / HTTP/1.1\r\n: empty-name\r\n\r\n",
        "GET / HTTP/1.1\r\nX: a\x00b\r\n\r\n", // NUL
        "GET / HTTP/1.1\r\nX: a\x7fb\r\n\r\n", // DEL
    };
    for (bad) |h| try testing.expectError(error.MalformedHead, check(h, 10));
}

test "bad request lines are refused" {
    const bad = [_][]const u8{
        "GET HTTP/1.1\r\n\r\n",
        "GET /a b HTTP/1.1\r\n\r\n",
        "GET http://example.com/ HTTP/1.1\r\n\r\n",
        "OPTIONS * HTTP/1.1\r\n\r\n",
        "GET  / HTTP/1.1\r\n\r\n",
        " / HTTP/1.1\r\n\r\n",
        "GET /\x01 HTTP/1.1\r\n\r\n",
    };
    for (bad) |h| {
        const res = check(h, 10);
        if (res) |_| return error.TestExpectedError else |_| {}
    }
}

test "high bytes in the target are accepted" {
    _ = try check("GET /caf\xc3\xa9 HTTP/1.1\r\n\r\n", 10);
}

fn fuzzHead(_: void, smith: *testing.Smith) !void {
    var buf: [512]u8 = undefined;
    const n = smith.slice(&buf);
    const info = check(buf[0..n], 16) catch return;
    // What passes is something std.http parses without trouble, and the
    // header iterator (which assumes a well-formed head) stays in bounds.
    _ = std.http.Server.Request.Head.parse(buf[0..n]) catch {};
    var it = std.http.HeaderIterator.init(buf[0..n]);
    var seen: usize = 0;
    while (it.next()) |_| seen += 1;
    try testing.expect(seen >= info.header_count or it.is_trailer);
    try testing.expect(info.path.len <= info.target.len);
    try testing.expect(info.target[0] == '/');
}

fn seed(comptime s: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, s.len)) ++ s.*);
}

test "fuzz: request head check" {
    try testing.fuzz({}, fuzzHead, .{ .corpus = &.{
        seed("GET /api/v1/health HTTP/1.1\r\nHost: a\r\n\r\n"),
        seed("POST /x?y=1 HTTP/1.1\r\nContent-Length: 3\r\nTransfer-Encoding: chunked\r\n\r\n"),
        seed("GET / HTTP/1.1\r\nHost : a\r\n\r\n"),
        seed("GET / HTTP/1.1\r\n a: b\r\n\r\n"),
    } });
}
