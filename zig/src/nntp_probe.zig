//! The connection test of a Server (internal/app/prober.go, the part that
//! belongs to the nntp package): connect, authenticate, run DATE, and report
//! a safe outcome. The class and detail tokens and the messages are Go's, so
//! the API and the web UI show the same words. A result never contains
//! credentials.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const conn_mod = @import("nntp_conn.zig");
const diag_mod = @import("nntp_diag.zig");
const transport = @import("nntp_transport.zig");

const Diag = diag_mod.Diag;

/// Which step of a connection test failed.
pub const Class = enum {
    ok,
    dns,
    tcp,
    tls,
    auth,
    protocol,

    pub fn name(c: Class) []const u8 {
        return @tagName(c);
    }
};

/// Bounds of a whole test; the connect and read timeouts are shorter.
pub const Options = struct {
    probe_timeout_ms: u32 = 20_000,
    connect_timeout_ms: u32 = 10_000,
    read_timeout_ms: u32 = 10_000,
};

pub const Target = struct {
    host: []const u8,
    port: u16 = conn_mod.default_port,
    user: []const u8 = "",
    pass: []const u8 = "",
};

pub const Result = struct {
    ok: bool = false,
    class: Class = .protocol,
    /// A stable machine-readable refinement of `class`, empty for ok:
    ///   dns:      not_found, timeout, other
    ///   tcp:      refused, timeout, unreachable, other
    ///   tls:      certificate_untrusted, certificate_hostname,
    ///             certificate_expired, certificate_invalid, not_tls,
    ///             timeout, handshake
    ///   auth:     rejected
    ///   protocol: greeting, auth_response, date, other
    detail: []const u8 = "",
    message_buf: [320]u8 = undefined,
    message_len: u16 = 0,
    /// Dial, TLS handshake, greeting and authentication, and the round trip
    /// of the DATE command; both 0 unless the connection worked.
    connect_ns: i64 = 0,
    latency_ns: i64 = 0,

    /// A short human-readable explanation.
    pub fn message(r: *const Result) []const u8 {
        return r.message_buf[0..r.message_len];
    }

    fn fail(class: Class, detail: []const u8, msg: []const u8) Result {
        var r: Result = .{ .class = class, .detail = detail };
        r.setMessage("{s}", .{msg});
        return r;
    }

    fn setMessage(r: *Result, comptime fmt: []const u8, args: anytype) void {
        const out = std.fmt.bufPrint(&r.message_buf, fmt, args) catch r.message_buf[0..r.message_buf.len];
        r.message_len = @intCast(out.len);
    }
};

/// Tests a connection to `target`. Never fails because of the Server: a
/// problem is the returned result. Only cancellation and out-of-memory are
/// errors.
pub fn probe(gpa: Allocator, io: Io, dialer: transport.Dialer, target: Target, options: Options) error{ Canceled, OutOfMemory }!Result {
    const start = Io.Clock.now(.awake, io);
    const cfg: conn_mod.Config = .{
        .host = target.host,
        .port = target.port,
        .user = target.user,
        .pass = target.pass,
        .connect_timeout_ms = options.connect_timeout_ms,
        .read_timeout_ms = options.read_timeout_ms,
        .deadline = Io.Clock.Timestamp.now(io, .awake).addDuration(.{ .raw = .fromMilliseconds(options.probe_timeout_ms), .clock = .awake }),
    };
    var diag: Diag = .{};
    const conn = conn_mod.Conn.connect(gpa, io, dialer, cfg, &diag) catch |err| switch (err) {
        error.Canceled => return error.Canceled,
        error.OutOfMemory => return error.OutOfMemory,
        error.InvalidConfig => return Result.fail(.protocol, "other", "the connection test failed"),
        error.Auth, error.Connection, error.Protocol => return classify(&diag),
    };
    defer conn.close(io);
    const connected = Io.Clock.now(.awake, io);
    _ = conn.date(io) catch |err| switch (err) {
        error.Canceled => return error.Canceled,
        error.Connection, error.Protocol => return classify(&conn.diag),
    };
    const done = Io.Clock.now(.awake, io);
    var r: Result = .{ .ok = true, .class = .ok };
    r.setMessage("connected and authenticated", .{});
    r.connect_ns = @intCast(start.durationTo(connected).toNanoseconds());
    r.latency_ns = @intCast(connected.durationTo(done).toNanoseconds());
    return r;
}

/// Maps a failure to a result (Go: classifyProbeError). The diagnostic holds
/// no credentials, so nothing needs scrubbing here.
pub fn classify(d: *const Diag) Result {
    switch (d.op) {
        .dial => return classifyDial(d.cause),
        .tls => return classifyTls(d.cause),
        else => {},
    }
    if (d.kind == .auth) {
        var r: Result = .{ .class = .auth, .detail = "rejected" };
        r.setMessage("the server rejected the user name or password ({d})", .{d.code});
        return r;
    }
    const detail: []const u8 = switch (d.op) {
        .greeting => "greeting",
        .date => "date",
        .auth => "auth_response",
        else => "other",
    };
    var r: Result = .{ .class = .protocol, .detail = detail };
    if (d.code != 0) {
        if (d.text().len > 0) {
            r.setMessage("unexpected response from the server ({d} {s})", .{ d.code, d.text() });
        } else r.setMessage("unexpected response from the server ({d})", .{d.code});
    } else if (d.cause.isTimeout()) {
        r.setMessage("the server did not answer in time", .{});
    } else r.setMessage("unexpected response from the server", .{});
    return r;
}

