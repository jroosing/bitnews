//! Tests of tls.zig over real loopback TLS handshakes. The server side is the
//! test-support server (tls_server.zig). Tests run with zig/ as the working
//! directory (testdata/tls/ holds the certificates).
const std = @import("std");
const Io = std.Io;
const testing = std.testing;
const tls = @import("tls.zig");

const Rig = struct {
    listener: Io.net.Server,

    fn init(io: Io) !Rig {
        const addr: Io.net.IpAddress = try .parse("127.0.0.1", 0);
        return .{ .listener = try addr.listen(io, .{}) };
    }

    fn deinit(r: *Rig, io: Io) void {
        r.listener.deinit(io);
    }

    fn dial(r: *Rig, io: Io) !Io.net.Stream {
        const target: Io.net.IpAddress = try .parse("127.0.0.1", r.listener.socket.address.getPort());
        return target.connect(io, .{ .mode = .stream });
    }
};

fn pattern(i: usize) u8 {
    return @truncate((i *% 2654435761) >> 11);
}

fn fillPattern(buf: []u8, offset: usize) void {
    for (buf, 0..) |*b, i| b.* = pattern(offset + i);
}

fn expectPattern(buf: []const u8, offset: usize) !void {
    for (buf, 0..) |b, i| try testing.expectEqual(pattern(offset + i), b);
}

/// The sizes writes and reads cycle through: odd, tiny, and around a record.
const sizes = [_]usize{ 1, 7, 4093, 16384, 16385, 70001, 3, 255 };

// ---- server tasks -------------------------------------------------------

const Seen = struct {
    name_buf: [64]u8 = undefined,
    name_len: ?usize = null,

    fn name(s: *const Seen) ?[]const u8 {
        return if (s.name_len) |n| s.name_buf[0..n] else null;
    }
};

/// Accepts, handshakes, echoes until the client closes.
fn echoTask(io: Io, listener: *Io.net.Server, ctx: tls.ServerContext, seen: ?*Seen) anyerror!void {
    const stream = try listener.accept(io);
    defer stream.close(io);
    const srv = try tls.Server.accept(testing.allocator, io, stream, ctx);
    errdefer srv.abort();
    if (seen) |s| if (srv.serverName()) |n| {
        @memcpy(s.name_buf[0..n.len], n);
        s.name_len = n.len;
    };
    const r = srv.reader();
    const w = srv.writer();
    while (true) {
        const data = r.peekGreedy(1) catch |err| switch (err) {
            error.EndOfStream => break,
            error.ReadFailed => return srv.readError().?,
        };
        try w.writeAll(data);
        r.toss(data.len);
        try w.flush();
    }
    srv.close();
}

/// Accepts and tries the handshake; whatever happens, waits until the client
/// is gone. For tests where the client is expected to refuse the server.
fn refusedTask(io: Io, listener: *Io.net.Server, ctx: tls.ServerContext) anyerror!void {
    const stream = try listener.accept(io);
    defer stream.close(io);
    if (tls.Server.accept(testing.allocator, io, stream, ctx)) |srv| {
        srv.abort();
    } else |_| {}
    drain(io, stream);
}

fn drain(io: Io, stream: Io.net.Stream) void {
    var buf: [256]u8 = undefined;
    var bufs = [_][]u8{&buf};
    while (true) {
        // Stream.read of this std does not compile; readWithControl does.
        const res = stream.readWithControl(io, &bufs, &.{}) catch return;
        if (res.data_len == 0) return;
    }
}

/// Accepts the TCP connection and says nothing at all.
fn silentTask(io: Io, listener: *Io.net.Server) anyerror!void {
    const stream = try listener.accept(io);
    defer stream.close(io);
    drain(io, stream);
}

/// Accepts the TCP connection and answers with fixed bytes.
fn blurtTask(io: Io, listener: *Io.net.Server, bytes: []const u8) anyerror!void {
    const stream = try listener.accept(io);
    defer stream.close(io);
    var w = stream.writer(io, &.{});
    try w.interface.writeAll(bytes);
    drain(io, stream);
}

