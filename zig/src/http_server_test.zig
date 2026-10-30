//! End-to-end tests of the HTTP server: a real server on 127.0.0.1:0, talked
//! to over raw sockets (so malformed, oversized and slow requests can be sent)
//! and once with std.http.Client.
const std = @import("std");
const Io = std.Io;
const net = Io.net;
const testing = std.testing;
const srv = @import("http_server.zig");
const common = @import("http_common.zig");
const ex = @import("http_exchange.zig");

const key = "bitnews_testkey0123456789";

// ---- test doubles ----------------------------------------------------------

const TestKeys = struct {
    fn check(_: *anyopaque, presented: []const u8) bool {
        return std.mem.eql(u8, presented, key);
    }
    var dummy: u8 = 0;
    const checker: common.KeyChecker = .{ .ctx = &dummy, .check = check };
};

/// Collects log lines as "level msg k=v k=v".
const TestLog = struct {
    mutex: Io.Mutex = .init,
    text: std.ArrayList(u8) = .empty,

    fn logger(l: *TestLog) common.Logger {
        return .{ .ctx = l, .log = write };
    }

    fn write(ctx: *anyopaque, level: common.Level, msg: []const u8, attrs: []const common.Attr) void {
        const l: *TestLog = @ptrCast(@alignCast(ctx));
        l.mutex.lockUncancelable(testing.io);
        defer l.mutex.unlock(testing.io);
        var aw: Io.Writer.Allocating = .fromArrayList(testing.allocator, &l.text);
        defer l.text = aw.toArrayList();
        aw.writer.print("{s} {s}", .{ @tagName(level), msg }) catch return;
        for (attrs) |a| switch (a.value) {
            .str => |v| aw.writer.print(" {s}={s}", .{ a.key, v }) catch return,
            .int => |v| aw.writer.print(" {s}={d}", .{ a.key, v }) catch return,
            .uint => |v| aw.writer.print(" {s}={d}", .{ a.key, v }) catch return,
            .duration_ns => |v| aw.writer.print(" {s}={d}ns", .{ a.key, v }) catch return,
        };
        aw.writer.writeByte('\n') catch return;
    }

    fn contains(l: *TestLog, needle: []const u8) bool {
        l.mutex.lockUncancelable(testing.io);
        defer l.mutex.unlock(testing.io);
        return std.mem.find(u8, l.text.items, needle) != null;
    }

    fn deinit(l: *TestLog) void {
        l.text.deinit(testing.allocator);
    }
};

fn echo(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const body = try e.bodyAlloc();
    e.res.status = .ok;
    e.res.body = body;
}

fn slow(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try e.io.sleep(.fromMilliseconds(300), .awake);
    try e.res.json(e.arena, .ok, .{ .slow = true });
}

fn nobody(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try e.res.json(e.arena, .ok, .{ .ok = true });
}

fn boom(_: ?*anyopaque, _: *ex.Exchange) anyerror!void {
    return error.SecretDetail;
}

/// How the stream handlers below ended: 1 the client left, 2 the server is
/// stopping, 3 a write failed.
var stream_outcome: std.atomic.Value(u8) = .init(0);

fn streamShort(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try e.res.setHeader(e.arena, "Content-Type", "text/plain");
    try e.startStream();
    try e.streamWrite("one");
    try e.streamWrite("two");
}

fn streamWatch(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try e.startStream();
    try e.streamWrite("hi");
    while (true) {
        try e.io.sleep(.fromMilliseconds(20), .awake);
        switch (try e.streamState()) {
            .open => {},
            .peer_gone => return stream_outcome.store(1, .release),
            .stopping => return stream_outcome.store(2, .release),
        }
    }
}

fn streamFlood(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try e.startStream();
    const block = try e.arena.alloc(u8, 64 << 10);
    @memset(block, 'x');
    while (true) e.streamWrite(block) catch return stream_outcome.store(3, .release);
}

