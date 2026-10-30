//! Tests of the connection: the conversations of Go's conn_test, auth_test,
//! body_test, date_test and dial_test, played against a scripted loopback
//! server, plus a fuzz target over an in-memory transport.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const conn_mod = @import("nntp_conn.zig");
const proto = @import("nntp_proto.zig");
const diag_mod = @import("nntp_diag.zig");
const transport_mod = @import("nntp_transport.zig");
const scripted = @import("nntp_scripted.zig");
const types = @import("usenet_types.zig");

const Conn = conn_mod.Conn;
const Config = conn_mod.Config;
const Diag = diag_mod.Diag;
const Script = scripted.Script;
const Session = scripted.Session;

const io = testing.io;

fn start(handler: scripted.Handler) !*Script {
    return Script.start(testing.allocator, io, .{ .handler = handler });
}

fn cfgFor(sc: *Script) Config {
    return .{ .host = scripted.test_host, .port = sc.port(), .connect_timeout_ms = 5000, .read_timeout_ms = 5000 };
}

fn dial(sc: *Script, cfg: Config, diag: *Diag) !*Conn {
    return Conn.connect(testing.allocator, io, sc.dialer(), cfg, diag);
}

fn dialOk(sc: *Script) !*Conn {
    var d: Diag = .{};
    return dial(sc, cfgFor(sc), &d);
}

/// Collects the lines a Sink receives, copied.
const Collect = struct {
    lines: std.ArrayList([]u8) = .empty,
    /// Refuse the line with this index.
    abort_at: ?usize = null,

    fn deinit(c: *Collect) void {
        for (c.lines.items) |l| testing.allocator.free(l);
        c.lines.deinit(testing.allocator);
    }

    fn sink(c: *Collect) types.Sink {
        return .{ .ctx = c, .line_fn = take };
    }

    fn take(ctx: *anyopaque, line: []const u8) error{Abort}!void {
        const c: *Collect = @ptrCast(@alignCast(ctx));
        if (c.abort_at == c.lines.items.len) return error.Abort;
        const copy = testing.allocator.dupe(u8, line) catch return error.Abort;
        c.lines.append(testing.allocator, copy) catch {
            testing.allocator.free(copy);
            return error.Abort;
        };
    }

    fn expectLines(c: *const Collect, want: []const []const u8) !void {
        try testing.expectEqual(want.len, c.lines.items.len);
        for (want, c.lines.items) |w, g| try testing.expectEqualStrings(w, g);
    }
};

fn expectCmds(sc: *Script, want: []const []const u8) !void {
    const got = sc.received();
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expectEqualStrings(w, g);
    try testing.expectEqual(@as(usize, 0), sc.failureCount());
}

// ---- greeting and config -------------------------------------------------------

fn readQuit(_: ?*anyopaque, s: *Session) void {
    s.line("200 welcome");
    _ = s.readCmd();
}

test "greeting 200 and 201 (posting prohibited) are accepted; close sends QUIT" {
    for ([_][]const u8{ "200 welcome", "201 no posting" }) |greeting| {
        const H = struct {
            fn run(ctx: ?*anyopaque, s: *Session) void {
                const g: *const []const u8 = @ptrCast(@alignCast(ctx.?));
                s.line(g.*);
                _ = s.readCmd();
            }
        };
        var g = greeting;
        const sc = try Script.start(testing.allocator, io, .{ .handler = H.run, .ctx = @ptrCast(&g) });
        defer sc.stop();
        const c = try dialOk(sc);
        try testing.expect(!c.isDead());
        c.close(io);
        try expectCmds(sc, &.{"QUIT"});
    }
}

test "bad greetings: class, code and step" {
    const Case = struct { greeting: []const u8, err: anyerror, code: u16 };
    const cases = [_]Case{
        .{ .greeting = "502 too many connections", .err = error.Connection, .code = 502 },
        .{ .greeting = "400 service temporarily unavailable", .err = error.Connection, .code = 400 },
        .{ .greeting = "211 something else", .err = error.Protocol, .code = 211 },
        .{ .greeting = "hello there", .err = error.Protocol, .code = 0 },
        .{ .greeting = "2000 hi", .err = error.Protocol, .code = 0 },
        .{ .greeting = "200hi", .err = error.Protocol, .code = 0 },
    };
    for (cases) |case| {
        const H = struct {
            fn run(ctx: ?*anyopaque, s: *Session) void {
                const g: *const []const u8 = @ptrCast(@alignCast(ctx.?));
                s.line(g.*);
            }
        };
        var g = case.greeting;
        const sc = try Script.start(testing.allocator, io, .{ .handler = H.run, .ctx = @ptrCast(&g) });
        defer sc.stop();
        var d: Diag = .{};
        try testing.expectError(@as(conn_mod.OpenError, @errorCast(case.err)), dial(sc, cfgFor(sc), &d));
        try testing.expectEqual(case.code, d.code);
        try testing.expectEqual(diag_mod.Op.greeting, d.op);
    }
}