fn classifyDial(cause: diag_mod.Cause) Result {
    return switch (cause) {
        .dns_not_found => Result.fail(.dns, "not_found", "the host name does not exist"),
        .dns_timeout => Result.fail(.dns, "timeout", "the host name lookup timed out"),
        .dns_other => Result.fail(.dns, "other", "the host name could not be resolved"),
        .tcp_refused => Result.fail(.tcp, "refused", "the connection was refused: check the host and port"),
        .tcp_timeout, .timeout => Result.fail(.tcp, "timeout", "the connection timed out"),
        .tcp_unreachable => Result.fail(.tcp, "unreachable", "the host cannot be reached"),
        else => Result.fail(.tcp, "other", "could not connect to the server"),
    };
}

fn classifyTls(cause: diag_mod.Cause) Result {
    return switch (cause) {
        .tls_cert_untrusted => Result.fail(.tls, "certificate_untrusted", "the server's certificate is not trusted"),
        .tls_cert_hostname => Result.fail(.tls, "certificate_hostname", "the server's certificate is not valid for this host name"),
        .tls_cert_expired => Result.fail(.tls, "certificate_expired", "the server's certificate has expired or is not valid yet"),
        .tls_cert_invalid => Result.fail(.tls, "certificate_invalid", "the server's certificate is not valid"),
        .tls_not_tls => Result.fail(.tls, "not_tls", "the server does not speak TLS on this port: bitnews only connects over TLS"),
        .tls_timeout, .timeout => Result.fail(.tls, "timeout", "the TLS handshake timed out"),
        else => Result.fail(.tls, "handshake", "the TLS handshake failed"),
    };
}

// ---- tests ------------------------------------------------------------------------------

const testing = std.testing;
const nntptest = @import("nntptest.zig");
const scripted = @import("nntp_scripted.zig");
const nntp_tls = @import("nntp_tls.zig");
const tls = @import("tls.zig");

const tio = testing.io;

fn targetOf(srv: *nntptest.Server) Target {
    const c = srv.config();
    return .{ .host = c.host, .port = c.port, .user = c.user, .pass = c.pass };
}

fn expectFail(r: Result, class: Class, detail: []const u8) !void {
    try testing.expect(!r.ok);
    try testing.expectEqual(class, r.class);
    try testing.expectEqualStrings(detail, r.detail);
}

test "a working server is ok, with timings" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    srv.setLatencyMs(20);
    const r = try probe(testing.allocator, tio, srv.dialer(), targetOf(srv), .{});
    try testing.expect(r.ok);
    try testing.expectEqual(Class.ok, r.class);
    try testing.expectEqualStrings("", r.detail);
    try testing.expectEqualStrings("connected and authenticated", r.message());
    try testing.expect(r.connect_ns > 0);
    try testing.expect(r.latency_ns >= 20 * std.time.ns_per_ms);
    try testing.expectEqual(@as(usize, 1), srv.authAttempts());
}

test "rejected credentials are auth/rejected with the code and without the credentials" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.requireAuth("alice", "hunter2-secret");
    var t = targetOf(srv);
    t.pass = "wrong";
    const r = try probe(testing.allocator, tio, srv.dialer(), t, .{});
    try expectFail(r, .auth, "rejected");
    try testing.expectEqualStrings("the server rejected the user name or password (481)", r.message());
}

test "a refused port is tcp/refused" {
    const any: Io.net.IpAddress = try .parse("127.0.0.1", 0);
    var l = try any.listen(tio, .{});
    const port = l.socket.address.getPort();
    l.deinit(tio);
    var td = try nntp_tls.TlsDialer.init(.{});
    defer td.deinit();
    const r = try probe(testing.allocator, tio, td.dialer(), .{ .host = "127.0.0.1", .port = port }, .{});
    try expectFail(r, .tcp, "refused");
    try testing.expectEqualStrings("the connection was refused: check the host and port", r.message());
}

test "an unknown host is dns/not_found" {
    var td = try nntp_tls.TlsDialer.init(.{});
    defer td.deinit();
    const r = try probe(testing.allocator, tio, td.dialer(), .{ .host = "bad host name" }, .{});
    try expectFail(r, .dns, "not_found");
}

test "a busy server is protocol/greeting with its code and text" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    srv.setMaxConnections(1);
    var hd: Diag = .{};
    const hold = try conn_mod.Conn.connect(testing.allocator, tio, srv.dialer(), srv.config(), &hd);
    defer hold.close(tio);
    const r = try probe(testing.allocator, tio, srv.dialer(), targetOf(srv), .{});
    try expectFail(r, .protocol, "greeting");
    try testing.expectEqualStrings("unexpected response from the server (502 Too many connections)", r.message());
}

