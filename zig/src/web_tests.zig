//! End-to-end tests of the web UI routes (Z6).
const std = @import("std");
const Io = std.Io;
const net = Io.net;
const testing = std.testing;
const srv = @import("http_server.zig");
const common = @import("http_common.zig");
const web = @import("web.zig");

const key = "bitnews_testkey0123456789";

const TestKeys = struct {
    fn check(_: *anyopaque, presented: []const u8) bool {
        return std.mem.eql(u8, presented, key);
    }
    var dummy: u8 = 0;
    const checker: common.KeyChecker = .{ .ctx = &dummy, .check = check };
};

const TestGate = struct {
    var open = std.atomic.Value(bool).init(true);
    var dummy: u8 = 0;
    fn required(_: *anyopaque) bool {
        return open.load(.acquire);
    }
    const gate: common.SetupGate = .{ .ctx = &dummy, .required = required };
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

    fn readReply(c: *Client, arena: std.mem.Allocator) !Reply {
        c.reader.deadline = srv.deadlineIn(testing.io, 5000);
        const r = &c.reader.interface;
        var head: std.ArrayList(u8) = .empty;
        while (true) {
            const line = r.takeDelimiterInclusive('\n') catch |err| switch (err) {
                error.EndOfStream => return .{ .status = 0, .head = head.items, .body = "" },
                error.ReadFailed => return error.ReadFailed,
                error.StreamTooLong => return error.StreamTooLong,
            };
            try head.appendSlice(arena, line);
            if (line.len == 2) break;
        }
        const status = try std.fmt.parseInt(u16, head.items[9..12], 10);
        var reply: Reply = .{ .status = status, .head = head.items, .body = "" };
        if (reply.header("content-length")) |cl| {
            const n = try std.fmt.parseInt(usize, cl, 10);
            reply.body = try r.readAlloc(arena, n);
        }
        return reply;
    }
};

const Harness = struct {
    server: *srv.Server,
    fut: Io.Future(error{NotListening}!void),
    port: u16,
    stopped: bool = false,

    fn startGated(h: *Harness, limits: srv.Limits, gate: ?common.SetupGate) !void {
        h.server = try srv.Server.init(testing.allocator, testing.io, .{
            .keys = TestKeys.checker,
            .version = .{ .version = "1.2.3", .commit = "abc123", .zig_version = "0.17.0" },
            .limits = limits,
            .setup_gate = gate,
        });
        errdefer h.server.deinit();
        try web.register(&h.server.routes);
        h.port = try h.server.listen("127.0.0.1:0");
        h.fut = try testing.io.concurrent(srv.Server.serve, .{h.server});
        h.stopped = false;
    }

    fn stop(h: *Harness) !void {
        try h.fut.cancel(testing.io);
        h.stopped = true;
    }

    fn deinit(h: *Harness) void {
        if (!h.stopped) _ = h.stop() catch {};
        h.server.deinit();
    }

    fn roundTrip(h: *const Harness, arena: std.mem.Allocator, bytes: []const u8) !Reply {
        const c = try Client.open(h.port);
        defer c.close();
        try c.send(bytes);
        return c.readReply(arena);
    }
};

fn get(h: *const Harness, arena: std.mem.Allocator, path: []const u8, extra: []const u8) !Reply {
    const req = try std.fmt.allocPrint(arena, "GET {s} HTTP/1.1\r\nHost: t\r\nConnection: close\r\n{s}\r\n", .{ path, extra });
    return h.roundTrip(arena, req);
}

fn post(h: *const Harness, arena: std.mem.Allocator, path: []const u8, content_type: []const u8, body: []const u8) !Reply {
    const req = try std.fmt.allocPrint(arena, "POST {s} HTTP/1.1\r\nHost: t\r\nConnection: close\r\nContent-Type: {s}\r\nContent-Length: {d}\r\n\r\n{s}", .{ path, content_type, body.len, body });
    return h.roundTrip(arena, req);
}

fn newArena() std.heap.ArenaAllocator {
    return .init(testing.allocator);
}

test {
    _ = @import("web_origin.zig");
}

test "GET / while setup is open redirects to /setup" {
    TestGate.open.store(true, .release);
    var h: Harness = undefined;
    try h.startGated(.{}, TestGate.gate);
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();

    const r = try get(&h, arena.allocator(), "/", "");
    try testing.expectEqual(@as(u16, 303), r.status);
    try testing.expectEqualStrings("/setup", r.header("Location").?);
}

test "GET /login while setup is open redirects to /setup" {
    TestGate.open.store(true, .release);
    var h: Harness = undefined;
    try h.startGated(.{}, TestGate.gate);
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();

    const r = try get(&h, arena.allocator(), "/login", "");
    try testing.expectEqual(@as(u16, 303), r.status);
    try testing.expectEqualStrings("/setup", r.header("Location").?);
}

test "GET /setup while setup is open returns the setup form" {
    TestGate.open.store(true, .release);
    var h: Harness = undefined;
    try h.startGated(.{}, TestGate.gate);
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();

    const r = try get(&h, arena.allocator(), "/setup", "");
    try testing.expectEqual(@as(u16, 200), r.status);
    const ct = r.header("Content-Type") orelse return error.MissingContentType;
    try testing.expect(std.mem.indexOf(u8, ct, "text/html") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "id=\"setup-form\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "hx-post=\"/setup\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "hx-target=\"this\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "hx-swap=\"outerHTML\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "id=\"token\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "id=\"username\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "id=\"password\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "id=\"confirm\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "hx-on") == null);
    try testing.expect(std.mem.indexOf(u8, r.body, "javascript:") == null);
}

test "POST /setup with empty token returns 422" {
    TestGate.open.store(true, .release);
    var h: Harness = undefined;
    try h.startGated(.{}, TestGate.gate);
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();

    const body = "token=&username=owner&password=twelvechars%21&confirm=twelvechars%21";
    const r = try post(&h, arena.allocator(), "/setup", "application/x-www-form-urlencoded", body);
    try testing.expectEqual(@as(u16, 422), r.status);
    try testing.expect(std.mem.indexOf(u8, r.body, "Enter the setup token") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "id=\"setup-form\"") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "twelvechars") == null);
}

test "GET unknown page is HTML not JSON" {
    TestGate.open.store(false, .release);
    var h: Harness = undefined;
    try h.startGated(.{}, TestGate.gate);
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();

    const r = try get(&h, arena.allocator(), "/no/such/page", "");
    try testing.expectEqual(@as(u16, 404), r.status);
    const ct = r.header("Content-Type") orelse return error.MissingContentType;
    try testing.expect(std.mem.indexOf(u8, ct, "text/html") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "Page not found") != null);
    try testing.expect(std.mem.indexOf(u8, r.body, "\"not_found\"") == null);
}