test "a server that closes before the greeting is a connection error" {
    const sc = try start(struct {
        fn run(_: ?*anyopaque, _: *Session) void {}
    }.run);
    defer sc.stop();
    var d: Diag = .{};
    try testing.expectError(error.Connection, dial(sc, cfgFor(sc), &d));
    try testing.expectEqual(diag_mod.Cause.eof, d.cause);
}

test "server text in a diagnostic is sanitised and truncated" {
    const sc = try start(struct {
        fn run(_: ?*anyopaque, s: *Session) void {
            var buf: [1100]u8 = undefined;
            const head = "502 bad\x01\x1b[31m ";
            @memcpy(buf[0..head.len], head);
            @memset(buf[head.len .. head.len + 1000], 'x');
            buf[head.len + 1000] = 0xff;
            s.line(buf[0 .. head.len + 1001]);
        }
    }.run);
    defer sc.stop();
    var d: Diag = .{};
    try testing.expectError(error.Connection, dial(sc, cfgFor(sc), &d));
    try testing.expect(d.text().len <= 200);
    for (d.text()) |ch| try testing.expect(ch >= 0x20 and ch <= 0x7e);
}

test "a bad config is refused before anything is dialed" {
    const sc = try start(struct {
        fn run(_: ?*anyopaque, _: *Session) void {}
    }.run);
    defer sc.stop();
    var d: Diag = .{};
    try testing.expectError(error.InvalidConfig, dial(sc, .{ .host = "" }, &d));
    try testing.expectError(error.InvalidConfig, dial(sc, .{ .host = "h", .user = "u\r\nQUIT", .pass = "p" }, &d));
    try testing.expectError(error.InvalidConfig, dial(sc, .{ .host = "h", .user = "u", .pass = "p\x00" }, &d));
    try testing.expectEqual(@as(usize, 0), sc.acceptedCount());
}

// ---- authentication -------------------------------------------------------------

test "AUTHINFO USER then PASS" {
    const sc = try start(struct {
        fn run(_: ?*anyopaque, s: *Session) void {
            s.line("200 hi");
            s.expect("AUTHINFO USER alice");
            s.line("381 password required");
            s.expect("AUTHINFO PASS s3cret");
            s.line("281 ok");
            _ = s.readCmd();
        }
    }.run);
    defer sc.stop();
    var d: Diag = .{};
    var cfg = cfgFor(sc);
    cfg.user = "alice";
    cfg.pass = "s3cret";
    const c = try dial(sc, cfg, &d);
    c.close(io);
    try expectCmds(sc, &.{ "AUTHINFO USER alice", "AUTHINFO PASS s3cret", "QUIT" });
}

test "a server that accepts the user name alone is not sent the password" {
    const sc = try start(struct {
        fn run(_: ?*anyopaque, s: *Session) void {
            s.line("200 hi");
            s.expect("AUTHINFO USER alice");
            s.line("281 no password needed");
            _ = s.readCmd();
        }
    }.run);
    defer sc.stop();
    var d: Diag = .{};
    var cfg = cfgFor(sc);
    cfg.user = "alice";
    cfg.pass = "ignored";
    const c = try dial(sc, cfg, &d);
    c.close(io);
    try expectCmds(sc, &.{ "AUTHINFO USER alice", "QUIT" });
}

test "a server without credentials never sees AUTHINFO" {
    const sc = try start(readQuit);
    defer sc.stop();
    const c = try dialOk(sc);
    c.close(io);
    try expectCmds(sc, &.{"QUIT"});
}

