//! The seams under the NNTP connection: a `Transport` (a secured byte stream
//! with a reader, a writer and a close) produced by a `Dialer`.
//!
//! The Dialer is where TLS happens (`nntp_tls.zig`) and where the lead plugs
//! the network-interface binding (VPN kill switch): a bound Dialer fails
//! rather than going out unbound. Production code only ever asks a Dialer for
//! a secure Transport; there is no flag for plain text. Tests dial a plain
//! loopback socket through the same seam (`nntptest.zig`).
//!
//! This file also holds the parts every Dialer needs: `connectTcp` (DNS and
//! TCP connect with a timeout and classified errors) and `TimedStream`, a
//! socket reader/writer with an idle timeout and a command deadline, which the
//! threaded `Io` lacks.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const net = Io.net;
const proto = @import("nntp_proto.zig");

/// Why a dial failed, in the classes the connection test reports.
pub const DialError = error{
    Canceled,
    OutOfMemory,
    DnsNotFound,
    DnsOther,
    TcpRefused,
    TcpTimeout,
    TcpUnreachable,
    TcpOther,
    TlsCertUntrusted,
    TlsCertHostname,
    TlsCertExpired,
    TlsCertInvalid,
    TlsNotTls,
    TlsTimeout,
    TlsHandshake,
};

pub const DialRequest = struct {
    /// The DNS name or IP of the Server; also the name its certificate must
    /// match.
    host: []const u8,
    port: u16,
    /// Bounds the TCP connect plus the TLS handshake.
    connect_timeout_ms: u32,
    /// The idle timeout of every read and write on the Transport.
    read_timeout_ms: u32,
};

pub const Failure = enum { none, timeout, canceled, io };

/// A connected, secured stream. The reader's buffer holds at least
/// `proto.min_read_buffer` bytes. Not safe for concurrent use.
pub const Transport = struct {
    ctx: *anyopaque,
    reader: *Io.Reader,
    writer: *Io.Writer,
    vtable: *const VTable,

    pub const VTable = struct {
        /// A command starts: its reads and writes end at the idle timeout, or
        /// at `deadline` if that comes first (null: no hard deadline).
        begin: *const fn (ctx: *anyopaque, deadline: ?Io.Clock.Timestamp) void,
        /// The command made progress (a line arrived): restarts the idle
        /// timeout of transports that cannot enforce it per read.
        touch: *const fn (ctx: *anyopaque) void,
        /// The command is over.
        end: *const fn (ctx: *anyopaque) void,
        /// Why the last read or write failed.
        failure: *const fn (ctx: *anyopaque) Failure,
        /// Closes the connection without a goodbye. Idempotent.
        shutdown: *const fn (ctx: *anyopaque, io: Io) void,
        /// Shuts down if needed and frees the Transport.
        destroy: *const fn (ctx: *anyopaque, io: Io) void,
    };

    pub fn begin(t: Transport, deadline: ?Io.Clock.Timestamp) void {
        t.vtable.begin(t.ctx, deadline);
    }

    pub fn touch(t: Transport) void {
        t.vtable.touch(t.ctx);
    }

    pub fn end(t: Transport) void {
        t.vtable.end(t.ctx);
    }

    pub fn failure(t: Transport) Failure {
        return t.vtable.failure(t.ctx);
    }

    pub fn shutdown(t: Transport, io: Io) void {
        t.vtable.shutdown(t.ctx, io);
    }

    pub fn destroy(t: Transport, io: Io) void {
        t.vtable.destroy(t.ctx, io);
    }
};

/// Opens secure Transports. The Transport is allocated with `gpa`.
pub const Dialer = struct {
    ctx: *anyopaque,
    dial_fn: *const fn (ctx: *anyopaque, gpa: Allocator, io: Io, req: DialRequest) DialError!Transport,

    pub fn dial(d: Dialer, gpa: Allocator, io: Io, req: DialRequest) DialError!Transport {
        return d.dial_fn(d.ctx, gpa, io, req);
    }
};

// ---- TCP connect -----------------------------------------------------------

const Racer = union(enum) {
    connect: net.HostName.ConnectError!net.Stream,
    timer: Io.Cancelable!void,
};