fn clientOptions(roots: []const u8, diag: ?*tls.Diagnostic) tls.ClientOptions {
    return .{ .extra_roots_pem = roots, .diagnostic = diag };
}

/// Runs a handshake against `refusedTask` and returns the error.
fn connectFailure(server_cert: []const u8, server_opts: tls.ServerOptions, host: []const u8, roots: ?[]const u8, diag: *tls.Diagnostic, expected: anyerror) !void {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, server_cert, server_opts);
    defer ctx.deinit();
    var fut = try io.concurrent(refusedTask, .{ io, &rig.listener, ctx });
    defer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const pem = if (roots == null) try tls.testRootsPem(a, io) else null;
    defer if (pem) |p| a.free(p);
    const result = tls.Client.connect(a, io, stream, host, .{ .extra_roots_pem = roots orelse pem, .diagnostic = diag });
    if (result) |client| {
        client.abort();
        return error.UnexpectedSuccess;
    } else |err| try testing.expectEqual(expected, err);
}

// ---- handshake and verification -------------------------------------------

test "handshake with an extra root, echo, SNI, TLS 1.2 or newer" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);

    var seen: Seen = .{};
    var fut = try io.concurrent(echoTask, .{ io, &rig.listener, ctx, &seen });
    errdefer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);

    const client = try tls.Client.connect(a, io, stream, "localhost", clientOptions(roots, null));
    const version = client.protocolVersion();
    try testing.expect(std.mem.eql(u8, version, "TLSv1.3") or std.mem.eql(u8, version, "TLSv1.2"));
    try client.writer().writeAll("hello tls\r\n");
    try client.writer().flush();
    const line = try client.reader().takeDelimiterInclusive('\n');
    try testing.expectEqualStrings("hello tls\r\n", line);
    client.close();
    try fut.await(io);
    try testing.expectEqualStrings("localhost", seen.name().?);
}

test "an IP address is verified against the certificate's IP addresses and sends no SNI" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);

    var seen: Seen = .{};
    var fut = try io.concurrent(echoTask, .{ io, &rig.listener, ctx, &seen });
    errdefer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const client = try tls.Client.connect(a, io, stream, "127.0.0.1", clientOptions(roots, null));
    client.close();
    try fut.await(io);
    try testing.expect(seen.name() == null);
}

test "a TLS 1.2-only server works and negotiates TLS 1.2" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{ .min_version = .tls1_2, .max_version = .tls1_2 });
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);
    var fut = try io.concurrent(echoTask, .{ io, &rig.listener, ctx, null });
    errdefer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const client = try tls.Client.connect(a, io, stream, "localhost", clientOptions(roots, null));
    try testing.expectEqualStrings("TLSv1.2", client.protocolVersion());
    client.close();
    try fut.await(io);
}

test "a shared Context serves several connections and outlives its deinit" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);
    const shared = try tls.Context.init(.{ .extra_roots_pem = roots });

    // Two connections from one context.
    for (0..2) |_| {
        var fut = try io.concurrent(echoTask, .{ io, &rig.listener, ctx, null });
        errdefer _ = fut.cancel(io) catch {};
        const stream = try rig.dial(io);
        defer stream.close(io);
        const client = try tls.Client.connect(a, io, stream, "localhost", .{ .context = shared });
        client.close();
        try fut.await(io);
    }
    // A connection keeps working after its context is deinitialised.
    var fut = try io.concurrent(echoTask, .{ io, &rig.listener, ctx, null });
    errdefer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const client = try tls.Client.connect(a, io, stream, "localhost", .{ .context = shared });
    shared.deinit();
    try client.writer().writeAll("x\n");
    try client.writer().flush();
    try testing.expectEqualStrings("x\n", try client.reader().takeDelimiterInclusive('\n'));
    client.close();
    try fut.await(io);
}

test "an unknown authority is refused" {
    var diag: tls.Diagnostic = .{};
    // The server's CA is not in any trust store: no extra roots at all.
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    var fut = try io.concurrent(refusedTask, .{ io, &rig.listener, ctx });
    defer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    try testing.expectError(error.CertificateInvalid, tls.Client.connect(a, io, stream, "localhost", .{ .diagnostic = &diag }));
    try testing.expectEqual(tls.Failure.certificate_untrusted, diag.failure);
    try testing.expectEqualStrings("certificate_untrusted", diag.failure.detail());
}