const Harness = struct {
    log: TestLog = .{},
    server: *srv.Server,
    fut: Io.Future(error{NotListening}!void),
    port: u16,
    stopped: bool = false,

    fn start(h: *Harness, limits: srv.Limits) !void {
        return h.startGated(limits, null);
    }

    fn startGated(h: *Harness, limits: srv.Limits, gate: ?common.SetupGate) !void {
        h.log = .{};
        h.server = try srv.Server.init(testing.allocator, testing.io, .{
            .logger = h.log.logger(),
            .keys = TestKeys.checker,
            .version = .{ .version = "1.2.3", .commit = "abc123", .zig_version = "0.17.0" },
            .limits = limits,
            .setup_gate = gate,
        });
        errdefer h.server.deinit();
        try h.server.routes.add("POST /echo", .{ .handler = echo, .auth = .none, .body_limit = 16 });
        try h.server.routes.add("GET /slow", .{ .handler = slow, .auth = .none });
        try h.server.routes.add("POST /nobody", .{ .handler = nobody, .auth = .none, .body_limit = 16 });
        try h.server.routes.add("GET /boom", .{ .handler = boom, .auth = .none });
        try h.server.routes.add("GET /stream", .{ .handler = streamShort, .auth = .none });
        try h.server.routes.add("GET /stream-watch", .{ .handler = streamWatch, .auth = .none });
        try h.server.routes.add("GET /stream-flood", .{ .handler = streamFlood, .auth = .none });
        h.port = try h.server.listen("127.0.0.1:0");
        h.fut = try testing.io.concurrent(srv.Server.serve, .{h.server});
        h.stopped = false;
    }

    /// Cancels the serving task and returns how long the shutdown took.
    fn stop(h: *Harness) !i64 {
        const t0 = Io.Clock.Timestamp.now(testing.io, .awake);
        try h.fut.cancel(testing.io);
        h.stopped = true;
        const took = t0.durationTo(Io.Clock.Timestamp.now(testing.io, .awake));
        return took.raw.toMilliseconds();
    }

    fn deinit(h: *Harness) void {
        if (!h.stopped) _ = h.stop() catch {};
        h.server.deinit();
        h.log.deinit();
    }

    fn open(h: *const Harness) !*Client {
        return Client.open(h.port);
    }

    /// Sends `bytes` on a new connection and returns everything the server
    /// says until it closes the connection.
    fn roundTrip(h: *const Harness, arena: std.mem.Allocator, bytes: []const u8) !Reply {
        const c = try h.open();
        defer c.close();
        try c.send(bytes);
        return c.readReply(arena);
    }
};

const Reply = struct {
    status: u16,
    head: []const u8,
    body: []const u8,

    fn header(r: Reply, name: []const u8) ?[]const u8 {
        var it = std.mem.splitSequence(u8, r.head, "\r\n");
        _ = it.next();
        while (it.next()) |line| {
            const colon = std.mem.findScalar(u8, line, ':') orelse continue;
            if (std.ascii.eqlIgnoreCase(line[0..colon], name)) return std.mem.trim(u8, line[colon + 1 ..], " ");
        }
        return null;
    }
};

const Client = struct {
    stream: net.Stream,
    rbuf: [4096]u8 = undefined,
    wbuf: [1024]u8 = undefined,
    reader: srv.TimedReader,
    writer: net.Stream.Writer,

    fn open(port: u16) !*Client {
        const addr: net.IpAddress = try .parse("127.0.0.1", port);
        const c = try testing.allocator.create(Client);
        errdefer testing.allocator.destroy(c);
        c.stream = try addr.connect(testing.io, .{ .mode = .stream });
        c.reader = .init(testing.io, c.stream, &c.rbuf);
        c.writer = c.stream.writer(testing.io, &c.wbuf);
        return c;
    }

    fn close(c: *Client) void {
        c.stream.close(testing.io);
        testing.allocator.destroy(c);
    }

    fn send(c: *Client, bytes: []const u8) !void {
        try c.writer.interface.writeAll(bytes);
        try c.writer.interface.flush();
    }

    /// Reads one response (head and body by Content-Length) or, when the
    /// server closes first, whatever came. error.ClientTimeout after 5 s.
    fn readReply(c: *Client, arena: std.mem.Allocator) !Reply {
        c.reader.deadline = srv.deadlineIn(testing.io, 5000);
        const r = &c.reader.interface;
        var head: std.ArrayList(u8) = .empty;
        while (true) {
            const line = r.takeDelimiterInclusive('\n') catch |err| switch (err) {
                error.EndOfStream => return .{ .status = 0, .head = head.items, .body = "" },
                error.ReadFailed => return clientFailure(c),
                error.StreamTooLong => return error.StreamTooLong,
            };
            try head.appendSlice(arena, line);
            if (line.len == 2) break;
        }
        const status = try std.fmt.parseInt(u16, head.items[9..12], 10);
        var reply: Reply = .{ .status = status, .head = head.items, .body = "" };
        if (reply.header("content-length")) |cl| {
            const n = try std.fmt.parseInt(usize, cl, 10);
            // A HEAD response announces a length and sends no body; the caller
            // knows and uses readHead for it.
            reply.body = r.readAlloc(arena, n) catch |err| switch (err) {
                error.ReadFailed => return clientFailure(c),
                else => |e| return e,
            };
        }
        return reply;
    }

    /// Whether the server has closed the connection (reads end of stream).
    fn closedByServer(c: *Client, within_ms: u32) !bool {
        c.reader.deadline = srv.deadlineIn(testing.io, within_ms);
        c.reader.err = null;
        _ = c.reader.interface.peekByte() catch |err| switch (err) {
            error.EndOfStream => return true,
            error.ReadFailed => {
                if (c.reader.err) |cause| {
                    if (cause == error.Timeout) return false;
                    if (cause == error.ConnectionResetByPeer) return true;
                }
                return error.ReadFailed;
            },
        };
        return false;
    }

    fn clientFailure(c: *Client) error{ ClientTimeout, ReadFailed } {
        if (c.reader.err) |cause| if (cause == error.Timeout) return error.ClientTimeout;
        return error.ReadFailed;
    }
};