fn connectTask(io: Io, host: net.HostName, port: u16) net.HostName.ConnectError!net.Stream {
    return host.connect(io, port, .{ .mode = .stream });
}

fn timerTask(io: Io, ms: u32) Io.Cancelable!void {
    return io.sleep(.fromMilliseconds(ms), .awake);
}

/// Resolves `host` and connects, giving up after `timeout_ms`. The threaded
/// `Io` has no connect timeout of its own, so the connect races a timer.
pub fn connectTcp(io: Io, host: []const u8, port: u16, timeout_ms: u32) DialError!net.Stream {
    const name = net.HostName.init(host) catch return error.DnsNotFound;
    var buf: [2]Racer = undefined;
    var sel: Io.Select(Racer) = .init(io, &buf);
    sel.concurrent(.connect, connectTask, .{ io, name, port }) catch return error.TcpOther;
    sel.concurrent(.timer, timerTask, .{ io, timeout_ms }) catch {
        discard(io, &sel);
        return error.TcpOther;
    };
    const first = sel.await() catch {
        discard(io, &sel);
        return error.Canceled;
    };
    switch (first) {
        .connect => |r| {
            // The timer is left; nothing it returns needs freeing.
            sel.cancelDiscard();
            return r catch |err| return classifyConnect(err);
        },
        .timer => {
            discard(io, &sel);
            return error.TcpTimeout;
        },
    }
}

/// Cancels what is left of the race and closes a stream that was connected
/// after all.
fn discard(io: Io, sel: *Io.Select(Racer)) void {
    while (sel.cancel()) |left| switch (left) {
        .connect => |r| if (r) |s| s.close(io) else |_| {},
        .timer => {},
    };
}

fn classifyConnect(err: net.HostName.ConnectError) DialError {
    return switch (err) {
        error.Canceled => error.Canceled,
        error.UnknownHostName, error.NoAddressReturned => error.DnsNotFound,
        error.ResolvConfParseFailed,
        error.InvalidDnsARecord,
        error.InvalidDnsAAAARecord,
        error.InvalidDnsCnameRecord,
        error.NameServerFailure,
        error.DetectingNetworkConfigurationFailed,
        => error.DnsOther,
        error.ConnectionRefused => error.TcpRefused,
        error.Timeout => error.TcpTimeout,
        error.HostUnreachable, error.NetworkUnreachable, error.NetworkDown => error.TcpUnreachable,
        else => error.TcpOther,
    };
}

// ---- sockets with deadlines ------------------------------------------------

/// One socket operation that gives up at `deadline` with error.Timeout.
fn timedOperate(io: Io, op: Io.Operation, deadline: Io.Clock.Timestamp) !Io.Operation.Result {
    var storage: [1]Io.Operation.Storage = undefined;
    var batch: Io.Batch = .init(&storage);
    defer batch.cancel(io);
    batch.addAt(0, op);
    try batch.awaitConcurrent(io, .{ .deadline = deadline });
    return batch.next().?.result;
}