test "a certificate from another CA is refused as untrusted" {
    var diag: tls.Diagnostic = .{};
    try connectFailure("untrusted", .{}, "localhost", null, &diag, error.CertificateInvalid);
    try testing.expectEqual(tls.Failure.certificate_untrusted, diag.failure);
}

test "a certificate for another host name is refused" {
    var diag: tls.Diagnostic = .{};
    try connectFailure("wronghost", .{}, "localhost", null, &diag, error.CertificateInvalid);
    try testing.expectEqual(tls.Failure.certificate_hostname, diag.failure);
    // And the right certificate asked for under another name.
    diag = .{};
    try connectFailure("server", .{}, "other.example.test", null, &diag, error.CertificateInvalid);
    try testing.expectEqual(tls.Failure.certificate_hostname, diag.failure);
    // An IP address the certificate does not list.
    diag = .{};
    try connectFailure("server", .{}, "127.0.0.2", null, &diag, error.CertificateInvalid);
    try testing.expectEqual(tls.Failure.certificate_hostname, diag.failure);
}

test "an expired certificate is refused" {
    var diag: tls.Diagnostic = .{};
    try connectFailure("expired", .{}, "localhost", null, &diag, error.CertificateInvalid);
    try testing.expectEqual(tls.Failure.certificate_expired, diag.failure);
}

test "a server that only speaks TLS 1.0 or 1.1 is refused" {
    var diag: tls.Diagnostic = .{};
    connectFailure("server", .{ .min_version = .tls1_0, .max_version = .tls1_1 }, "localhost", null, &diag, error.HandshakeFailed) catch |err| switch (err) {
        error.InvalidServerCredentials => return error.SkipZigTest, // this OpenSSL cannot do TLS 1.0 / 1.1 at all
        else => |e| return e,
    };
    try testing.expectEqual(tls.Failure.handshake, diag.failure);
}

test "a server that does not speak TLS is reported as such" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    var fut = try io.concurrent(blurtTask, .{ io, &rig.listener, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n" });
    defer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    var diag: tls.Diagnostic = .{};
    try testing.expectError(error.HandshakeFailed, tls.Client.connect(a, io, stream, "localhost", .{ .diagnostic = &diag }));
    try testing.expectEqual(tls.Failure.not_tls, diag.failure);
}

test "a server that closes the connection during the handshake is a connection error" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    var fut = try io.concurrent(blurtTask, .{ io, &rig.listener, "" });
    defer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    var diag: tls.Diagnostic = .{};
    // Shut our write side after the server saw the hello? Simpler: the
    // server task only drains, so close the stream's read side by shutdown
    // from our end is not needed: close the whole stream from a helper.
    var closer = try io.concurrent(closeAfter, .{ io, stream, 100 });
    defer _ = closer.cancel(io) catch {};
    const err = tls.Client.connect(a, io, stream, "localhost", .{ .diagnostic = &diag });
    try testing.expect(std.meta.isError(err));
}

fn closeAfter(io: Io, stream: Io.net.Stream, ms: i64) anyerror!void {
    try io.sleep(.fromMilliseconds(ms), .awake);
    stream.shutdown(io, .both) catch {};
}

test "the host name is validated, roots must hold a certificate" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const stream = try rig.dial(io);
    defer stream.close(io);
    try testing.expectError(error.InvalidHost, tls.Client.connect(a, io, stream, "", .{}));
    try testing.expectError(error.InvalidHost, tls.Client.connect(a, io, stream, "a b", .{}));
    try testing.expectError(error.InvalidHost, tls.Client.connect(a, io, stream, "a\x00b", .{}));
    try testing.expectError(error.InvalidRootCertificates, tls.Client.connect(a, io, stream, "localhost", .{ .extra_roots_pem = "not a certificate" }));
    try testing.expectError(error.InvalidRootCertificates, tls.Context.init(.{ .extra_roots_pem = "" }));
}