fn get(h: *const Harness, arena: std.mem.Allocator, path: []const u8, extra: []const u8) !Reply {
    const req = try std.fmt.allocPrint(arena, "GET {s} HTTP/1.1\r\nHost: t\r\nConnection: close\r\n{s}\r\n", .{ path, extra });
    return h.roundTrip(arena, req);
}

fn expectErrorBody(want_code: []const u8, want_message: []const u8, r: Reply) !void {
    var buf: [256]u8 = undefined;
    const want = try std.fmt.bufPrint(&buf, "{{\"error\":{{\"code\":\"{s}\",\"message\":\"{s}\"}}}}\n", .{ want_code, want_message });
    try testing.expectEqualStrings(want, r.body);
    try testing.expectEqualStrings("application/json", r.header("Content-Type").?);
    try testing.expectEqualStrings("no-store", r.header("Cache-Control").?);
}

fn repeat(arena: std.mem.Allocator, c: u8, n: usize) ![]u8 {
    const buf = try arena.alloc(u8, n);
    @memset(buf, c);
    return buf;
}

fn newArena() std.heap.ArenaAllocator {
    return .init(testing.allocator);
}

// ---- the routes ------------------------------------------------------------

test "listen on port 0 returns the bound port, and bad addresses are refused" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    try testing.expect(h.port != 0);
    try testing.expectError(error.AlreadyListening, h.server.listen("127.0.0.1:0"));
}

test "a second server cannot bind a port that is taken" {
    var first: Harness = undefined;
    try first.start(.{});
    defer first.deinit();

    // SO_REUSEPORT would let the second bind succeed and share the port.
    const second = try srv.Server.init(testing.allocator, testing.io, .{ .keys = TestKeys.checker });
    defer second.deinit();
    var buf: [32]u8 = undefined;
    const addr = try std.fmt.bufPrint(&buf, "127.0.0.1:{d}", .{first.port});

    try testing.expectError(error.AddressInUse, second.listen(addr));
}

test "listen addresses" {
    try testing.expectError(error.InvalidAddress, srv.parseAddress("nonsense"));
    try testing.expectError(error.InvalidAddress, srv.parseAddress("127.0.0.1:99999"));
    try testing.expectError(error.InvalidAddress, srv.parseAddress("127.0.0.1:"));
    try testing.expectError(error.InvalidAddress, srv.parseAddress("example.invalid:80"));
    try testing.expectEqual(9494, (try srv.parseAddress("127.0.0.1:9494")).getPort());
    try testing.expectEqual(9494, (try srv.parseAddress(":9494")).getPort());
    try testing.expectEqual(9494, (try srv.parseAddress("localhost:9494")).getPort());
    try testing.expectEqual(9494, (try srv.parseAddress("[::1]:9494")).getPort());
    try testing.expectEqual(0, (try srv.parseAddress("0.0.0.0:0")).getPort());
}

test "health is OK without a key, with the API headers" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();

    const r = try get(&h, arena.allocator(), "/api/v1/health", "");

    try testing.expectEqual(200, r.status);
    try testing.expectEqualStrings("{\"status\":\"ok\"}\n", r.body);
    try testing.expectEqualStrings("application/json", r.header("Content-Type").?);
    try testing.expectEqualStrings("no-store", r.header("Cache-Control").?);
    try testing.expectEqualStrings("default-src 'self'; frame-ancestors 'none'", r.header("Content-Security-Policy").?);
    try testing.expectEqualStrings("nosniff", r.header("X-Content-Type-Options").?);
    try testing.expectEqualStrings("same-origin", r.header("Referrer-Policy").?);
    try testing.expectEqual(32, r.header("X-Request-ID").?.len);
    try testing.expectEqual(29, r.header("Date").?.len);
}

const TestGate = struct {
    var open = std.atomic.Value(bool).init(true);
    var dummy: u8 = 0;
    fn required(_: *anyopaque) bool {
        return open.load(.acquire);
    }
    const gate: common.SetupGate = .{ .ctx = &dummy, .required = required };
};

test "while setup is open everything but GET health is 503 setup_required, then it is gone" {
    TestGate.open.store(true, .release);
    var h: Harness = undefined;
    try h.startGated(.{}, TestGate.gate);
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const auth = "Authorization: Bearer " ++ key ++ "\r\n";

    const health = try get(&h, a, "/api/v1/health", "");
    const version = try get(&h, a, "/api/v1/version", auth);
    const unknown = try get(&h, a, "/api/v1/nope", "");
    const post_health = try h.roundTrip(a, "POST /api/v1/health HTTP/1.1\r\nHost: t\r\nConnection: close\r\nContent-Length: 0\r\n\r\n");

    try testing.expectEqual(200, health.status);
    const msg = "first-run setup is required: create the admin account with the setup token from the server log";
    for ([_]Reply{ version, unknown, post_health }) |r| {
        try testing.expectEqual(503, r.status);
        try expectErrorBody("setup_required", msg, r);
    }

    TestGate.open.store(false, .release);
    const after = try get(&h, a, "/api/v1/version", auth);
    try testing.expectEqual(200, after.status);
}