test "481, 482 and 452 are authentication failures, at USER and at PASS, without credentials in the diagnostic" {
    for ([_][]const u8{ "481 bad credentials", "482 rejected", "452 not allowed" }) |code| {
        for ([_]bool{ false, true }) |at_pass| {
            const Ctx = struct { code: []const u8, at_pass: bool };
            const H = struct {
                fn run(ctx: ?*anyopaque, s: *Session) void {
                    const x: *const Ctx = @ptrCast(@alignCast(ctx.?));
                    s.line("200 hi");
                    _ = s.readCmd();
                    if (x.at_pass) {
                        s.line("381 pass");
                        _ = s.readCmd();
                    }
                    s.line(x.code);
                }
            };
            var x: Ctx = .{ .code = code, .at_pass = at_pass };
            const sc = try Script.start(testing.allocator, io, .{ .handler = H.run, .ctx = &x });
            defer sc.stop();
            var cfg = cfgFor(sc);
            cfg.user = "alice";
            cfg.pass = "hunter2-secret";
            var d: Diag = .{};
            try testing.expectError(error.Auth, dial(sc, cfg, &d));
            try testing.expectEqual(diag_mod.Kind.auth, d.kind);
            try testing.expectEqual(diag_mod.Op.auth, d.op);
            try testing.expect(std.mem.indexOf(u8, d.text(), "hunter2-secret") == null);
        }
    }
}

test "other answers to AUTHINFO are not authentication failures (502 may mean too many connections)" {
    const Case = struct { resp: []const u8, err: conn_mod.OpenError };
    for ([_]Case{
        .{ .resp = "502 try later", .err = error.Connection },
        .{ .resp = "400 bye", .err = error.Connection },
        .{ .resp = "200 what", .err = error.Protocol },
    }) |case| {
        const H = struct {
            fn run(ctx: ?*anyopaque, s: *Session) void {
                const r: *const []const u8 = @ptrCast(@alignCast(ctx.?));
                s.line("200 hi");
                _ = s.readCmd();
                s.line(r.*);
            }
        };
        var r = case.resp;
        const sc = try Script.start(testing.allocator, io, .{ .handler = H.run, .ctx = @ptrCast(&r) });
        defer sc.stop();
        var cfg = cfgFor(sc);
        cfg.user = "u";
        cfg.pass = "p";
        var d: Diag = .{};
        try testing.expectError(case.err, dial(sc, cfg, &d));
    }
}

test "credentials echoed by the server are redacted" {
    const sc = try start(struct {
        fn run(_: ?*anyopaque, s: *Session) void {
            s.line("200 hi");
            _ = s.readCmd();
            s.line("381 pass");
            _ = s.readCmd();
            s.line("481 wrong password hunter2-secret for alice");
        }
    }.run);
    defer sc.stop();
    var cfg = cfgFor(sc);
    cfg.user = "alice";
    cfg.pass = "hunter2-secret";
    var d: Diag = .{};
    try testing.expectError(error.Auth, dial(sc, cfg, &d));
    try testing.expectEqualStrings("wrong password *** for ***", d.text());
}

test "a stalled authentication ends at the read timeout" {
    const sc = try start(struct {
        fn run(_: ?*anyopaque, s: *Session) void {
            s.line("200 hi");
            _ = s.readCmd();
            s.stall();
        }
    }.run);
    defer sc.stop();
    var cfg = cfgFor(sc);
    cfg.user = "u";
    cfg.pass = "p";
    cfg.read_timeout_ms = 100;
    var d: Diag = .{};
    try testing.expectError(error.Connection, dial(sc, cfg, &d));
    try testing.expectEqual(diag_mod.Op.auth, d.op);
    try testing.expectEqual(diag_mod.Cause.timeout, d.cause);
}

test "no greeting ends at the read timeout" {
    const sc = try start(struct {
        fn run(_: ?*anyopaque, s: *Session) void {
            s.stall();
        }
    }.run);
    defer sc.stop();
    var cfg = cfgFor(sc);
    cfg.read_timeout_ms = 100;
    var d: Diag = .{};
    try testing.expectError(error.Connection, dial(sc, cfg, &d));
    try testing.expectEqual(diag_mod.Op.greeting, d.op);
    try testing.expectEqual(diag_mod.Cause.timeout, d.cause);
}

// ---- dial failures ---------------------------------------------------------------