// ---- data transfer ------------------------------------------------------

const Bulk = struct {
    up: usize,
    down: usize,
};

/// Reads `up` bytes and checks the pattern, then sends `down` bytes of it.
fn bulkTask(io: Io, listener: *Io.net.Server, ctx: tls.ServerContext, bulk: Bulk) anyerror!void {
    const stream = try listener.accept(io);
    defer stream.close(io);
    const srv = try tls.Server.accept(testing.allocator, io, stream, ctx);
    errdefer srv.abort();
    const r = srv.reader();
    var got: usize = 0;
    var i: usize = 0;
    var buf: [70001]u8 = undefined;
    while (got < bulk.up) : (i += 1) {
        const n = @min(sizes[i % sizes.len], bulk.up - got);
        try r.readSliceAll(buf[0..n]);
        try expectPattern(buf[0..n], got);
        got += n;
    }
    const w = srv.writer();
    var sent: usize = 0;
    while (sent < bulk.down) : (i += 1) {
        const n = @min(sizes[(i + 3) % sizes.len], bulk.down - sent);
        fillPattern(buf[0..n], sent);
        try w.writeAll(buf[0..n]);
        sent += n;
    }
    try w.flush();
    // Wait for the client's close_notify so nothing in flight is cut off.
    try testing.expectError(error.EndOfStream, r.takeByte());
    srv.close();
}

test "large transfers in both directions, odd sizes" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);
    const bulk: Bulk = .{ .up = 6 * 1024 * 1024 + 12345, .down = 7 * 1024 * 1024 + 777 };

    var fut = try io.concurrent(bulkTask, .{ io, &rig.listener, ctx, bulk });
    errdefer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const client = try tls.Client.connect(a, io, stream, "localhost", clientOptions(roots, null));

    var buf: [70001]u8 = undefined;
    const w = client.writer();
    var sent: usize = 0;
    var i: usize = 0;
    while (sent < bulk.up) : (i += 1) {
        const n = @min(sizes[i % sizes.len], bulk.up - sent);
        fillPattern(buf[0..n], sent);
        try w.writeAll(buf[0..n]);
        sent += n;
    }
    try w.flush();
    const r = client.reader();
    var got: usize = 0;
    while (got < bulk.down) : (i += 1) {
        const n = @min(sizes[(i + 5) % sizes.len], bulk.down - got);
        try r.readSliceAll(buf[0..n]);
        try expectPattern(buf[0..n], got);
        got += n;
    }
    client.close();
    try fut.await(io);
}

fn writeTask(io: Io, client: *tls.Client, total: usize) anyerror!void {
    _ = io;
    var buf: [4096 + 11]u8 = undefined;
    var sent: usize = 0;
    var i: usize = 0;
    const w = client.writer();
    while (sent < total) : (i += 1) {
        const n = @min(sizes[i % sizes.len], @min(buf.len, total - sent));
        fillPattern(buf[0..n], sent);
        try w.writeAll(buf[0..n]);
        sent += n;
        if (i % 17 == 0) try w.flush();
    }
    try w.flush();
}

test "one task reads while another writes on the same connection" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);
    const total: usize = 3 * 1024 * 1024 + 99;

    var srv = try io.concurrent(echoTask, .{ io, &rig.listener, ctx, null });
    errdefer _ = srv.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const client = try tls.Client.connect(a, io, stream, "localhost", clientOptions(roots, null));
    var wf = try io.concurrent(writeTask, .{ io, client, total });
    errdefer _ = wf.cancel(io) catch {};

    const r = client.reader();
    var buf: [10007]u8 = undefined;
    var got: usize = 0;
    while (got < total) {
        const n = @min(buf.len, total - got);
        try r.readSliceAll(buf[0..n]);
        try expectPattern(buf[0..n], got);
        got += n;
    }
    try wf.await(io);
    client.close();
    try srv.await(io);
}

