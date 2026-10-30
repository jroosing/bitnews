//! The real Dialer: TCP connect (`nntp_transport.connectTcp`), then a strict
//! TLS client handshake (tls.zig: certificate chain and host name verified,
//! TLS 1.2 minimum, no way to weaken any of it). The lead wraps or replaces
//! the TCP step with the network-interface binding (VPN kill switch): that
//! step is `connectTcp` below.
//!
//! The connect timeout covers the TCP connect plus the handshake. The TLS
//! engine has no deadlines of its own, so the idle timeout and the command
//! deadline of the connection are enforced by a watchdog task per connection:
//! it looks at the progress the connection reports (`touch`) and, when none
//! comes within the idle timeout (or the hard deadline passes), shuts the
//! socket down, which fails the blocked read or write.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const net = Io.net;
const tls = @import("tls.zig");
const transport = @import("nntp_transport.zig");

const Transport = transport.Transport;
const DialError = transport.DialError;

pub const TlsDialer = struct {
    context: tls.Context,

    /// One OpenSSL context (trust store, protocol limits) for all the
    /// connections of this dialer. `extra_roots_pem` is for tests; production
    /// passes null and trusts the system store.
    pub fn init(options: tls.ContextOptions) tls.Error!TlsDialer {
        return .{ .context = try tls.Context.init(options) };
    }

    pub fn deinit(d: *TlsDialer) void {
        d.context.deinit();
    }

    /// The returned Dialer points at `d`: keep `d` where it is.
    pub fn dialer(d: *TlsDialer) transport.Dialer {
        return .{ .ctx = d, .dial_fn = dial };
    }

    fn dial(ctx: *anyopaque, gpa: Allocator, io: Io, req: transport.DialRequest) DialError!Transport {
        const self: *TlsDialer = @ptrCast(@alignCast(ctx));
        const t0 = Io.Clock.now(.awake, io);
        const stream = try transport.connectTcp(io, req.host, req.port, req.connect_timeout_ms);
        errdefer stream.close(io);
        const elapsed: u64 = @intCast(@max(0, t0.durationTo(Io.Clock.now(.awake, io)).toMilliseconds()));
        const remaining: u32 = if (elapsed >= req.connect_timeout_ms) 1 else @intCast(req.connect_timeout_ms - elapsed);

        const client = try handshake(gpa, io, stream, req.host, self.context, remaining);
        errdefer client.abort();
        return TlsTransport.create(gpa, io, stream, client, req.read_timeout_ms);
    }
};

// ---- the handshake with a deadline ---------------------------------------------------

const Racer = union(enum) {
    handshake: tls.Error!*tls.Client,
    timer: Io.Cancelable!void,
};

const Handshake = struct {
    gpa: Allocator,
    io: Io,
    stream: net.Stream,
    host: []const u8,
    context: tls.Context,
    diag: tls.Diagnostic = .{},
};

fn handshakeTask(h: *Handshake) tls.Error!*tls.Client {
    return tls.Client.connect(h.gpa, h.io, h.stream, h.host, .{ .context = h.context, .diagnostic = &h.diag });
}

fn timerTask(io: Io, ms: u32) Io.Cancelable!void {
    return io.sleep(.fromMilliseconds(ms), .awake);
}

fn handshake(gpa: Allocator, io: Io, stream: net.Stream, host: []const u8, context: tls.Context, timeout_ms: u32) DialError!*tls.Client {
    var h: Handshake = .{ .gpa = gpa, .io = io, .stream = stream, .host = host, .context = context };
    var buf: [2]Racer = undefined;
    var sel: Io.Select(Racer) = .init(io, &buf);
    sel.concurrent(.handshake, handshakeTask, .{&h}) catch return error.OutOfMemory;
    sel.concurrent(.timer, timerTask, .{ io, timeout_ms }) catch {
        discard(&sel);
        return error.OutOfMemory;
    };
    const first = sel.await() catch {
        discard(&sel);
        return error.Canceled;
    };
    switch (first) {
        .handshake => |r| {
            sel.cancelDiscard();
            return r catch |err| return classify(err, h.diag.failure);
        },
        .timer => {
            discard(&sel);
            return error.TlsTimeout;
        },
    }
}

/// Cancels what is left of the race and frees a client that was connected
/// after all.
fn discard(sel: *Io.Select(Racer)) void {
    while (sel.cancel()) |left| switch (left) {
        .handshake => |r| if (r) |c| c.abort() else |_| {},
        .timer => {},
    };
}