const FailingDialer = struct {
    err: transport_mod.DialError,
    calls: usize = 0,
    host: []const u8 = "",
    port: u16 = 0,

    fn dialer(f: *FailingDialer) transport_mod.Dialer {
        return .{ .ctx = f, .dial_fn = dial_ };
    }

    fn dial_(ctx: *anyopaque, _: std.mem.Allocator, _: Io, req: transport_mod.DialRequest) transport_mod.DialError!transport_mod.Transport {
        const f: *FailingDialer = @ptrCast(@alignCast(ctx));
        f.calls += 1;
        f.host = req.host;
        f.port = req.port;
        return f.err;
    }
};

test "dial failures keep their step and cause" {
    const Case = struct { err: transport_mod.DialError, op: diag_mod.Op, cause: diag_mod.Cause };
    for ([_]Case{
        .{ .err = error.DnsNotFound, .op = .dial, .cause = .dns_not_found },
        .{ .err = error.TcpRefused, .op = .dial, .cause = .tcp_refused },
        .{ .err = error.TcpTimeout, .op = .dial, .cause = .tcp_timeout },
        .{ .err = error.TlsCertHostname, .op = .tls, .cause = .tls_cert_hostname },
        .{ .err = error.TlsNotTls, .op = .tls, .cause = .tls_not_tls },
    }) |case| {
        var f: FailingDialer = .{ .err = case.err };
        var d: Diag = .{};
        try testing.expectError(error.Connection, Conn.connect(testing.allocator, io, f.dialer(), .{ .host = "news.example", .port = 5555 }, &d));
        try testing.expectEqual(case.op, d.op);
        try testing.expectEqual(case.cause, d.cause);
        try testing.expectEqualStrings("news.example", f.host);
        try testing.expectEqual(@as(u16, 5555), f.port);
    }
    var f: FailingDialer = .{ .err = error.Canceled };
    var d: Diag = .{};
    try testing.expectError(error.Canceled, Conn.connect(testing.allocator, io, f.dialer(), .{ .host = "h" }, &d));
}

test "the port defaults to 563" {
    var f: FailingDialer = .{ .err = error.TcpOther };
    var d: Diag = .{};
    try testing.expectError(error.Connection, Conn.connect(testing.allocator, io, f.dialer(), .{ .host = "h" }, &d));
    try testing.expectEqual(@as(u16, 563), f.port);
}

// ---- BODY ------------------------------------------------------------------------

fn bodyServer(comptime serve: fn (s: *Session) void) !*Script {
    return start(struct {
        fn run(_: ?*anyopaque, s: *Session) void {
            s.line("200 hi");
            serve(s);
        }
    }.run);
}

test "BODY streams dot-unstuffed lines without terminators" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            s.expect("BODY <a@b>");
            s.send("222 0 <a@b>\r\n=ybegin line=128\r\n..foo\r\n...\r\n\r\nbar\r\na..b\r\nlf\n.\r\n");
            _ = s.readCmd();
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    var got: Collect = .{};
    defer got.deinit();
    try c.body(io, "a@b", got.sink());
    try got.expectLines(&.{ "=ybegin line=128", ".foo", "..", "", "bar", "a..b", "lf" });
    try testing.expect(!c.isDead());
    c.close(io);
    try expectCmds(sc, &.{ "BODY <a@b>", "QUIT" });
}

test "an empty body" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            s.send("222 ok\r\n.\r\n");
            _ = s.readCmd();
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    var got: Collect = .{};
    defer got.deinit();
    try c.body(io, "a@b", got.sink());
    try testing.expectEqual(@as(usize, 0), got.lines.items.len);
}

test "the terminator and the status line may arrive in pieces" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            for ([_][]const u8{ "22", "2 ok\r", "\nli", "ne\r\n.", "\r", "\n" }) |part| {
                s.send(part);
                s.script.io.sleep(.fromMilliseconds(15), .awake) catch return;
            }
            _ = s.readCmd();
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    var got: Collect = .{};
    defer got.deinit();
    try c.body(io, "a@b", got.sink());
    try got.expectLines(&.{"line"});
}