test "server text that echoes the credentials is scrubbed" {
    const sc = try scripted.Script.start(testing.allocator, tio, .{ .handler = struct {
        fn run(_: ?*anyopaque, s: *scripted.Session) void {
            s.line("200 hi");
            _ = s.readCmd();
            s.line("381 pass");
            _ = s.readCmd();
            s.line("502 sorry alice, hunter2-secret failed");
        }
    }.run });
    defer sc.stop();
    const r = try probe(testing.allocator, tio, sc.dialer(), .{ .host = "h", .port = sc.port(), .user = "alice", .pass = "hunter2-secret" }, .{});
    try expectFail(r, .protocol, "auth_response");
    try testing.expectEqualStrings("unexpected response from the server (502 sorry ***, *** failed)", r.message());
}

test "a bad DATE answer is protocol/date; a silent server is a timeout message" {
    {
        const sc = try scripted.Script.start(testing.allocator, tio, .{ .handler = struct {
            fn run(_: ?*anyopaque, s: *scripted.Session) void {
                s.line("200 hi");
                _ = s.readCmd();
                s.line("500 what?");
            }
        }.run });
        defer sc.stop();
        const r = try probe(testing.allocator, tio, sc.dialer(), .{ .host = "h", .port = sc.port() }, .{});
        try expectFail(r, .protocol, "date");
        try testing.expectEqualStrings("unexpected response from the server (500 what?)", r.message());
    }
    {
        const sc = try scripted.Script.start(testing.allocator, tio, .{ .handler = struct {
            fn run(_: ?*anyopaque, s: *scripted.Session) void {
                s.stall();
            }
        }.run });
        defer sc.stop();
        const r = try probe(testing.allocator, tio, sc.dialer(), .{ .host = "h", .port = sc.port() }, .{ .read_timeout_ms = 100 });
        try expectFail(r, .protocol, "greeting");
        try testing.expectEqualStrings("the server did not answer in time", r.message());
    }
}

test "the overall deadline bounds a server that trickles" {
    const sc = try scripted.Script.start(testing.allocator, tio, .{ .handler = struct {
        fn run(_: ?*anyopaque, s: *scripted.Session) void {
            s.line("200 hi");
            _ = s.readCmd();
            for (0..50) |_| {
                s.script.io.sleep(.fromMilliseconds(40), .awake) catch return;
                s.send("1");
            }
        }
    }.run });
    defer sc.stop();
    const t0 = Io.Clock.now(.awake, tio);
    const r = try probe(testing.allocator, tio, sc.dialer(), .{ .host = "h", .port = sc.port() }, .{ .probe_timeout_ms = 300 });
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, tio)).toMilliseconds() < 1500);
    try expectFail(r, .protocol, "date");
    try testing.expectEqualStrings("the server did not answer in time", r.message());
}

test "TLS problems come out as tls/<detail> and a plain server as not_tls" {
    const Case = struct { cert: []const u8, detail: []const u8 };
    for ([_]Case{
        .{ .cert = "wronghost", .detail = "certificate_hostname" },
        .{ .cert = "expired", .detail = "certificate_expired" },
        .{ .cert = "untrusted", .detail = "certificate_untrusted" },
    }) |case| {
        const roots = try tls.testRootsPem(testing.allocator, tio);
        defer testing.allocator.free(roots);
        const ctx = try tls.testContext(testing.allocator, tio, case.cert, .{});
        const srv = try nntptest.Server.startTls(testing.allocator, tio, ctx);
        defer srv.stop();
        var td = try nntp_tls.TlsDialer.init(.{ .extra_roots_pem = roots });
        defer td.deinit();
        const r = try probe(testing.allocator, tio, td.dialer(), targetOf(srv), .{});
        try expectFail(r, .tls, case.detail);
    }
    const plain = try nntptest.Server.start(testing.allocator, tio);
    defer plain.stop();
    var td = try nntp_tls.TlsDialer.init(.{});
    defer td.deinit();
    const r = try probe(testing.allocator, tio, td.dialer(), .{ .host = "127.0.0.1", .port = plain.port() }, .{});
    try testing.expectEqual(Class.tls, r.class);
    try testing.expect(std.mem.eql(u8, r.detail, "not_tls") or std.mem.eql(u8, r.detail, "handshake"));
}

test "a good TLS server probes ok" {
    const roots = try tls.testRootsPem(testing.allocator, tio);
    defer testing.allocator.free(roots);
    const ctx = try tls.testContext(testing.allocator, tio, "server", .{});
    const srv = try nntptest.Server.startTls(testing.allocator, tio, ctx);
    defer srv.stop();
    var td = try nntp_tls.TlsDialer.init(.{ .extra_roots_pem = roots });
    defer td.deinit();
    const r = try probe(testing.allocator, tio, td.dialer(), targetOf(srv), .{});
    try testing.expect(r.ok);
}

test "a bad config is protocol/other and sends nothing" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    const r = try probe(testing.allocator, tio, srv.dialer(), .{ .host = "h", .user = "a\r\nb" }, .{});
    try expectFail(r, .protocol, "other");
    try testing.expectEqual(@as(usize, 0), srv.dials());
}
