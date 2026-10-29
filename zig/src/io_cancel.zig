const std = @import("std");
const Io = std.Io;

// Reads from a connection that never sends anything: blocks until cancelled.
fn blockedRead(io: Io, stream: Io.net.Stream) anyerror!usize {
    var buf: [64]u8 = undefined;
    var r = stream.reader(io, &buf);
    var out: [16]u8 = undefined;
    return r.interface.readSliceShort(&out) catch |err| switch (err) {
        error.ReadFailed => return r.err.?, // the real cause: the std Reader reports ReadFailed only
        else => |e| return e,
    };
}

test "cancelling a task blocked in a socket read returns promptly" {
    const io = std.testing.io;
    const addr: Io.net.IpAddress = try .parse("127.0.0.1", 0);
    var server = try addr.listen(io, .{});
    defer server.deinit(io);
    const port = server.socket.address.getPort();

    const target: Io.net.IpAddress = try .parse("127.0.0.1", port);
    const client = try target.connect(io, .{ .mode = .stream });
    defer client.close(io);
    const peer = try server.accept(io);
    defer peer.close(io);

    var fut = try io.concurrent(blockedRead, .{ io, client });
    try io.sleep(.fromMilliseconds(100), .awake); // let it block in the read
    const t0 = Io.Clock.now(.awake, io);
    const res = fut.cancel(io);
    const waited = t0.durationTo(Io.Clock.now(.awake, io));
    std.debug.print("cancel result: {any}, waited {d} ms\n", .{ res, @divTrunc(waited.nanoseconds, std.time.ns_per_ms) });
    try std.testing.expectError(error.Canceled, res);
}