fn classify(err: tls.Error, failure: tls.Failure) DialError {
    return switch (err) {
        error.Canceled => error.Canceled,
        error.OutOfMemory => error.OutOfMemory,
        error.CertificateInvalid => switch (failure) {
            .certificate_untrusted => error.TlsCertUntrusted,
            .certificate_hostname => error.TlsCertHostname,
            .certificate_expired => error.TlsCertExpired,
            else => error.TlsCertInvalid,
        },
        error.HandshakeFailed => if (failure == .not_tls) error.TlsNotTls else error.TlsHandshake,
        error.InvalidHost => error.DnsNotFound,
        error.ConnectionClosed,
        error.TlsFailure,
        error.NetworkFailure,
        error.InvalidRootCertificates,
        error.InvalidServerCredentials,
        => error.TlsHandshake,
    };
}

// ---- the transport ---------------------------------------------------------------------

const TlsTransport = struct {
    gpa: Allocator,
    io: Io,
    stream: net.Stream,
    client: *tls.Client,
    idle_ns: i64,
    poll_ms: u32,
    watchdog: ?Io.Future(void) = null,
    /// A command is running (written by the owner task, read by the watchdog).
    active: std.atomic.Value(bool) = .init(false),
    last_progress: std.atomic.Value(i64) = .init(0),
    /// Hard deadline in `awake` nanoseconds, 0 for none.
    hard_ns: std.atomic.Value(i64) = .init(0),
    timed_out: std.atomic.Value(bool) = .init(false),
    closed: bool = false,

    /// Takes over `client` (aborted on failure); `stream` stays the caller's
    /// to close on failure, the transport's afterwards.
    fn create(gpa: Allocator, io: Io, stream: net.Stream, client: *tls.Client, idle_ms: u32) DialError!Transport {
        const self = gpa.create(TlsTransport) catch return error.OutOfMemory;
        errdefer gpa.destroy(self);
        self.* = .{
            .gpa = gpa,
            .io = io,
            .stream = stream,
            .client = client,
            .idle_ns = @as(i64, idle_ms) * std.time.ns_per_ms,
            .poll_ms = std.math.clamp(idle_ms / 4, 10, 250),
        };
        self.watchdog = io.concurrent(watch, .{self}) catch return error.OutOfMemory;
        return .{ .ctx = self, .reader = client.reader(), .writer = client.writer(), .vtable = &vtable };
    }

    const vtable: Transport.VTable = .{
        .begin = begin,
        .touch = touch,
        .end = end,
        .failure = failure,
        .shutdown = shutdown,
        .destroy = destroy,
    };

    fn from(ctx: *anyopaque) *TlsTransport {
        return @ptrCast(@alignCast(ctx));
    }

    fn now(self: *const TlsTransport) i64 {
        return @intCast(Io.Clock.Timestamp.now(self.io, .awake).raw.nanoseconds);
    }

    fn begin(ctx: *anyopaque, deadline: ?Io.Clock.Timestamp) void {
        const self = from(ctx);
        self.hard_ns.store(if (deadline) |d| @intCast(d.raw.nanoseconds) else 0, .release);
        self.last_progress.store(self.now(), .release);
        self.active.store(true, .release);
    }

    fn touch(ctx: *anyopaque) void {
        const self = from(ctx);
        self.last_progress.store(self.now(), .release);
    }

    fn end(ctx: *anyopaque) void {
        from(ctx).active.store(false, .release);
    }

    fn failure(ctx: *anyopaque) transport.Failure {
        const self = from(ctx);
        if (self.timed_out.load(.acquire)) return .timeout;
        const re = self.client.readError();
        const we = self.client.writeError();
        if ((re != null and re.? == error.Canceled) or (we != null and we.? == error.Canceled)) return .canceled;
        return .io;
    }

    /// Wakes up a few times per idle timeout; when the running command has
    /// made no progress for the idle timeout, or its hard deadline passed,
    /// shuts the socket down so the blocked read or write fails.
    fn watch(self: *TlsTransport) void {
        while (true) {
            self.io.sleep(.fromMilliseconds(self.poll_ms), .awake) catch return;
            if (!self.active.load(.acquire)) continue;
            const t = self.now();
            var limit = self.last_progress.load(.acquire) + self.idle_ns;
            const hard = self.hard_ns.load(.acquire);
            if (hard != 0 and hard < limit) limit = hard;
            if (t >= limit) {
                self.timed_out.store(true, .release);
                self.stream.shutdown(self.io, .both) catch {};
                return;
            }
        }
    }

    fn stopWatchdog(self: *TlsTransport) void {
        if (self.watchdog) |*w| {
            w.cancel(self.io);
            self.watchdog = null;
        }
    }

    fn shutdown(ctx: *anyopaque, io: Io) void {
        const self = from(ctx);
        if (self.closed) return;
        self.closed = true;
        self.stopWatchdog();
        self.stream.close(io);
    }

    fn destroy(ctx: *anyopaque, io: Io) void {
        const self = from(ctx);
        if (self.closed) {
            // The socket is gone: no close_notify possible.
            self.client.abort();
        } else {
            self.closed = true;
            self.stopWatchdog();
            self.client.close(); // close_notify; the writer was flushed
            self.stream.close(io);
        }
        const gpa = self.gpa;
        gpa.destroy(self);
    }
};

