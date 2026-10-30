//! End-to-end tests of the event stream: a real server on 127.0.0.1:0 with a
//! real bus, talked to over raw sockets (internal/api/events_test.go).
const std = @import("std");
const Io = std.Io;
const net = Io.net;
const testing = std.testing;
const srv = @import("http_server.zig");
const common = @import("http_common.zig");
const events_api = @import("events_api.zig");
const bus_mod = @import("events_bus.zig");

const key = "bitnews_testkey0123456789";
const request = "GET /api/v1/events HTTP/1.1\r\nHost: t\r\nAuthorization: Bearer " ++ key ++ "\r\n\r\n";
const hello = "event: hello\ndata: {\"instance_id\":\"instance-1\"}\n\n";

const TestKeys = struct {
    fn check(_: *anyopaque, presented: []const u8) bool {
        return std.mem.eql(u8, presented, key);
    }
    var dummy: u8 = 0;
    const checker: common.KeyChecker = .{ .ctx = &dummy, .check = check };
};

const Harness = struct {
    bus: *bus_mod.Bus,
    events: events_api.Events,
    server: *srv.Server,
    fut: Io.Future(error{NotListening}!void),
    port: u16,
    stopped: bool = false,

    const Config = struct {
        buffer: usize = 256,
        limits: srv.Limits = .{},
        ping_interval_ms: u32 = 15_000,
    };

    fn start(h: *Harness, cfg: Config) !void {
        h.bus = try bus_mod.Bus.init(testing.allocator, testing.io, .{ .buffer = cfg.buffer });
        errdefer h.bus.deinit();
        h.events = .{ .bus = h.bus, .instance_id = "instance-1", .ping_interval_ms = cfg.ping_interval_ms, .poll_ms = 50 };
        h.server = try srv.Server.init(testing.allocator, testing.io, .{ .keys = TestKeys.checker, .limits = cfg.limits });
        errdefer h.server.deinit();
        try events_api.register(&h.server.routes, &h.events);
        h.port = try h.server.listen("127.0.0.1:0");
        h.fut = try testing.io.concurrent(srv.Server.serve, .{h.server});
        h.stopped = false;
    }

    fn stop(h: *Harness) !i64 {
        const t0 = Io.Clock.Timestamp.now(testing.io, .awake);
        try h.fut.cancel(testing.io);
        h.stopped = true;
        return t0.durationTo(Io.Clock.Timestamp.now(testing.io, .awake)).raw.toMilliseconds();
    }

    fn deinit(h: *Harness) void {
        if (!h.stopped) _ = h.stop() catch {};
        h.server.deinit();
        h.bus.deinit();
    }

    fn live(h: *Harness) usize {
        h.bus.mutex.lockUncancelable(testing.io);
        defer h.bus.mutex.unlock(testing.io);
        return h.bus.live;
    }

    /// Waits until exactly `n` subscriptions are left, or fails after 5 s.
    fn expectLive(h: *Harness, n: usize) !void {
        for (0..500) |_| {
            if (h.live() == n) return;
            try testing.io.sleep(.fromMilliseconds(10), .awake);
        }
        try testing.expectEqual(n, h.live());
    }

    fn connect(h: *const Harness) !*Client {
        return Client.open(h.port);
    }

    /// Opens a stream and reads its head and hello.
    fn subscribe(h: *const Harness, arena: std.mem.Allocator) !*Client {
        const c = try h.connect();
        errdefer c.close();
        try c.send(request);
        const head = try c.readHead(arena);
        try testing.expect(std.mem.startsWith(u8, head, "HTTP/1.1 200"));
        try testing.expectEqualStrings(hello, (try c.readChunk(arena)).?);
        return c;
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

    fn arm(c: *Client, ms: u32) void {
        c.reader.deadline = srv.deadlineIn(testing.io, ms);
        c.reader.err = null;
    }

    fn failure(c: *Client) error{ ClientTimeout, ReadFailed } {
        if (c.reader.err) |cause| if (cause == error.Timeout) return error.ClientTimeout;
        return error.ReadFailed;
    }

    /// The status line and headers, up to and including the blank line.
    fn readHead(c: *Client, arena: std.mem.Allocator) ![]const u8 {
        c.arm(5000);
        var head: std.ArrayList(u8) = .empty;
        while (true) {
            const line = c.reader.interface.takeDelimiterInclusive('\n') catch |err| switch (err) {
                error.ReadFailed => return c.failure(),
                else => |e| return e,
            };
            try head.appendSlice(arena, line);
            if (line.len == 2) return head.items;
        }
    }

    /// The next chunk of a chunked body; null at the last chunk or when the
    /// server closed the connection.
    fn readChunk(c: *Client, arena: std.mem.Allocator) !?[]u8 {
        return c.readChunkWithin(arena, 5000);
    }

    fn readChunkWithin(c: *Client, arena: std.mem.Allocator, ms: u32) !?[]u8 {
        c.arm(ms);
        const r = &c.reader.interface;
        const line = r.takeDelimiterInclusive('\n') catch |err| switch (err) {
            error.EndOfStream => return null,
            error.ReadFailed => {
                if (c.reader.err) |cause| if (cause == error.ConnectionResetByPeer) return null;
                return c.failure();
            },
            else => |e| return e,
        };
        const size = try std.fmt.parseInt(usize, std.mem.trim(u8, line, "\r\n"), 16);
        if (size == 0) {
            // The blank line that ends the trailers.
            r.discardAll(2) catch {};
            return null;
        }
        const data = r.readAlloc(arena, size) catch |err| switch (err) {
            error.ReadFailed => return c.failure(),
            else => |e| return e,
        };
        r.discardAll(2) catch |err| switch (err) {
            error.ReadFailed => return c.failure(),
            else => |e| return e,
        };
        return data;
    }

    fn closedByServer(c: *Client, within_ms: u32) !bool {
        c.arm(within_ms);
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
};

fn newArena() std.heap.ArenaAllocator {
    return .init(testing.allocator);
}

test "the stream needs the app key" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();

    const anonymous = try h.connect();
    defer anonymous.close();
    try anonymous.send("GET /api/v1/events HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n");
    try testing.expect(std.mem.startsWith(u8, try anonymous.readHead(arena.allocator()), "HTTP/1.1 401"));

    const bad = try h.connect();
    defer bad.close();
    try bad.send("GET /api/v1/events HTTP/1.1\r\nHost: t\r\nConnection: close\r\nAuthorization: Bearer bn_nope\r\n\r\n");
    try testing.expect(std.mem.startsWith(u8, try bad.readHead(arena.allocator()), "HTTP/1.1 401"));
    try h.expectLive(0);
}

test "the stream starts with the headers and a hello carrying the instance ID" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();

    const c = try h.connect();
    defer c.close();
    try c.send(request);
    const head = try c.readHead(a);

    try testing.expect(std.mem.startsWith(u8, head, "HTTP/1.1 200 OK\r\n"));
    try testing.expect(std.ascii.findIgnoreCase(head, "\r\ncontent-type: text/event-stream\r\n") != null);
    try testing.expect(std.ascii.findIgnoreCase(head, "\r\ncache-control: no-cache\r\n") != null);
    try testing.expect(std.ascii.findIgnoreCase(head, "\r\nx-accel-buffering: no\r\n") != null);
    try testing.expect(std.ascii.findIgnoreCase(head, "\r\nx-content-type-options: nosniff\r\n") != null);
    try testing.expect(std.ascii.findIgnoreCase(head, "\r\ntransfer-encoding: chunked\r\n") != null);
    try testing.expectEqualStrings(hello, (try c.readChunk(a)).?);
}