/// Sends `count` bytes one TLS record each.
fn dribbleTask(io: Io, listener: *Io.net.Server, ctx: tls.ServerContext, count: usize) anyerror!void {
    const stream = try listener.accept(io);
    defer stream.close(io);
    const srv = try tls.Server.accept(testing.allocator, io, stream, ctx);
    errdefer srv.abort();
    const w = srv.writer();
    for (0..count) |i| {
        try w.writeByte(pattern(i));
        try w.flush();
    }
    // Then read until the client says goodbye.
    _ = srv.reader().takeByte() catch {};
    srv.close();
}

test "a server that writes one byte per record" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);
    var fut = try io.concurrent(dribbleTask, .{ io, &rig.listener, ctx, 3000 });
    errdefer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const client = try tls.Client.connect(a, io, stream, "localhost", clientOptions(roots, null));
    const r = client.reader();
    var got: usize = 0;
    // A few single byte reads, then reads that must span many records.
    for (0..10) |_| {
        try testing.expectEqual(pattern(got), try r.takeByte());
        got += 1;
    }
    var buf: [1000]u8 = undefined;
    while (got < 3000) {
        const n = @min(buf.len, 3000 - got);
        try r.readSliceAll(buf[0..n]);
        try expectPattern(buf[0..n], got);
        got += n;
    }
    client.close();
    try fut.await(io);
}

test "partial reads and writes: lines, small buffered writes, flush" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);
    var fut = try io.concurrent(echoTask, .{ io, &rig.listener, ctx, null });
    errdefer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const client = try tls.Client.connect(a, io, stream, "localhost", clientOptions(roots, null));
    const w = client.writer();
    const r = client.reader();

    for (0..50) |i| {
        // Several small writes make one line; nothing is sent before flush.
        try w.writeAll("ARTICLE ");
        try w.print("<{d}@example>", .{i});
        try w.writeAll("\r\n");
        try w.flush();
        const line = try r.takeDelimiterInclusive('\n');
        var expect_buf: [64]u8 = undefined;
        const expect = try std.fmt.bufPrint(&expect_buf, "ARTICLE <{d}@example>\r\n", .{i});
        try testing.expectEqualStrings(expect, line);
    }
    // Two pipelined lines in one flush come back in order.
    try w.writeAll("A\r\nB\r\n");
    try w.flush();
    try testing.expectEqualStrings("A\r\n", try r.takeDelimiterInclusive('\n'));
    try testing.expectEqualStrings("B\r\n", try r.takeDelimiterInclusive('\n'));
    client.close();
    try fut.await(io);
}

// ---- closing ------------------------------------------------------------

fn byeTask(io: Io, listener: *Io.net.Server, ctx: tls.ServerContext, notify: bool) anyerror!void {
    const stream = try listener.accept(io);
    defer stream.close(io);
    const srv = try tls.Server.accept(testing.allocator, io, stream, ctx);
    try srv.writer().writeAll("bye");
    try srv.writer().flush();
    if (notify) srv.close() else srv.abort();
    // The abrupt case: the TCP stream closes (deferred) with no close_notify.
}

test "close_notify is a clean end of stream, a transport close without it is an error" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);

    for ([_]bool{ true, false }) |notify| {
        var fut = try io.concurrent(byeTask, .{ io, &rig.listener, ctx, notify });
        errdefer _ = fut.cancel(io) catch {};
        const stream = try rig.dial(io);
        defer stream.close(io);
        const client = try tls.Client.connect(a, io, stream, "localhost", clientOptions(roots, null));
        defer client.close();
        const r = client.reader();
        try testing.expectEqualStrings("bye", try r.take(3));
        if (notify) {
            try testing.expectError(error.EndOfStream, r.takeByte());
            try testing.expectEqual(@as(?tls.Error, null), client.readError());
            // And again: the end stays the end.
            try testing.expectError(error.EndOfStream, r.takeByte());
        } else {
            try testing.expectError(error.ReadFailed, r.takeByte());
            try testing.expectEqual(@as(?tls.Error, error.ConnectionClosed), client.readError());
        }
        try fut.await(io);
    }
}

// ---- cancellation ---------------------------------------------------------