/// A plain socket as a `Transport`: every read and write ends at the earlier
/// of now + the idle timeout and the hard deadline. Plain text on its own;
/// the TLS dialer must wrap it or use the stream underneath, and tests use it
/// on loopback.
pub const TimedStream = struct {
    gpa: Allocator,
    stream: net.Stream,
    idle_ms: u32,
    hard_deadline: ?Io.Clock.Timestamp = null,
    err: ?anyerror = null,
    closed: bool = false,
    io: Io,
    reader: Io.Reader,
    writer: Io.Writer,
    read_buf: []u8,
    write_buf: []u8,

    const write_buffer_size = 1024;

    /// Takes over `stream`: on failure it is closed.
    pub fn create(gpa: Allocator, io: Io, stream: net.Stream, idle_ms: u32) Allocator.Error!Transport {
        errdefer stream.close(io);
        const self = try gpa.create(TimedStream);
        errdefer gpa.destroy(self);
        const rb = try gpa.alloc(u8, 2 * proto.min_read_buffer);
        errdefer gpa.free(rb);
        const wb = try gpa.alloc(u8, write_buffer_size);
        self.* = .{
            .gpa = gpa,
            .stream = stream,
            .idle_ms = idle_ms,
            .io = io,
            .read_buf = rb,
            .write_buf = wb,
            .reader = .{ .vtable = &.{ .stream = streamImpl, .readVec = readVec }, .buffer = rb, .seek = 0, .end = 0 },
            .writer = .{ .vtable = &.{ .drain = drain }, .buffer = wb },
        };
        return .{ .ctx = self, .reader = &self.reader, .writer = &self.writer, .vtable = &vtable };
    }

    const vtable: Transport.VTable = .{
        .begin = beginCommand,
        .touch = touch,
        .end = endCommand,
        .failure = failure,
        .shutdown = shutdown,
        .destroy = destroy,
    };

    fn from(ctx: *anyopaque) *TimedStream {
        return @ptrCast(@alignCast(ctx));
    }

    fn beginCommand(ctx: *anyopaque, deadline: ?Io.Clock.Timestamp) void {
        from(ctx).hard_deadline = deadline;
    }

    fn touch(ctx: *anyopaque) void {
        _ = ctx; // every read gets its own idle deadline already
    }

    fn endCommand(ctx: *anyopaque) void {
        from(ctx).hard_deadline = null;
    }

    fn failure(ctx: *anyopaque) Failure {
        const e = from(ctx).err orelse return .none;
        return if (e == error.Timeout) .timeout else if (e == error.Canceled) .canceled else .io;
    }

    fn shutdown(ctx: *anyopaque, io: Io) void {
        const self = from(ctx);
        if (self.closed) return;
        self.closed = true;
        self.stream.close(io);
    }

    fn destroy(ctx: *anyopaque, io: Io) void {
        const self = from(ctx);
        shutdown(ctx, io);
        self.gpa.free(self.read_buf);
        self.gpa.free(self.write_buf);
        self.gpa.destroy(self);
    }

    /// The earlier of now + idle timeout and the hard deadline.
    fn nextDeadline(self: *TimedStream) Io.Clock.Timestamp {
        const idle = Io.Clock.Timestamp.now(self.io, .awake).addDuration(.{ .raw = .fromMilliseconds(self.idle_ms), .clock = .awake });
        const hard = self.hard_deadline orelse return idle;
        return if (hard.raw.nanoseconds < idle.raw.nanoseconds) hard else idle;
    }

    fn streamImpl(io_r: *Io.Reader, io_w: *Io.Writer, limit: Io.Limit) Io.Reader.StreamError!usize {
        const dest = limit.slice(try io_w.writableSliceGreedy(1));
        var data: [1][]u8 = .{dest};
        const n = try readVec(io_r, &data);
        io_w.advance(n);
        return n;
    }

    fn readVec(io_r: *Io.Reader, data: [][]u8) Io.Reader.Error!usize {
        const self: *TimedStream = @alignCast(@fieldParentPtr("reader", io_r));
        if (self.closed) {
            self.err = error.ConnectionResetByPeer;
            return error.ReadFailed;
        }
        var iovecs: [8][]u8 = undefined;
        const dest_n, const data_size = try io_r.writableVector(&iovecs, data);
        const op: Io.Operation = .{ .net_read = .{ .socket_handle = self.stream.socket.handle, .data = iovecs[0..dest_n] } };
        const result = timedOperate(self.io, op, self.nextDeadline()) catch |err| {
            self.err = err;
            return error.ReadFailed;
        };
        const read = result.net_read catch |err| {
            self.err = err;
            return error.ReadFailed;
        };
        if (read.data_len == 0) return error.EndOfStream;
        if (read.data_len > data_size) {
            io_r.end += read.data_len - data_size;
            return data_size;
        }
        return read.data_len;
    }

    fn drain(io_w: *Io.Writer, data: []const []const u8, splat: usize) Io.Writer.Error!usize {
        const self: *TimedStream = @alignCast(@fieldParentPtr("writer", io_w));
        if (self.closed) {
            self.err = error.ConnectionResetByPeer;
            return error.WriteFailed;
        }
        const op: Io.Operation = .{ .net_write = .{
            .socket_handle = self.stream.socket.handle,
            .header = io_w.buffered(),
            .data = data,
            .splat = splat,
        } };
        const result = timedOperate(self.io, op, self.nextDeadline()) catch |err| {
            self.err = err;
            return error.WriteFailed;
        };
        const n = result.net_write catch |err| {
            self.err = err;
            return error.WriteFailed;
        };
        return io_w.consume(n);
    }
};