test "every event kind arrives with the payload the Go daemon sends" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const c = try h.subscribe(a);
    defer c.close();

    const d: @import("download.zig").Download = .{
        .id = "abc",
        .name = "Some.Show",
        .source_kind = .nzb,
        .state = .queued,
        .queue_position = 2,
        .total_bytes = 1000,
        .added_at = 1793365901873,
        .updated_at = 1793365901000,
    };
    const p = h.bus.publisher();
    p.publish(.{ .download_added = d });
    p.publish(.{ .download_updated = d });
    p.publish(.{ .download_progress = .{ .id = "dl1", .done_bytes = 50, .total_bytes = 200, .bytes_per_second = 7 } });
    p.publish(.{ .fetched = "dl1" });
    p.publish(.{ .ready = .{ .id = "dl1", .output_path = "/data/out" } });
    p.publish(.{ .failed = .{ .id = "dl1", .reason = .missing_articles, .detail = "12 articles <a&b> \xe2\x80\xa8 \"q\" \\ \n \x01 \xff end" } });
    p.publish(.{ .removed = "dl1" });
    p.publish(.settings_changed);
    p.publish(.servers_changed);
    p.publish(.{ .queue_paused = .{ .source = .nzb, .reason = "disk_full", .detail = "10 bytes free, 20 needed" } });
    p.publish(.{ .queue_resumed = .nzb });
    p.publish(.{ .queue_paused = .{ .source = .torrent, .reason = "bind_unavailable" } });
    p.publish(.{ .queue_resumed = .torrent });

    const snapshot = "{\"id\":\"abc\",\"name\":\"Some.Show\",\"source_kind\":\"nzb\",\"category_id\":null,\"state\":\"queued\"," ++
        "\"priority\":\"normal\",\"queue_position\":2,\"paused\":false,\"total_bytes\":1000,\"done_bytes\":0," ++
        "\"added_at\":\"2026-10-30T13:11:41.873Z\",\"fetched_at\":null,\"ready_at\":null," ++
        "\"updated_at\":\"2026-10-30T13:11:41Z\"}";
    // The payloads are the output of the Go encoder for the same values.
    const want = [_][]const u8{
        "event: download_added\ndata: " ++ snapshot ++ "\n\n",
        "event: download_updated\ndata: " ++ snapshot ++ "\n\n",
        "event: download_progress\ndata: {\"id\":\"dl1\",\"done_bytes\":50,\"total_bytes\":200,\"bytes_per_second\":7}\n\n",
        "event: fetched\ndata: {\"id\":\"dl1\"}\n\n",
        "event: ready\ndata: {\"id\":\"dl1\",\"output_path\":\"/data/out\"}\n\n",
        "event: failed\ndata: {\"id\":\"dl1\",\"reason\":\"missing_articles\",\"detail\":\"12 articles \\u003ca\\u0026b\\u003e \\u2028 \\\"q\\\" \\\\ \\n \\u0001 \xef\xbf\xbd end\"}\n\n",
        "event: removed\ndata: {\"id\":\"dl1\"}\n\n",
        "event: settings_changed\ndata: {}\n\n",
        "event: servers_changed\ndata: {}\n\n",
        "event: usenet_queue_paused\ndata: {\"reason\":\"disk_full\",\"detail\":\"10 bytes free, 20 needed\"}\n\n",
        "event: usenet_queue_resumed\ndata: {\"reason\":\"\",\"detail\":\"\"}\n\n",
        "event: torrent_queue_paused\ndata: {\"reason\":\"bind_unavailable\",\"detail\":\"\"}\n\n",
        "event: torrent_queue_resumed\ndata: {\"reason\":\"\",\"detail\":\"\"}\n\n",
    };
    for (want) |frame| try testing.expectEqualStrings(frame, (try c.readChunk(a)).?);
}