test "every response carries the security headers, errors included" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    for ([_][]const u8{ "/api/v1/health", "/api/v1/version", "/nope" }) |path| {
        const r = try get(&h, arena.allocator(), path, "");
        try testing.expectEqualStrings("nosniff", r.header("X-Content-Type-Options").?);
        try testing.expectEqualStrings("same-origin", r.header("Referrer-Policy").?);
        try testing.expect(r.header("Content-Security-Policy") != null);
    }
}

test "version needs the app key" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();

    const none = try get(&h, a, "/api/v1/version", "");
    try testing.expectEqual(401, none.status);
    try testing.expectEqualStrings("Bearer", none.header("WWW-Authenticate").?);
    try expectErrorBody("unauthorized", "a valid API key is required", none);

    const wrong = try get(&h, a, "/api/v1/version", "Authorization: Bearer nope\r\n");
    try testing.expectEqual(401, wrong.status);
    try testing.expectEqualStrings("Bearer", wrong.header("WWW-Authenticate").?);

    const bearer = try get(&h, a, "/api/v1/version", "Authorization: Bearer " ++ key ++ "\r\n");
    try testing.expectEqual(200, bearer.status);
    try testing.expectEqualStrings("{\"commit\":\"abc123\",\"version\":\"1.2.3\",\"zig_version\":\"0.17.0\"}\n", bearer.body);
    try testing.expectEqualStrings("no-store", bearer.header("Cache-Control").?);

    const header_key = try get(&h, a, "/api/v1/version", "X-Api-Key: " ++ key ++ "\r\n");
    try testing.expectEqual(200, header_key.status);
}

test "a wrong Bearer is not rescued by a good X-Api-Key, and Bearer wins over it" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();

    const bad_then_good = try get(&h, a, "/api/v1/version", "Authorization: Bearer nope\r\nX-Api-Key: " ++ key ++ "\r\n");
    try testing.expectEqual(401, bad_then_good.status);
    const good_then_bad = try get(&h, a, "/api/v1/version", "Authorization: Bearer " ++ key ++ "\r\nX-Api-Key: nope\r\n");
    try testing.expectEqual(200, good_then_bad.status);
    const basic = try get(&h, a, "/api/v1/version", "Authorization: Basic abc\r\nX-Api-Key: " ++ key ++ "\r\n");
    try testing.expectEqual(200, basic.status);
}

test "unknown paths are a JSON 404" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const r = try get(&h, arena.allocator(), "/api/v1/nope", "");
    try testing.expectEqual(404, r.status);
    try expectErrorBody("not_found", "no such route", r);
    const slash = try get(&h, arena.allocator(), "/api/v1/health/", "");
    try testing.expectEqual(404, slash.status);
}

test "a wrong method is a JSON 405 with an Allow header" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const r = try h.roundTrip(arena.allocator(), "POST /api/v1/health HTTP/1.1\r\nHost: t\r\nConnection: close\r\nContent-Length: 0\r\n\r\n");
    try testing.expectEqual(405, r.status);
    try testing.expectEqualStrings("GET, HEAD", r.header("Allow").?);
    try expectErrorBody("method_not_allowed", "this method is not allowed for this path", r);
}

test "HEAD answers the GET headers without a body" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const c = try h.open();
    defer c.close();
    try c.send("HEAD /api/v1/health HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n");
    c.reader.deadline = srv.deadlineIn(testing.io, 5000);
    const all = try c.reader.interface.allocRemaining(arena.allocator(), .limited(4096));
    try testing.expect(std.mem.startsWith(u8, all, "HTTP/1.1 200 OK\r\n"));
    try testing.expect(std.mem.find(u8, all, "content-length: 16\r\n") != null);
    try testing.expect(std.mem.endsWith(u8, all, "\r\n\r\n"));
}

test "unclean paths redirect with the query kept" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const r = try get(&h, arena.allocator(), "/api//v1/./health?x=1", "");
    try testing.expectEqual(307, r.status);
    try testing.expectEqualStrings("/api/v1/health?x=1", r.header("Location").?);
}

test "a bad percent escape is a 400" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const r = try get(&h, arena.allocator(), "/api/%zz", "");
    try testing.expectEqual(400, r.status);
}

test "request IDs: echoed when safe, replaced when not" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const ok = try get(&h, a, "/api/v1/health", "X-Request-ID: trace-123.A_b\r\n");
    try testing.expectEqualStrings("trace-123.A_b", ok.header("X-Request-ID").?);
    for ([_][]const u8{ "a b", "a\"b", "caf\xc3\xa9", try repeat(a, 'a', 65) }) |bad| {
        const r = try get(&h, a, "/api/v1/health", try std.fmt.allocPrint(a, "X-Request-ID: {s}\r\n", .{bad}));
        try testing.expectEqual(32, r.header("X-Request-ID").?.len);
        try testing.expect(!std.mem.eql(u8, bad, r.header("X-Request-ID").?));
    }
}