test "430, 411, 412, 420 and 423 mean not found and keep the connection usable" {
    for ([_][]const u8{ "430", "411", "412", "420", "423" }) |code| {
        const H = struct {
            fn run(ctx: ?*anyopaque, s: *Session) void {
                const cd: *const []const u8 = @ptrCast(@alignCast(ctx.?));
                s.line("200 hi");
                _ = s.readCmd();
                var buf: [32]u8 = undefined;
                s.line(std.fmt.bufPrint(&buf, "{s} no such article", .{cd.*}) catch return);
                _ = s.readCmd();
                s.send("222 ok\r\ndata\r\n.\r\n");
                _ = s.readCmd();
            }
        };
        var cd = code;
        const sc = try Script.start(testing.allocator, io, .{ .handler = H.run, .ctx = @ptrCast(&cd) });
        defer sc.stop();
        const c = try dialOk(sc);
        defer c.close(io);
        var got: Collect = .{};
        defer got.deinit();
        try testing.expectError(error.ArticleNotFound, c.body(io, "a@b", got.sink()));
        try testing.expectEqual(diag_mod.Kind.article_not_found, c.diag.kind);
        try testing.expect(!c.isDead());
        try c.body(io, "c@d", got.sink());
        try got.expectLines(&.{"data"});
    }
}

test "other 4xx and 5xx answers to BODY kill the connection" {
    for ([_][]const u8{ "400", "502", "500", "503", "480", "450", "401" }) |code| {
        const H = struct {
            fn run(ctx: ?*anyopaque, s: *Session) void {
                const cd: *const []const u8 = @ptrCast(@alignCast(ctx.?));
                s.line("200 hi");
                _ = s.readCmd();
                var buf: [32]u8 = undefined;
                s.line(std.fmt.bufPrint(&buf, "{s} nope", .{cd.*}) catch return);
            }
        };
        var cd = code;
        const sc = try Script.start(testing.allocator, io, .{ .handler = H.run, .ctx = @ptrCast(&cd) });
        defer sc.stop();
        const c = try dialOk(sc);
        defer c.close(io);
        var got: Collect = .{};
        defer got.deinit();
        try testing.expectError(error.Connection, c.body(io, "a@b", got.sink()));
        try testing.expect(c.isDead());
        try testing.expectEqual(@as(u16, @intCast(try std.fmt.parseInt(u16, code, 10))), c.diag.code);
        // A dead connection fails fast.
        try testing.expectError(error.Connection, c.body(io, "a@b", got.sink()));
    }
}

test "an unexpected success code is a protocol error" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.line("221 head follows");
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    var got: Collect = .{};
    defer got.deinit();
    try testing.expectError(error.Protocol, c.body(io, "a@b", got.sink()));
    try testing.expect(c.isDead());
}

test "the connection lost mid-body, with and without a partial line" {
    for ([_][]const u8{ "222 ok\r\none\r\n", "222 ok\r\none\r\ntwo-partial" }) |wire| {
        const H = struct {
            fn run(ctx: ?*anyopaque, s: *Session) void {
                const w: *const []const u8 = @ptrCast(@alignCast(ctx.?));
                s.line("200 hi");
                _ = s.readCmd();
                s.send(w.*);
                s.close();
            }
        };
        var w = wire;
        const sc = try Script.start(testing.allocator, io, .{ .handler = H.run, .ctx = @ptrCast(&w) });
        defer sc.stop();
        const c = try dialOk(sc);
        defer c.close(io);
        var got: Collect = .{};
        defer got.deinit();
        try testing.expectError(error.Connection, c.body(io, "a@b", got.sink()));
        try got.expectLines(&.{"one"});
        try testing.expect(c.isDead());
    }
}

test "a status-looking line inside a body is just data" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.send("222 ok\r\n502 looks like a status\r\n.\r\n");
            _ = s.readCmd();
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    var got: Collect = .{};
    defer got.deinit();
    try c.body(io, "a@b", got.sink());
    try got.expectLines(&.{"502 looks like a status"});
}

fn longLine(comptime n: usize) fn (s: *Session) void {
    return struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.send("222 ok\r\n");
            const buf = testing.allocator.alloc(u8, n + 2) catch return;
            defer testing.allocator.free(buf);
            @memset(buf[0..n], 'a');
            buf[n] = '\r';
            buf[n + 1] = '\n';
            s.send(buf);
            s.send(".\r\n");
            _ = s.readCmd();
        }
    }.run;
}