test "events from before connecting are not replayed and Last-Event-ID is ignored" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    h.bus.publish(.{ .removed = "old" });

    const c = try h.connect();
    defer c.close();
    try c.send("GET /api/v1/events HTTP/1.1\r\nHost: t\r\nLast-Event-ID: 5\r\nAuthorization: Bearer " ++ key ++ "\r\n\r\n");
    _ = try c.readHead(a);
    try testing.expectEqualStrings(hello, (try c.readChunk(a)).?);
    h.bus.publish(.{ .removed = "new" });

    try testing.expectEqualStrings("event: removed\ndata: {\"id\":\"new\"}\n\n", (try c.readChunk(a)).?);
}

test "an idle stream gets a ping comment" {
    var h: Harness = undefined;
    try h.start(.{ .ping_interval_ms = 100 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const c = try h.subscribe(a);
    defer c.close();

    try testing.expectEqualStrings(": ping\n\n", (try c.readChunk(a)).?);
    try testing.expectEqualStrings(": ping\n\n", (try c.readChunk(a)).?);
}

test "the stream outlives the server's whole-response write timeout" {
    var h: Harness = undefined;
    try h.start(.{ .limits = .{ .write_timeout_ms = 150, .read_timeout_ms = 150 } });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const c = try h.subscribe(a);
    defer c.close();

    try testing.io.sleep(.fromMilliseconds(500), .awake);
    h.bus.publish(.{ .removed = "late" });

    try testing.expectEqualStrings("event: removed\ndata: {\"id\":\"late\"}\n\n", (try c.readChunk(a)).?);
}

test "a client that falls too far behind gets resync and the stream ends" {
    var h: Harness = undefined;
    try h.start(.{ .buffer = 2 });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const c = try h.subscribe(a);
    defer c.close();

    for (0..2000) |_| h.bus.publish(.{ .removed = "x" });

    var last: []const u8 = "";
    var frames: usize = 0;
    while (try c.readChunk(a)) |frame| {
        last = frame;
        frames += 1;
    }
    try testing.expectEqualStrings("event: resync\ndata: {}\n\n", last);
    try testing.expect(frames < 2000);
    try h.expectLive(0);
}

test "a client that hangs up releases its subscription" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    var clients: [3]*Client = undefined;
    for (&clients) |*slot| slot.* = try h.subscribe(arena.allocator());
    try testing.expectEqual(3, h.live());

    for (clients) |c| c.close();

    try h.expectLive(0);
    h.bus.publish(.settings_changed); // nobody left, must not block
}

test "shutdown ends open streams within the grace period" {
    var h: Harness = undefined;
    try h.start(.{ .limits = .{ .shutdown_grace_ms = 5000 } });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const one = try h.subscribe(a);
    defer one.close();
    const two = try h.subscribe(a);
    defer two.close();

    const took = try h.stop();

    try testing.expect(took < 1500);
    try testing.expectEqual(null, try one.readChunk(a));
    try testing.expectEqual(null, try two.readChunk(a));
    try testing.expect(try one.closedByServer(1000));
    try h.expectLive(0);
}

test "streams count against the connection cap" {
    var h: Harness = undefined;
    try h.start(.{ .limits = .{ .max_connections = 2 } });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const one = try h.subscribe(a);
    const two = try h.subscribe(a);
    defer two.close();

    const third = try h.connect();
    defer third.close();
    try third.send(request);
    try testing.expect(std.mem.startsWith(u8, try third.readHead(a), "HTTP/1.1 503"));

    one.close();
    try h.expectLive(1);
    var served = false;
    for (0..50) |_| {
        const again = try h.connect();
        defer again.close();
        try again.send(request);
        const head = try again.readHead(a);
        if (std.mem.startsWith(u8, head, "HTTP/1.1 200")) {
            served = true;
            break;
        }
        try testing.io.sleep(.fromMilliseconds(50), .awake);
    }
    try testing.expect(served);
}

test "a client that stops reading does not block the bus or the other clients" {
    var h: Harness = undefined;
    try h.start(.{ .buffer = 8, .limits = .{ .stream_write_timeout_ms = 300 } });
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const a = arena.allocator();
    const stalled = try h.subscribe(a);
    defer stalled.close();
    const reader = try h.subscribe(a);
    defer reader.close();

    const big = try a.alloc(u8, 60 * 1024);
    @memset(big, 'x');
    const t0 = Io.Clock.Timestamp.now(testing.io, .awake);
    for (0..40) |_| {
        h.bus.publish(.{ .failed = .{ .id = "dl1", .reason = .internal, .detail = big } });
        var arena2 = newArena();
        defer arena2.deinit();
        const frame = (try reader.readChunk(arena2.allocator())).?;
        try testing.expect(std.mem.startsWith(u8, frame, "event: failed\n"));
    }
    // Publishing and the healthy reader were never held up by the stalled one.
    try testing.expect(t0.durationTo(Io.Clock.Timestamp.now(testing.io, .awake)).raw.toMilliseconds() < 3000);

    // The stalled stream is cut by its write timeout and gives its slot back.
    try h.expectLive(1);
}

test "HEAD answers the headers and does not stream" {
    var h: Harness = undefined;
    try h.start(.{});
    defer h.deinit();
    var arena = newArena();
    defer arena.deinit();
    const c = try h.connect();
    defer c.close();
    try c.send("HEAD /api/v1/events HTTP/1.1\r\nHost: t\r\nConnection: close\r\nAuthorization: Bearer " ++ key ++ "\r\n\r\n");
    const head = try c.readHead(arena.allocator());
    try testing.expect(std.mem.startsWith(u8, head, "HTTP/1.1 200"));
    try testing.expect(std.ascii.findIgnoreCase(head, "text/event-stream") != null);
    try testing.expectEqual(0, h.live());
}