test "a handler error is a generic 500 and is logged with the request ID" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const r = try get(&h, arena.allocator(), "/boom", "X-Request-ID: req-boom\r\n");
    try testing.expectEqual(500, r.status);
    try expectErrorBody("internal", "internal error", r);
    try testing.expect(std.mem.find(u8, r.body, "SecretDetail") == null);
    _ = try h.stop();
    try testing.expect(h.log.contains("err request failed request_id=req-boom err=SecretDetail"));
    try testing.expect(h.log.contains("status=500"));
}

test "the access log has the request, redacts the URL and keeps health at debug" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    _ = try get(&h, arena.allocator(), "/api/v1/health?apikey=hunter2&x=1", "X-Request-ID: req-1\r\n");
    _ = try get(&h, arena.allocator(), "/api/v1/version?apikey=hunter2", "X-Request-ID: req-2\r\n");
    _ = try h.stop();
    try testing.expect(h.log.contains("debug request request_id=req-1 method=GET url=/api/v1/health?apikey=REDACTED&x=1 status=200"));
    try testing.expect(h.log.contains("info request request_id=req-2 method=GET url=/api/v1/version?apikey=REDACTED status=401"));
    try testing.expect(!h.log.contains("hunter2"));
}

// ---- limits ----------------------------------------------------------------

test "an oversized header is a 431 and the connection closes" {
    var h: Harness = undefined;
    try h.start(.{ .max_header_bytes = 1024 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const req = try std.fmt.allocPrint(a, "GET /api/v1/health HTTP/1.1\r\nHost: t\r\nX-Big: {s}\r\n\r\n", .{try repeat(a, 'a', 2048)});
    const c = try h.open();
    defer c.close();
    try c.send(req);
    const r = try c.readReply(a);
    try testing.expectEqual(431, r.status);
    try expectErrorBody("request_header_too_large", "the request header is too large", r);
    try testing.expectEqualStrings("close", r.header("Connection").?);
    try testing.expect(try c.closedByServer(2000));
}

test "too many headers is a 431" {
    var h: Harness = undefined;
    try h.start(.{ .max_headers = 8 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    var req: std.ArrayList(u8) = .empty;
    try req.appendSlice(a, "GET /api/v1/health HTTP/1.1\r\nHost: t\r\nConnection: close\r\n");
    for (0..20) |i| try req.print(a, "X-{d}: v\r\n", .{i});
    try req.appendSlice(a, "\r\n");
    const r = try h.roundTrip(a, req.items);
    try testing.expectEqual(431, r.status);
}

test "malformed requests are refused" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const cases = [_]struct { req: []const u8, status: u16 }{
        .{ .req = "GET /api/v1/health HTTP/1.1\r\nHost: t\nX-A: b\r\n\r\n", .status = 400 }, // bare LF
        .{ .req = "GET /api/v1/health HTTP/1.1\r\nHost : t\r\n\r\n", .status = 400 }, // space before colon
        .{ .req = "GET /api/v1/health HTTP/1.1\r\nHost: t\r\n a: folded\r\n\r\n", .status = 400 },
        .{ .req = "GET api/v1/health HTTP/1.1\r\nHost: t\r\n\r\n", .status = 400 },
        .{ .req = "GET http://t/api/v1/health HTTP/1.1\r\nHost: t\r\n\r\n", .status = 400 },
        .{ .req = "GET /api/v1/health HTTP/9.9\r\nHost: t\r\n\r\n", .status = 400 },
        .{ .req = "FETCH /api/v1/health HTTP/1.1\r\nHost: t\r\n\r\n", .status = 501 },
        .{ .req = "POST /echo HTTP/1.1\r\nHost: t\r\nContent-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", .status = 400 },
        .{ .req = "POST /echo HTTP/1.1\r\nHost: t\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\nx", .status = 400 },
        .{ .req = "POST /echo HTTP/1.1\r\nHost: t\r\nContent-Length: abc\r\n\r\n", .status = 400 },
        .{ .req = "GET /api/v1/health HTTP/1.1\r\nHost: t\r\nExpect: 200-ok\r\n\r\n", .status = 417 },
    };
    for (cases) |tc| {
        const r = try h.roundTrip(a, tc.req);
        testing.expectEqual(tc.status, r.status) catch |err| {
            std.debug.print("request: {s}\n", .{tc.req});
            return err;
        };
        try testing.expectEqualStrings("application/json", r.header("Content-Type").?);
    }
}

test "bodies: within the limit, over it declared, over it chunked" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();

    const ok = try h.roundTrip(a, "POST /echo HTTP/1.1\r\nHost: t\r\nConnection: close\r\nContent-Length: 5\r\n\r\nhello");
    try testing.expectEqual(200, ok.status);
    try testing.expectEqualStrings("hello", ok.body);

    const chunked_ok = try h.roundTrip(a, "POST /echo HTTP/1.1\r\nHost: t\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n");
    try testing.expectEqual(200, chunked_ok.status);
    try testing.expectEqualStrings("abcde", chunked_ok.body);

    // Declared too large: refused without reading the body (nothing is sent).
    const c = try h.open();
    defer c.close();
    try c.send("POST /echo HTTP/1.1\r\nHost: t\r\nContent-Length: 1000000000\r\n\r\n");
    const declared = try c.readReply(a);
    try testing.expectEqual(413, declared.status);
    try expectErrorBody("request_too_large", "request body is too large", declared);
    try testing.expect(try c.closedByServer(2000));

    const chunked_big = try h.roundTrip(a, "POST /echo HTTP/1.1\r\nHost: t\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n" ++
        "20\r\n" ++ "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" ++ "\r\n0\r\n\r\n");
    try testing.expectEqual(413, chunked_big.status);
    try expectErrorBody("request_too_large", "request body is too large", chunked_big);
}

test "Expect: 100-continue gets its interim response before the body is read" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const c = try h.open();
    defer c.close();
    try c.send("POST /echo HTTP/1.1\r\nHost: t\r\nConnection: close\r\nExpect: 100-continue\r\nContent-Length: 5\r\n\r\n");
    try testing.expectEqual(100, (try c.readReply(a)).status);
    try c.send("hello");
    const r = try c.readReply(a);
    try testing.expectEqual(200, r.status);
    try testing.expectEqualStrings("hello", r.body);

    // A body that is refused is not invited.
    const big = try h.open();
    defer big.close();
    try big.send("POST /echo HTTP/1.1\r\nHost: t\r\nExpect: 100-continue\r\nContent-Length: 99999\r\n\r\n");
    try testing.expectEqual(413, (try big.readReply(a)).status);
}

test "HTTP/1.1 needs exactly one Host" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    try testing.expectEqual(400, (try h.roundTrip(a, "GET /api/v1/health HTTP/1.1\r\nConnection: close\r\n\r\n")).status);
    try testing.expectEqual(400, (try h.roundTrip(a, "GET /api/v1/health HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n")).status);
    try testing.expectEqual(200, (try h.roundTrip(a, "GET /api/v1/health HTTP/1.0\r\n\r\n")).status);
}