test "a line of exactly the limit passes, one byte more is a protocol error" {
    {
        const sc = try bodyServer(longLine(proto.max_line_length));
        defer sc.stop();
        const c = try dialOk(sc);
        defer c.close(io);
        var got: Collect = .{};
        defer got.deinit();
        try c.body(io, "a@b", got.sink());
        try testing.expectEqual(@as(usize, proto.max_line_length), got.lines.items[0].len);
    }
    {
        const sc = try bodyServer(longLine(proto.max_line_length + 1));
        defer sc.stop();
        const c = try dialOk(sc);
        defer c.close(io);
        var got: Collect = .{};
        defer got.deinit();
        try testing.expectError(error.Protocol, c.body(io, "a@b", got.sink()));
        try testing.expect(c.isDead());
        try testing.expectEqual(@as(usize, 0), got.lines.items.len);
    }
    {
        const sc = try bodyServer(longLine(3 * 16 * 1024));
        defer sc.stop();
        const c = try dialOk(sc);
        defer c.close(io);
        var got: Collect = .{};
        defer got.deinit();
        try testing.expectError(error.Protocol, c.body(io, "a@b", got.sink()));
    }
}

test "a long status line is a protocol error" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            var buf: [9000]u8 = undefined;
            @memset(&buf, 'x');
            @memcpy(buf[0..4], "222 ");
            s.line(&buf);
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    var got: Collect = .{};
    defer got.deinit();
    try testing.expectError(error.Protocol, c.body(io, "a@b", got.sink()));
}

test "a body over the size limit is a protocol error" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.send("222 ok\r\n");
            const line = testing.allocator.alloc(u8, 8000 + 2) catch return;
            defer testing.allocator.free(line);
            @memset(line[0..8000], 'x');
            line[8000] = '\r';
            line[8001] = '\n';
            // Over 16 MiB of lines; the client hangs up at the limit.
            for (0..2200) |_| s.send(line);
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    const Count = struct {
        n: usize = 0,
        fn take(ctx: *anyopaque, line: []const u8) error{Abort}!void {
            const self: *@This() = @ptrCast(@alignCast(ctx));
            self.n += line.len + 2;
        }
    };
    var count: Count = .{};
    try testing.expectError(error.Protocol, c.body(io, "a@b", .{ .ctx = &count, .line_fn = Count.take }));
    try testing.expect(count.n <= proto.max_body_size);
    try testing.expect(c.isDead());
}

test "a message id that could break out of the command is refused without sending" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            _ = s.readCmd();
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    var got: Collect = .{};
    defer got.deinit();
    for ([_][]const u8{ "", "a b", "a\r\nQUIT", "<a@b>", "a>b", "caf\xc3\xa9@x" }) |bad| {
        try testing.expectError(error.InvalidMessageId, c.body(io, bad, got.sink()));
        try testing.expect(!c.isDead());
    }
    try testing.expectError(error.InvalidMessageId, c.body(io, &@as([251]u8, @splat('a')), got.sink()));
    c.close(io);
    try expectCmds(sc, &.{"QUIT"});
}

test "a sink that refuses a line aborts the command and kills the connection" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.send("222 ok\r\none\r\ntwo\r\nthree\r\n.\r\n");
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    var got: Collect = .{ .abort_at = 1 };
    defer got.deinit();
    try testing.expectError(error.Aborted, c.body(io, "a@b", got.sink()));
    try got.expectLines(&.{"one"});
    try testing.expect(c.isDead());
}

test "the idle timeout applies to each read, not to the whole command" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.send("222 ok\r\n");
            for (0..6) |_| {
                s.script.io.sleep(.fromMilliseconds(60), .awake) catch return;
                s.send("tick\r\n");
            }
            s.send(".\r\n");
            _ = s.readCmd();
        }
    }.run);
    defer sc.stop();
    var cfg = cfgFor(sc);
    cfg.read_timeout_ms = 250; // shorter than the whole body (360 ms), longer than each gap
    var d: Diag = .{};
    const c = try dial(sc, cfg, &d);
    defer c.close(io);
    var got: Collect = .{};
    defer got.deinit();
    try c.body(io, "a@b", got.sink());
    try testing.expectEqual(@as(usize, 6), got.lines.items.len);
}

test "a stalled body ends at the idle timeout" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.send("222 ok\r\npartial");
            s.stall();
        }
    }.run);
    defer sc.stop();
    var cfg = cfgFor(sc);
    cfg.read_timeout_ms = 120;
    var d: Diag = .{};
    const c = try dial(sc, cfg, &d);
    defer c.close(io);
    var got: Collect = .{};
    defer got.deinit();
    try testing.expectError(error.Connection, c.body(io, "a@b", got.sink()));
    try testing.expectEqual(diag_mod.Cause.timeout, c.diag.cause);
    try testing.expect(c.isDead());
}