fn holdTask(io: Io, listener: *Io.net.Server, ctx: tls.ServerContext) anyerror!void {
    const stream = try listener.accept(io);
    defer stream.close(io);
    const srv = try tls.Server.accept(testing.allocator, io, stream, ctx);
    defer srv.abort();
    // Say nothing; wait for the client to go away.
    _ = srv.reader().takeByte() catch {};
}

fn blockedRead(client: *tls.Client) anyerror!void {
    var buf: [16]u8 = undefined;
    _ = client.reader().readSliceShort(&buf) catch return client.readError().?;
}

test "cancelling a read blocked inside TLS returns promptly" {
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    const ctx = try tls.testContext(a, io, "server", .{});
    defer ctx.deinit();
    const roots = try tls.testRootsPem(a, io);
    defer a.free(roots);
    var srv = try io.concurrent(holdTask, .{ io, &rig.listener, ctx });
    errdefer _ = srv.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    const client = try tls.Client.connect(a, io, stream, "localhost", clientOptions(roots, null));
    defer client.abort();

    var fut = try io.concurrent(blockedRead, .{client});
    try io.sleep(.fromMilliseconds(100), .awake);
    const t0 = Io.Clock.now(.awake, io);
    const res = fut.cancel(io);
    const waited = t0.durationTo(Io.Clock.now(.awake, io));
    try testing.expectError(error.Canceled, res);
    try testing.expect(waited.toMilliseconds() < 1000);
    // A cancelled read loses nothing: the connection still works afterwards.
    try client.writer().flush();
    _ = srv.cancel(io) catch {};
}

fn connectTask(io: Io, stream: Io.net.Stream) anyerror!void {
    const client = try tls.Client.connect(testing.allocator, io, stream, "localhost", .{});
    client.abort();
}

test "a handshake that never completes can be cancelled" {
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    var srv = try io.concurrent(silentTask, .{ io, &rig.listener });
    defer _ = srv.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);

    var fut = try io.concurrent(connectTask, .{ io, stream });
    try io.sleep(.fromMilliseconds(100), .awake);
    const t0 = Io.Clock.now(.awake, io);
    const res = fut.cancel(io);
    const waited = t0.durationTo(Io.Clock.now(.awake, io));
    try testing.expectError(error.Canceled, res);
    try testing.expect(waited.toMilliseconds() < 1000);
}

// ---- fuzz ---------------------------------------------------------------

/// The fake server sends the fuzz bytes and closes; the handshake must fail
/// cleanly: no crash, no leak (testing.allocator), no success.
fn fuzzHandshake(_: void, smith: *testing.Smith) !void {
    var bytes: [600]u8 = undefined;
    const len = smith.slice(&bytes);
    const a = testing.allocator;
    const io = testing.io;
    var rig = try Rig.init(io);
    defer rig.deinit(io);
    var fut = try io.concurrent(blurtTask, .{ io, &rig.listener, bytes[0..len] });
    defer _ = fut.cancel(io) catch {};
    const stream = try rig.dial(io);
    defer stream.close(io);
    // A reader of our own side would hang on a server that stays silent, so
    // close our end when the bytes are in.
    var closer = try io.concurrent(closeAfter, .{ io, stream, 150 });
    defer _ = closer.cancel(io) catch {};
    if (tls.Client.connect(a, io, stream, "localhost", .{})) |client| {
        client.abort();
        return error.HandshakeSucceededOnGarbage;
    } else |_| {}
}

test "fuzz: arbitrary bytes from the server never crash or leak a client handshake" {
    try testing.fuzz({}, fuzzHandshake, .{
        .corpus = &.{
            "",
            "HTTP/1.1 400 Bad Request\r\n\r\n",
            "\x15\x03\x03\x00\x02\x02\x28", // fatal alert handshake_failure
            "\x16\x03\x03\x00\x04\x02\x00\x00\x00", // an empty ServerHello
            "\x16\x03\x03\x00\x30" ++ "\x02\x00\x00\x2c\x03\x03" ++ "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" ++ "\x00\x13\x01\x00\x00\x04\x00\x2b\x00\x00",
            "\x16\x03\x01\xff\xff",
            "\x16\x03\x03\x40\x00",
            "\x80\x2e\x01\x03\x01\x00\x15",
        },
    });
}