const testing = std.testing;
const scripted = @import("nntp_scripted.zig");
const nntptest = @import("nntptest.zig");
const conn_mod = @import("nntp_conn.zig");
const diag_mod = @import("nntp_diag.zig");
const types = @import("usenet_types.zig");
const pool_mod = @import("nntp_pool.zig");

const tio = testing.io;

const Collect = struct {
    lines: usize = 0,
    fn sink(c: *Collect) types.Sink {
        return .{ .ctx = c, .line_fn = take };
    }
    fn take(ctx: *anyopaque, _: []const u8) error{Abort}!void {
        const c: *Collect = @ptrCast(@alignCast(ctx));
        c.lines += 1;
    }
};

const Fixture = struct {
    srv: *nntptest.Server,
    roots: []u8,
    dialer: TlsDialer,

    fn start(cert: []const u8) !*Fixture {
        const f = try testing.allocator.create(Fixture);
        errdefer testing.allocator.destroy(f);
        f.roots = try tls.testRootsPem(testing.allocator, tio);
        errdefer testing.allocator.free(f.roots);
        const ctx = try tls.testContext(testing.allocator, tio, cert, .{});
        errdefer ctx.deinit();
        f.srv = try nntptest.Server.startTls(testing.allocator, tio, ctx);
        errdefer f.srv.stop();
        f.dialer = try TlsDialer.init(.{ .extra_roots_pem = f.roots });
        return f;
    }

    fn stop(f: *Fixture) void {
        f.dialer.deinit();
        f.srv.stop();
        testing.allocator.free(f.roots);
        testing.allocator.destroy(f);
    }

    fn connect(f: *Fixture, diag: *diag_mod.Diag) conn_mod.OpenError!*conn_mod.Conn {
        return conn_mod.Conn.connect(testing.allocator, tio, f.dialer.dialer(), f.srv.config(), diag);
    }
};

test "an article is fetched over strict TLS" {
    const f = try Fixture.start("server");
    defer f.stop();
    try f.srv.addArticle("a@x", &.{ "one", ".two", "three" });
    var d: diag_mod.Diag = .{};
    const c = try f.connect(&d);
    defer c.close(tio);
    var got: Collect = .{};
    try c.body(tio, "a@x", got.sink());
    try c.body(tio, "a@x", got.sink());
    try testing.expectEqual(@as(usize, 6), got.lines);
    try testing.expect(!c.isDead());
    try testing.expectError(error.ArticleNotFound, c.body(tio, "nobody@x", got.sink()));
}

test "certificate problems are told apart" {
    const Case = struct { cert: []const u8, err: DialError, cause: diag_mod.Cause };
    for ([_]Case{
        .{ .cert = "wronghost", .err = error.TlsCertHostname, .cause = .tls_cert_hostname },
        .{ .cert = "expired", .err = error.TlsCertExpired, .cause = .tls_cert_expired },
        .{ .cert = "untrusted", .err = error.TlsCertUntrusted, .cause = .tls_cert_untrusted },
    }) |case| {
        const f = try Fixture.start(case.cert);
        defer f.stop();
        var d: diag_mod.Diag = .{};
        try testing.expectError(error.Connection, f.connect(&d));
        try testing.expectEqual(diag_mod.Op.tls, d.op);
        try testing.expectEqual(case.cause, d.cause);
    }
}

test "a server that speaks plain text on the port is not TLS" {
    const sc = try scripted.Script.start(testing.allocator, tio, .{ .handler = struct {
        fn run(_: ?*anyopaque, s: *scripted.Session) void {
            s.line("200 plain text hello");
            s.stall();
        }
    }.run });
    defer sc.stop();
    var td = try TlsDialer.init(.{});
    defer td.deinit();
    var d: diag_mod.Diag = .{};
    const cfg: conn_mod.Config = .{ .host = "127.0.0.1", .port = sc.port(), .connect_timeout_ms = 3000, .read_timeout_ms = 3000 };
    try testing.expectError(error.Connection, conn_mod.Conn.connect(testing.allocator, tio, td.dialer(), cfg, &d));
    try testing.expectEqual(diag_mod.Op.tls, d.op);
    try testing.expect(d.cause == .tls_not_tls or d.cause == .tls_handshake);
}