const testing = std.testing;

test "connectTcp: refused, unresolvable and timed-out connects are told apart" {
    const io = testing.io;
    // A port nobody listens on: bind, note the port, close.
    const any: net.IpAddress = try .parse("127.0.0.1", 0);
    var l = try any.listen(io, .{});
    const port = l.socket.address.getPort();
    l.deinit(io);
    try testing.expectError(error.TcpRefused, connectTcp(io, "127.0.0.1", port, 5000));
    try testing.expectError(error.DnsNotFound, connectTcp(io, "bad host name", 563, 5000));
    try testing.expectError(error.DnsNotFound, connectTcp(io, "", 563, 5000));
}

test "connectTcp reaches a listener and the TimedStream carries bytes both ways" {
    const io = testing.io;
    const any: net.IpAddress = try .parse("127.0.0.1", 0);
    var l = try any.listen(io, .{});
    defer l.deinit(io);
    const port = l.socket.address.getPort();

    const s = try connectTcp(io, "127.0.0.1", port, 5000);
    const t = try TimedStream.create(testing.allocator, io, s, 5000);
    defer t.destroy(io);
    const peer = try l.accept(io);
    defer peer.close(io);

    try t.writer.writeAll("hello\r\n");
    try t.writer.flush();
    var pbuf: [64]u8 = undefined;
    var pr = peer.reader(io, &pbuf);
    try testing.expectEqualStrings("hello\r\n", try pr.interface.takeDelimiterInclusive('\n'));
    var pw_buf: [64]u8 = undefined;
    var pw = peer.writer(io, &pw_buf);
    try pw.interface.writeAll("200 hi\r\n");
    try pw.interface.flush();
    try testing.expectEqualStrings("200 hi\r\n", try t.reader.takeDelimiterInclusive('\n'));
}

test "TimedStream: a silent peer ends the read at the idle timeout, a hard deadline can come first" {
    const io = testing.io;
    const any: net.IpAddress = try .parse("127.0.0.1", 0);
    var l = try any.listen(io, .{});
    defer l.deinit(io);
    const s = try connectTcp(io, "127.0.0.1", l.socket.address.getPort(), 5000);
    const t = try TimedStream.create(testing.allocator, io, s, 150);
    defer t.destroy(io);
    const peer = try l.accept(io);
    defer peer.close(io);

    var t0 = Io.Clock.now(.awake, io);
    try testing.expectError(error.ReadFailed, t.reader.takeDelimiterInclusive('\n'));
    try testing.expectEqual(Failure.timeout, t.failure());
    var waited = t0.durationTo(Io.Clock.now(.awake, io)).toMilliseconds();
    try testing.expect(waited >= 100 and waited < 3000);

    // A hard deadline earlier than the idle timeout wins.
    const idle_long = try TimedStream.create(testing.allocator, io, try connectTcp(io, "127.0.0.1", l.socket.address.getPort(), 5000), 60_000);
    defer idle_long.destroy(io);
    const peer2 = try l.accept(io);
    defer peer2.close(io);
    idle_long.begin(Io.Clock.Timestamp.now(io, .awake).addDuration(.{ .raw = .fromMilliseconds(100), .clock = .awake }));
    t0 = Io.Clock.now(.awake, io);
    try testing.expectError(error.ReadFailed, idle_long.reader.takeDelimiterInclusive('\n'));
    waited = t0.durationTo(Io.Clock.now(.awake, io)).toMilliseconds();
    try testing.expect(waited < 3000);
    try testing.expectEqual(Failure.timeout, idle_long.failure());
}

test "a closed peer reads as end of stream, not as a failure" {
    const io = testing.io;
    const any: net.IpAddress = try .parse("127.0.0.1", 0);
    var l = try any.listen(io, .{});
    defer l.deinit(io);
    const s = try connectTcp(io, "127.0.0.1", l.socket.address.getPort(), 5000);
    const t = try TimedStream.create(testing.allocator, io, s, 5000);
    defer t.destroy(io);
    const peer = try l.accept(io);
    peer.close(io);
    try testing.expectError(error.EndOfStream, t.reader.takeDelimiterInclusive('\n'));
    try testing.expectEqual(Failure.none, t.failure());
}