test "a command deadline bounds a command that keeps trickling" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.send("222 ok\r\n");
            for (0..100) |_| {
                s.script.io.sleep(.fromMilliseconds(30), .awake) catch return;
                s.send("tick\r\n");
            }
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    c.command_deadline = Io.Clock.Timestamp.now(io, .awake).addDuration(.{ .raw = .fromMilliseconds(200), .clock = .awake });
    var got: Collect = .{};
    defer got.deinit();
    const t0 = Io.Clock.now(.awake, io);
    try testing.expectError(error.Connection, c.body(io, "a@b", got.sink()));
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, io)).toMilliseconds() < 2000);
    try testing.expectEqual(diag_mod.Cause.timeout, c.diag.cause);
}

fn bodyTask(c: *Conn, got: *Collect) conn_mod.BodyError!void {
    return c.body(io, "a@b", got.sink());
}

test "cancelling the task aborts a blocked read at once and leaves the connection dead" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.send("222 ok\r\none\r\n");
            s.stall();
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    var got: Collect = .{};
    defer got.deinit();
    var fut = try io.concurrent(bodyTask, .{ c, &got });
    try io.sleep(.fromMilliseconds(100), .awake);
    const t0 = Io.Clock.now(.awake, io);
    try testing.expectError(error.Canceled, fut.cancel(io));
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, io)).toMilliseconds() < 1000);
    try got.expectLines(&.{"one"});
    try testing.expect(c.isDead());
}

test "close sends QUIT only on a live connection" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.line("502 bye");
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    var got: Collect = .{};
    defer got.deinit();
    try testing.expectError(error.Connection, c.body(io, "a@b", got.sink()));
    c.close(io);
    try expectCmds(sc, &.{"BODY <a@b>"});
}

// ---- DATE -------------------------------------------------------------------------

fn dateServer(comptime reply: []const u8) !*Script {
    return bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.line(reply);
            _ = s.readCmd();
        }
    }.run);
}

test "DATE parses the server's clock as UTC" {
    const sc = try dateServer("111 20260102150405");
    defer sc.stop();
    const c = try dialOk(sc);
    try testing.expectEqual(@as(i64, 1767366245), try c.date(io));
    try testing.expect(!c.isDead());
    c.close(io);
    try expectCmds(sc, &.{ "DATE", "QUIT" });
}

test "a malformed DATE timestamp is a protocol error" {
    inline for (.{ "111", "111 ", "111 notadate", "111 2026010215040", "111 20261301000000", "111 20260230000000" }) |reply| {
        const sc = try dateServer(reply);
        defer sc.stop();
        const c = try dialOk(sc);
        defer c.close(io);
        try testing.expectError(error.Protocol, c.date(io));
        try testing.expect(c.isDead());
    }
}

test "other answers to DATE kill the connection" {
    inline for (.{ .{ "500 what?", error.Connection }, .{ "502 go away", error.Connection }, .{ "400 bye", error.Connection }, .{ "200 not a date", error.Protocol } }) |case| {
        const sc = try dateServer(case[0]);
        defer sc.stop();
        const c = try dialOk(sc);
        defer c.close(io);
        try testing.expectError(case[1], c.date(io));
        try testing.expect(c.isDead());
        try testing.expectError(error.Connection, c.date(io));
    }
}

test "a connection lost during DATE" {
    const sc = try bodyServer(struct {
        fn run(s: *Session) void {
            _ = s.readCmd();
            s.close();
        }
    }.run);
    defer sc.stop();
    const c = try dialOk(sc);
    defer c.close(io);
    try testing.expectError(error.Connection, c.date(io));
    try testing.expect(c.isDead());
}

// ---- fuzz --------------------------------------------------------------------------