// Go refuses an over-long body when the handler reads it (MaxBytesReader),
// not when the head arrives, so a route that ignores bodies, like health,
// still answers; the connection is closed because the body was not consumed.
test "a declared body above a route's limit does not stop a route that never reads it" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const r = try h.roundTrip(arena.allocator(), "POST /nobody HTTP/1.1\r\nHost: t\r\nContent-Length: 100000\r\n\r\n");
    try testing.expectEqual(200, r.status);
    try testing.expectEqualStrings("close", r.header("Connection").?);
}

test "a body on a method that has none cannot smuggle a second request" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const inner = "GET /api/v1/version HTTP/1.1\r\nHost: t\r\n\r\n";
    const req = try std.fmt.allocPrint(a, "GET /api/v1/health HTTP/1.1\r\nHost: t\r\nContent-Length: {d}\r\n\r\n{s}", .{ inner.len, inner });
    const c = try h.open();
    defer c.close();
    try c.send(req);
    const first = try c.readReply(a);
    try testing.expectEqual(200, first.status);
    try testing.expectEqualStrings("close", first.header("Connection").?);
    try testing.expect(try c.closedByServer(2000));
}

test "keep-alive serves several requests on one connection" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const c = try h.open();
    defer c.close();
    for (0..3) |_| {
        try c.send("GET /api/v1/health HTTP/1.1\r\nHost: t\r\n\r\n");
        const r = try c.readReply(a);
        try testing.expectEqual(200, r.status);
        try testing.expect(r.header("Connection") == null);
    }
    // Two requests in one write (pipelined).
    try c.send("GET /api/v1/health HTTP/1.1\r\nHost: t\r\n\r\nGET /nope HTTP/1.1\r\nHost: t\r\n\r\n");
    try testing.expectEqual(200, (try c.readReply(a)).status);
    try testing.expectEqual(404, (try c.readReply(a)).status);
}

test "slowloris: a header that never ends is cut off" {
    var h: Harness = undefined;
    try h.start(.{ .read_header_timeout_ms = 300 });
    defer h.deinit();
    const c = try h.open();
    defer c.close();
    try c.send("GET /api/v1/health HTTP/1.1\r\nHost: t\r\n");
    try testing.expect(try c.closedByServer(3000));
}

test "slowloris: dribbling bytes does not extend the header deadline" {
    var h: Harness = undefined;
    try h.start(.{ .read_header_timeout_ms = 400 });
    defer h.deinit();
    const c = try h.open();
    defer c.close();
    const t0 = Io.Clock.Timestamp.now(testing.io, .awake);
    var closed = false;
    for ("GET /api/v1/health HTTP/1.1\r\nX-A: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") |byte| {
        c.send(&.{byte}) catch {
            closed = true;
            break;
        };
        try testing.io.sleep(.fromMilliseconds(60), .awake);
        if (try c.closedByServer(1)) {
            closed = true;
            break;
        }
    }
    try testing.expect(closed);
    const took = t0.durationTo(Io.Clock.Timestamp.now(testing.io, .awake)).raw.toMilliseconds();
    try testing.expect(took < 2000);
}