test "a handshake that never answers ends at the connect timeout" {
    const sc = try scripted.Script.start(testing.allocator, tio, .{ .handler = struct {
        fn run(_: ?*anyopaque, s: *scripted.Session) void {
            s.stall();
        }
    }.run });
    defer sc.stop();
    var td = try TlsDialer.init(.{});
    defer td.deinit();
    var d: diag_mod.Diag = .{};
    const cfg: conn_mod.Config = .{ .host = "127.0.0.1", .port = sc.port(), .connect_timeout_ms = 300, .read_timeout_ms = 3000 };
    const t0 = Io.Clock.now(.awake, tio);
    try testing.expectError(error.Connection, conn_mod.Conn.connect(testing.allocator, tio, td.dialer(), cfg, &d));
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, tio)).toMilliseconds() < 3000);
    try testing.expectEqual(diag_mod.Cause.tls_timeout, d.cause);
}

test "a refused connection is a TCP error, not a TLS one" {
    const any: net.IpAddress = try .parse("127.0.0.1", 0);
    var l = try any.listen(tio, .{});
    const port = l.socket.address.getPort();
    l.deinit(tio);
    var td = try TlsDialer.init(.{});
    defer td.deinit();
    var d: diag_mod.Diag = .{};
    try testing.expectError(error.Connection, conn_mod.Conn.connect(testing.allocator, tio, td.dialer(), .{ .host = "127.0.0.1", .port = port }, &d));
    try testing.expectEqual(diag_mod.Cause.tcp_refused, d.cause);
}

test "over TLS a stalled body ends at the idle timeout" {
    const f = try Fixture.start("server");
    defer f.stop();
    try f.srv.addArticle("a@x", &.{ "one", "two" });
    f.srv.setLatencyMs(2000);
    var cfg = f.srv.config();
    cfg.read_timeout_ms = 200;
    var d: diag_mod.Diag = .{};
    const c = try conn_mod.Conn.connect(testing.allocator, tio, f.dialer.dialer(), cfg, &d);
    defer c.close(tio);
    var got: Collect = .{};
    const t0 = Io.Clock.now(.awake, tio);
    try testing.expectError(error.Connection, c.body(tio, "a@x", got.sink()));
    const waited = t0.durationTo(Io.Clock.now(.awake, tio)).toMilliseconds();
    try testing.expect(waited >= 150 and waited < 1500);
    try testing.expectEqual(diag_mod.Cause.timeout, c.diag.cause);
    try testing.expect(c.isDead());
}

test "over TLS a command deadline bounds a trickling command" {
    const f = try Fixture.start("server");
    defer f.stop();
    f.srv.setLatencyMs(3000);
    var d: diag_mod.Diag = .{};
    const c = try f.connect(&d);
    defer c.close(tio);
    c.command_deadline = Io.Clock.Timestamp.now(tio, .awake).addDuration(.{ .raw = .fromMilliseconds(250), .clock = .awake });
    const t0 = Io.Clock.now(.awake, tio);
    try testing.expectError(error.Connection, c.date(tio));
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, tio)).toMilliseconds() < 2000);
    try testing.expectEqual(diag_mod.Cause.timeout, c.diag.cause);
}

fn bodyTask(c: *conn_mod.Conn, got: *Collect) conn_mod.BodyError!void {
    return c.body(tio, "a@x", got.sink());
}

test "cancelling the task aborts a blocked TLS read at once" {
    const f = try Fixture.start("server");
    defer f.stop();
    try f.srv.addArticle("a@x", &.{"one"});
    f.srv.setLatencyMs(5000);
    var d: diag_mod.Diag = .{};
    const c = try f.connect(&d);
    defer c.close(tio);
    var got: Collect = .{};
    var fut = try tio.concurrent(bodyTask, .{ c, &got });
    try tio.sleep(.fromMilliseconds(100), .awake);
    const t0 = Io.Clock.now(.awake, tio);
    try testing.expectError(error.Canceled, fut.cancel(tio));
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, tio)).toMilliseconds() < 1000);
    try testing.expect(c.isDead());
}

test "a pool works over TLS" {
    const f = try Fixture.start("server");
    defer f.stop();
    try f.srv.addArticle("a@x", &.{ "one", "two" });
    const p = try pool_mod.Pool.create(testing.allocator, tio, .{ .name = "tls", .config = f.srv.config(), .dialer = f.dialer.dialer(), .max_connections = 2 });
    defer p.destroy();
    var got: Collect = .{};
    try p.fetcher().body(tio, "a@x", got.sink());
    try p.fetcher().body(tio, "a@x", got.sink());
    try testing.expectEqual(@as(usize, 4), got.lines);
    try testing.expectEqual(@as(usize, 1), f.srv.dials());
}