/// A transport over bytes in memory: reads what it is given, discards writes.
const MemTransport = struct {
    reader: Io.Reader,
    writer: Io.Writer,
    wbuf: [64]u8 = undefined,
    rbuf: [2 * proto.min_read_buffer]u8 = undefined,
    data: []const u8,
    pos: usize = 0,

    fn init(self: *MemTransport, data: []const u8) transport_mod.Transport {
        self.* = .{
            .data = data,
            .reader = .{ .vtable = &.{ .stream = stream, .readVec = readVec }, .buffer = undefined, .seek = 0, .end = 0 },
            .writer = .{ .vtable = &.{ .drain = drain }, .buffer = undefined },
        };
        self.reader.buffer = &self.rbuf;
        self.writer.buffer = &self.wbuf;
        return .{ .ctx = self, .reader = &self.reader, .writer = &self.writer, .vtable = &vtable };
    }

    const vtable: transport_mod.Transport.VTable = .{ .begin = begin, .touch = touch, .end = end, .failure = failure, .shutdown = shutdown, .destroy = destroy };
    fn begin(_: *anyopaque, _: ?Io.Clock.Timestamp) void {}
    fn touch(_: *anyopaque) void {}
    fn end(_: *anyopaque) void {}
    fn failure(_: *anyopaque) transport_mod.Failure {
        return .none;
    }
    fn shutdown(_: *anyopaque, _: Io) void {}
    fn destroy(_: *anyopaque, _: Io) void {}

    fn stream(_: *Io.Reader, _: *Io.Writer, _: Io.Limit) Io.Reader.StreamError!usize {
        return error.EndOfStream;
    }

    fn readVec(r: *Io.Reader, data: [][]u8) Io.Reader.Error!usize {
        const self: *MemTransport = @alignCast(@fieldParentPtr("reader", r));
        // Hand over at most 1000 bytes at a time, so lines span reads.
        var iovecs: [1][]u8 = undefined;
        const dest_n, const data_size = try r.writableVector(&iovecs, data);
        _ = dest_n;
        const left = self.data[self.pos..];
        if (left.len == 0) return error.EndOfStream;
        const n = @min(left.len, iovecs[0].len, 1000);
        @memcpy(iovecs[0][0..n], left[0..n]);
        self.pos += n;
        if (n > data_size) {
            r.end += n - data_size;
            return data_size;
        }
        return n;
    }

    fn drain(w: *Io.Writer, data: []const []const u8, splat: usize) Io.Writer.Error!usize {
        var total: usize = 0;
        for (data[0 .. data.len - 1]) |d| total += d.len;
        total += data[data.len - 1].len * splat;
        _ = w.consume(w.end);
        return total;
    }
};

test "fuzz: a body read from any bytes ends in a known class, within the limits, with the connection dead unless not found" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var bytes: [12_000]u8 = undefined;
            const n = smith.slice(&bytes);
            var mem: MemTransport = undefined;
            // The greeting is part of the input too: prepend a good one so
            // the body path is reached most of the time.
            var gen: [12_100]u8 = undefined;
            const greeting = "200 hi\r\n";
            @memcpy(gen[0..greeting.len], greeting);
            @memcpy(gen[greeting.len..][0..n], bytes[0..n]);
            const t2 = mem.init(gen[0 .. greeting.len + n]);
            var d: Diag = .{};
            const c = Conn.open(testing.allocator, io, t2, .{ .host = "h" }, &d) catch return;
            defer c.close(io);
            const Check = struct {
                total: usize = 0,
                fn take(ctx: *anyopaque, line: []const u8) error{Abort}!void {
                    const self: *@This() = @ptrCast(@alignCast(ctx));
                    if (std.mem.indexOfScalar(u8, line, '\n') != null) return error.Abort;
                    if (line.len > proto.max_line_length) return error.Abort;
                    self.total += line.len + 2;
                }
            };
            var chk: Check = .{};
            c.body(io, "a@b", .{ .ctx = &chk, .line_fn = Check.take }) catch |err| switch (err) {
                error.ArticleNotFound => return,
                error.Connection, error.Protocol => {
                    try testing.expect(c.isDead());
                    return;
                },
                error.Aborted => return error.TestUnexpectedResult, // the checks above failed
                else => return err,
            };
            try testing.expect(chk.total <= proto.max_body_size);
        }
    }.one, .{ .corpus = &.{
        "\x24\x00\x00\x00222 ok\r\nline\r\n..stuffed\r\n.\r\n    ",
        "\x0c\x00\x00\x00222 ok\r\n.\r\n",
        "\x0a\x00\x00\x00430 gone\r\n",
        "\x0b\x00\x00\x00502 bye\r\n\r\n",
        "\x14\x00\x00\x00222 ok\r\npartial     ",
        "\x0f\x00\x00\x00222 ok\nlf only\n.\n",
        "\x07\x00\x00\x00garbage",
    } });
}