test "a silent connection is closed after the header timeout" {
    var h: Harness = undefined;
    try h.start(.{ .read_header_timeout_ms = 200 });
    defer h.deinit();
    const c = try h.open();
    defer c.close();
    try testing.expect(try c.closedByServer(3000));
}

test "an idle keep-alive connection is closed after the idle timeout" {
    var h: Harness = undefined;
    try h.start(.{ .idle_timeout_ms = 300 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const c = try h.open();
    defer c.close();
    try c.send("GET /api/v1/health HTTP/1.1\r\nHost: t\r\n\r\n");
    try testing.expectEqual(200, (try c.readReply(arena.allocator())).status);
    try testing.expect(!try c.closedByServer(50));
    try testing.expect(try c.closedByServer(3000));
}

test "a stalled body is cut off by the read timeout" {
    var h: Harness = undefined;
    try h.start(.{ .read_timeout_ms = 300 });
    defer h.deinit();
    const c = try h.open();
    defer c.close();
    try c.send("POST /echo HTTP/1.1\r\nHost: t\r\nContent-Length: 10\r\n\r\nabc");
    var arena = newArena();
    defer arena.deinit();
    const r = try c.readReply(arena.allocator());
    try testing.expectEqual(400, r.status);
    try testing.expect(try c.closedByServer(3000));
}

test "connections above the cap get a 503, and room frees up" {
    var h: Harness = undefined;
    try h.start(.{ .max_connections = 4 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();

    var held: [4]*Client = undefined;
    for (&held) |*slot| {
        slot.* = try h.open();
        // A partial request keeps the connection busy in the server.
        try slot.*.send("GET /api/v1/health HTTP/1.1\r\n");
    }
    // Let the server accept all four before the fifth arrives.
    try testing.io.sleep(.fromMilliseconds(200), .awake);

    const extra = try h.open();
    defer extra.close();
    const r = try extra.readReply(a);
    try testing.expectEqual(503, r.status);
    try expectErrorBody("unavailable", "too many connections", r);
    try testing.expect(try extra.closedByServer(2000));

    held[0].close();
    var served = false;
    for (0..50) |_| {
        const ok = get(&h, a, "/api/v1/health", "") catch continue;
        if (ok.status == 200) {
            served = true;
            break;
        }
        try testing.io.sleep(.fromMilliseconds(50), .awake);
    }
    try testing.expect(served);
    for (held[1..]) |c| c.close();
}

test "many connections at once are all served" {
    var h: Harness = undefined;
    try h.start(.{ .max_connections = 64 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    var clients: [48]*Client = undefined;
    for (&clients) |*slot| slot.* = try h.open();
    for (clients) |c| try c.send("GET /api/v1/health HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n");
    for (clients) |c| {
        try testing.expectEqual(200, (try c.readReply(a)).status);
        c.close();
    }
}

// ---- shutdown --------------------------------------------------------------

fn cancelServe(io: Io, fut: *Io.Future(error{NotListening}!void)) void {
    _ = fut.cancel(io) catch {};
}

test "shutdown lets a request in flight finish, then closes" {
    var h: Harness = undefined;
    try h.start(.{ .shutdown_grace_ms = 5000 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();

    const c = try h.open();
    defer c.close();
    try c.send("GET /slow HTTP/1.1\r\nHost: t\r\n\r\n");
    try testing.io.sleep(.fromMilliseconds(100), .awake); // the handler is sleeping now

    const t0 = Io.Clock.Timestamp.now(testing.io, .awake);
    var stopper = try testing.io.concurrent(cancelServe, .{ testing.io, &h.fut });
    const r = try c.readReply(a);
    try testing.expectEqual(200, r.status);
    try testing.expectEqualStrings("{\"slow\":true}\n", r.body);
    try testing.expectEqualStrings("close", r.header("Connection").?);
    stopper.await(testing.io);
    h.stopped = true;
    const took = t0.durationTo(Io.Clock.Timestamp.now(testing.io, .awake)).raw.toMilliseconds();
    try testing.expect(took < 3000);
    try testing.expect(try c.closedByServer(1000));
}

test "shutdown cancels what outlasts the grace period" {
    var h: Harness = undefined;
    try h.start(.{ .shutdown_grace_ms = 50 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();

    const c = try h.open();
    defer c.close();
    try c.send("GET /slow HTTP/1.1\r\nHost: t\r\n\r\n");
    try testing.io.sleep(.fromMilliseconds(100), .awake);

    const took = try h.stop();
    try testing.expect(took < 250);
    // The handler was cancelled: no response, connection closed.
    const r = try c.readReply(arena.allocator());
    try testing.expectEqual(0, r.status);
}

test "shutdown closes idle and half-sent connections promptly" {
    var h: Harness = undefined;
    try h.start(.{ .shutdown_grace_ms = 100 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const idle = try h.open();
    defer idle.close();
    try idle.send("GET /api/v1/health HTTP/1.1\r\nHost: t\r\n\r\n");
    try testing.expectEqual(200, (try idle.readReply(arena.allocator())).status);
    const half = try h.open();
    defer half.close();
    try half.send("GET /api/v1/health HTTP/1.1\r\nHost: t\r\n");
    try testing.io.sleep(.fromMilliseconds(100), .awake);

    const took = try h.stop();
    try testing.expect(took < 1500);
    try testing.expect(try idle.closedByServer(1000));
    try testing.expect(try half.closedByServer(1000));
}

// ---- streaming responses ---------------------------------------------------

fn waitForOutcome(want: u8, within_ms: u32) !void {
    var waited: u32 = 0;
    while (stream_outcome.load(.acquire) != want and waited < within_ms) : (waited += 10) {
        try testing.io.sleep(.fromMilliseconds(10), .awake);
    }
    try testing.expectEqual(want, stream_outcome.load(.acquire));
}

test "a stream is chunked on HTTP/1.1 and the connection closes after it" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const c = try h.open();
    defer c.close();
    try c.send("GET /stream HTTP/1.1\r\nHost: t\r\n\r\n");

    c.reader.deadline = srv.deadlineIn(testing.io, 5000);
    const raw = try c.reader.interface.allocRemaining(arena.allocator(), .limited(65536));

    try testing.expect(std.mem.startsWith(u8, raw, "HTTP/1.1 200 OK\r\n"));
    try testing.expect(std.mem.find(u8, raw, "Transfer-Encoding: chunked\r\n") != null);
    try testing.expect(std.mem.find(u8, raw, "Connection: close\r\n") != null);
    try testing.expect(std.mem.find(u8, raw, "Content-Type: text/plain\r\n") != null);
    try testing.expect(std.mem.find(u8, raw, "X-Content-Type-Options: nosniff\r\n") != null);
    try testing.expect(std.mem.find(u8, raw, "Date: ") != null);
    try testing.expect(std.mem.endsWith(u8, raw, "\r\n\r\n3\r\none\r\n3\r\ntwo\r\n0\r\n\r\n"));
}

test "a stream is close-delimited on HTTP/1.0" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const c = try h.open();
    defer c.close();
    try c.send("GET /stream HTTP/1.0\r\n\r\n");

    c.reader.deadline = srv.deadlineIn(testing.io, 5000);
    const raw = try c.reader.interface.allocRemaining(arena.allocator(), .limited(65536));

    try testing.expect(std.mem.startsWith(u8, raw, "HTTP/1.0 200 OK\r\n"));
    try testing.expect(std.mem.find(u8, raw, "Transfer-Encoding") == null);
    try testing.expect(std.mem.endsWith(u8, raw, "\r\n\r\nonetwo"));
}

test "a stream notices a client that went away" {
    stream_outcome.store(0, .release);
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    const c = try h.open();
    errdefer c.close();
    try c.send("GET /stream-watch HTTP/1.1\r\nHost: t\r\n\r\n");
    c.reader.deadline = srv.deadlineIn(testing.io, 5000);
    while (true) {
        const line = try c.reader.interface.takeDelimiterInclusive('\n');
        if (line.len == 2) break;
    }
    c.close();

    try waitForOutcome(1, 3000);
}

test "a client that stops reading is cut by the stream write timeout, and the server goes on" {
    stream_outcome.store(0, .release);
    var h: Harness = undefined;
    try h.start(.{ .stream_write_timeout_ms = 200 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const c = try h.open();
    defer c.close();
    try c.send("GET /stream-flood HTTP/1.1\r\nHost: t\r\n\r\n");

    // The server is not held up while the stream is stuck ...
    try testing.io.sleep(.fromMilliseconds(100), .awake);
    try testing.expectEqual(200, (try get(&h, arena.allocator(), "/api/v1/health", "")).status);
    // ... and gives up on the stream.
    try waitForOutcome(3, 5000);
}

test "shutdown ends an open stream" {
    stream_outcome.store(0, .release);
    var h: Harness = undefined;
    try h.start(.{ .shutdown_grace_ms = 5000 });
    defer h.deinit();
    const c = try h.open();
    defer c.close();
    try c.send("GET /stream-watch HTTP/1.1\r\nHost: t\r\n\r\n");
    try testing.io.sleep(.fromMilliseconds(150), .awake);

    const took = try h.stop();

    try testing.expect(took < 1500);
    try testing.expectEqual(2, stream_outcome.load(.acquire));
}

// ---- interop ---------------------------------------------------------------

test "the std HTTP client can talk to it" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var client: std.http.Client = .{ .allocator = testing.allocator, .io = testing.io };
    defer client.deinit();
    const url = try std.fmt.allocPrint(testing.allocator, "http://127.0.0.1:{d}/api/v1/version", .{h.port});
    defer testing.allocator.free(url);
    var body: Io.Writer.Allocating = .init(testing.allocator);
    defer body.deinit();
    const res = try client.fetch(.{
        .location = .{ .url = url },
        .extra_headers = &.{.{ .name = "Authorization", .value = "Bearer " ++ key }},
        .response_writer = &body.writer,
    });
    try testing.expectEqual(.ok, res.status);
    try testing.expectEqualStrings("{\"commit\":\"abc123\",\"version\":\"1.2.3\",\"zig_version\":\"0.17.0\"}\n", body.written());
}
