//! Tests of the connection test of a Server, through the routes' handlers
//! down to the NNTP client: against the fake NNTP server (plain loopback
//! dialer), scripted conversations, and the real TLS dialer with the test
//! certificates. They pin the exact answers of internal/app/prober.go and
//! the black-box tests of internal/blackbox/usenet_prober_test.go.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const nntp = @import("nntp.zig");
const nntptest = @import("nntptest.zig");
const scripted = @import("nntp_scripted.zig");
const tls = @import("tls.zig");
const prober_mod = @import("servers_prober.zig");
const api = @import("servers_api.zig");
const Key = @import("keyfile.zig").Key;

const ex = @import("http_exchange.zig");
const Rig = api.Rig;
const tio = testing.io;

/// The credentials every test sends; the secret must never come back.
const user = "joe";
const secret = "pw-SECRET-4711";

/// The answer of a test route, field for field (extra or missing fields fail
/// the parse).
const Answer = struct {
    ok: bool,
    class: []const u8,
    detail: []const u8,
    message: []const u8,
    connect_ms: i64,
    latency_ms: i64,
};

fn parseAnswer(e: ex.Exchange) !Answer {
    try testing.expectEqual(std.http.Status.ok, e.res.status);
    try testing.expectEqualStrings("application/json", e.res.header("Content-Type").?);
    try testing.expect(std.mem.indexOf(u8, e.res.body, secret) == null);
    const a = try std.json.parseFromSliceLeaky(Answer, e.arena, e.res.body, .{});
    try testing.expect(a.connect_ms >= 0 and a.latency_ms >= 0);
    if (!a.ok) {
        // No times unless the connection worked.
        try testing.expectEqual(@as(i64, 0), a.connect_ms);
        try testing.expectEqual(@as(i64, 0), a.latency_ms);
    }
    return a;
}

fn expectAnswer(a: Answer, ok: bool, class: []const u8, detail: []const u8, message: []const u8) !void {
    try testing.expectEqual(ok, a.ok);
    try testing.expectEqualStrings(class, a.class);
    try testing.expectEqualStrings(detail, a.detail);
    try testing.expectEqualStrings(message, a.message);
}

/// A Rig whose service tests through an NntpProber on `dialer`, with short
/// timeouts so a silent server costs milliseconds, not the 10 seconds of
/// production.
const Env = struct {
    rig: Rig,
    np: prober_mod.NntpProber,

    fn start(env: *Env, dialer: nntp.Dialer) !void {
        try env.rig.open();
        env.np = .init(testing.allocator, dialer);
        env.np.options = .{ .probe_timeout_ms = 10_000, .connect_timeout_ms = 3000, .read_timeout_ms = 3000 };
        env.rig.svc.prober = env.np.prober();
    }

    fn stop(env: *Env) void {
        env.rig.close();
    }

    fn body(env: *Env, host: []const u8, port: u16, password: []const u8) ![]const u8 {
        return std.fmt.allocPrint(env.rig.arena_state.allocator(), "{{\"name\":\"probe\",\"host\":\"{s}\",\"port\":{d},\"username\":\"{s}\",\"password\":\"{s}\"}}", .{ host, port, user, password });
    }

    /// Tests an unsaved spec for `host:port` with the test credentials.
    fn probe(env: *Env, host: []const u8, port: u16) !Answer {
        return parseAnswer(try env.rig.testSpec(try env.body(host, port, secret)));
    }
};

fn freePort() !u16 {
    const any: Io.net.IpAddress = try .parse("127.0.0.1", 0);
    var l = try any.listen(tio, .{});
    defer l.deinit(tio);
    return l.socket.address.getPort();
}

// ---- scripted conversations ----------------------------------------------------------

const Step = union(enum) {
    /// Send a line.
    say: []const u8,
    /// Read one command.
    read,
    /// Wait for the client to hang up.
    stall,
    /// Drop the connection.
    hang,
};

fn handlerOf(comptime steps: []const Step) scripted.Handler {
    return struct {
        fn run(_: ?*anyopaque, s: *scripted.Session) void {
            inline for (steps) |st| switch (st) {
                .say => |l| s.line(l),
                .read => _ = s.readCmd(),
                .stall => s.stall(),
                .hang => s.close(),
            };
        }
    }.run;
}

fn startScript(comptime steps: []const Step) !*scripted.Script {
    return scripted.Script.start(testing.allocator, tio, .{ .handler = handlerOf(steps) });
}

// ---- the fake NNTP server --------------------------------------------------------------

test "a working server is ok, and the commands are AUTHINFO USER, PASS, DATE" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.requireAuth(user, secret);
    var env: Env = undefined;
    try env.start(srv.dialer());
    defer env.stop();

    const a = try env.probe("localhost", srv.port());

    try expectAnswer(a, true, "ok", "", "connected and authenticated");
    const cmds = srv.commands();
    try testing.expect(cmds.len >= 3);
    try testing.expectEqualStrings("AUTHINFO USER " ++ user, cmds[0]);
    try testing.expectEqualStrings("AUTHINFO PASS " ++ secret, cmds[1]);
    try testing.expectEqualStrings("DATE", cmds[2]);
    try testing.expectEqual(@as(usize, 1), srv.dials());
}

test "the milliseconds of the answer are the whole milliseconds of the round trips" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    srv.setLatencyMs(30);
    var env: Env = undefined;
    try env.start(srv.dialer());
    defer env.stop();

    const a = try env.probe("localhost", srv.port());

    try testing.expect(a.ok);
    try testing.expect(a.latency_ms >= 30 and a.latency_ms < 5000);
}

test "a wrong password is auth/rejected with the code, after one attempt" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.requireAuth(user, "another-password");
    var env: Env = undefined;
    try env.start(srv.dialer());
    defer env.stop();

    const a = try env.probe("localhost", srv.port());

    try expectAnswer(a, false, "auth", "rejected", "the server rejected the user name or password (481)");
    try testing.expectEqual(@as(usize, 1), srv.authAttempts());
}

test "a saved Server is tested with its stored password, also after a change" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.requireAuth(user, secret);
    var env: Env = undefined;
    try env.start(srv.dialer());
    defer env.stop();
    const created = try env.rig.create(try env.body("localhost", srv.port(), secret));
    const id = try env.rig.idOf(created);

    try expectAnswer(try parseAnswer(try env.rig.testById(id)), true, "ok", "", "connected and authenticated");
    try testing.expectEqualStrings("AUTHINFO PASS " ++ secret, srv.commands()[1]);

    // A wrong password in the stored Server is reported the same way.
    _ = try env.rig.patchServer(id, "{\"password\":\"changed\"}");
    try expectAnswer(try parseAnswer(try env.rig.testById(id)), false, "auth", "rejected", "the server rejected the user name or password (481)");
}

test "a saved Server whose password cannot be opened is a conflict, and nothing is dialed" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    var env: Env = undefined;
    try env.start(srv.dialer());
    defer env.stop();
    const created = try env.rig.create(try env.body("localhost", srv.port(), secret));
    const id = try env.rig.idOf(created);
    var other: Key = .{ .bytes = @splat(9) };
    defer other.deinit();
    env.rig.svc.repo.key = &other;

    const e = try env.rig.testById(id);

    try api.expectError(e, .conflict, "password_unavailable");
    try testing.expectEqualStrings(
        "{\"error\":{\"code\":\"password_unavailable\",\"message\":\"the stored password cannot be decrypted: enter the password again\"}}\n",
        e.res.body,
    );
    try testing.expectEqual(@as(usize, 0), srv.dials());
}

test "an unknown saved Server is not found" {
    var env: Env = undefined;
    try env.start(undefined);
    defer env.stop();

    const e = try env.rig.testById("nope");

    try testing.expectEqual(std.http.Status.not_found, e.res.status);
    try testing.expectEqualStrings("{\"error\":{\"code\":\"not_found\",\"message\":\"not found\"}}\n", e.res.body);
}

test "an unsaved spec is validated before any connection and saves nothing" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    var env: Env = undefined;
    try env.start(srv.dialer());
    defer env.stop();

    for ([_]struct { body: []const u8, code: []const u8 }{
        .{ .body = "{\"name\":\"x\"}", .code = "invalid_host" },
        .{ .body = "{\"host\":\"https://h.example\"}", .code = "invalid_host" },
        .{ .body = "{\"host\":\"h.example\",\"port\":99999}", .code = "invalid_port" },
        .{ .body = "{\"host\":\"h.example\",\"max_connections\":101}", .code = "invalid_max_connections" },
        .{ .body = "{\"host\":\"h.example\",\"tls\":false}", .code = "invalid_json" },
        .{ .body = "{\"name\":\"a\\tb\",\"host\":\"h.example\"}", .code = "invalid_name" },
        .{ .body = "{\"host\":\"h.example\",\"username\":\"a\\nb\"}", .code = "invalid_credentials" },
    }) |c| try api.expectError(try env.rig.testSpec(c.body), .bad_request, c.code);

    try testing.expectEqual(@as(usize, 0), srv.dials());
    const list = try env.rig.svc.list(env.rig.arena_state.allocator());
    try testing.expectEqual(@as(usize, 0), list.len);
}

test "an unsaved spec without a name or port is tested on 563 as a Server named test" {
    var env: Env = undefined;
    try env.start(undefined);
    defer env.stop();
    var fake: Fake = .{};
    defer fake.deinit();
    env.rig.svc.prober = fake.prober();

    const a = try parseAnswer(try env.rig.testSpec("{\"host\":\"  news.example.com \",\"username\":\"joe\",\"password\":\"hunter2\"}"));

    try testing.expect(a.ok);
    try testing.expectEqual(@as(usize, 1), fake.calls);
    try testing.expectEqualStrings("news.example.com", fake.host);
    try testing.expectEqual(@as(u16, 563), fake.port);
    try testing.expectEqualStrings("joe", fake.user);
    try testing.expectEqualStrings("hunter2", fake.pass);
}

test "a result is shown with whole milliseconds and the words of the class" {
    var env: Env = undefined;
    try env.start(undefined);
    defer env.stop();
    var fake: Fake = .{ .result = .{ .ok = true, .class = .ok, .connect_ns = 12_999_999, .latency_ns = 999_999 } };
    defer fake.deinit();
    env.rig.svc.prober = fake.prober();

    const e = try env.rig.testSpec("{\"host\":\"h.example\"}");

    try testing.expectEqualStrings("{\"ok\":true,\"class\":\"ok\",\"detail\":\"\",\"message\":\"\",\"connect_ms\":12,\"latency_ms\":0}\n", e.res.body);
}

test "a saved Server hands its host, port and stored credentials to the prober" {
    var env: Env = undefined;
    try env.start(undefined);
    defer env.stop();
    var fake: Fake = .{};
    defer fake.deinit();
    env.rig.svc.prober = fake.prober();
    const created = try env.rig.create("{\"name\":\"main\",\"host\":\"news.example.com\",\"port\":443,\"username\":\"joe\",\"password\":\"hunter2\"}");

    _ = try parseAnswer(try env.rig.testById(try env.rig.idOf(created)));

    try testing.expectEqualStrings("news.example.com", fake.host);
    try testing.expectEqual(@as(u16, 443), fake.port);
    try testing.expectEqualStrings("joe", fake.user);
    try testing.expectEqualStrings("hunter2", fake.pass);
}

/// A Prober that records what it was asked and answers `result`.
const Fake = struct {
    calls: usize = 0,
    host: []const u8 = "",
    user: []const u8 = "",
    pass: []const u8 = "",
    port: u16 = 0,
    result: prober_mod.Result = .{ .ok = true, .class = .ok },

    fn deinit(f: *Fake) void {
        testing.allocator.free(f.host);
        testing.allocator.free(f.user);
        testing.allocator.free(f.pass);
    }

    fn prober(f: *Fake) prober_mod.Prober {
        return .{ .ctx = f, .probe_fn = run };
    }

    fn run(ctx: *anyopaque, _: Io, t: prober_mod.Target) error{ Canceled, OutOfMemory }!prober_mod.Result {
        const f: *Fake = @ptrCast(@alignCast(ctx));
        f.deinit();
        f.calls += 1;
        f.host = try testing.allocator.dupe(u8, t.host);
        f.user = try testing.allocator.dupe(u8, t.user);
        f.pass = try testing.allocator.dupe(u8, t.pass);
        f.port = t.port;
        return f.result;
    }
};

test "a server that is full answers 502 Too many connections" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    srv.setMaxConnections(1);
    var env: Env = undefined;
    try env.start(srv.dialer());
    defer env.stop();
    var hd: nntp.diag.Diag = .{};
    const hold = try nntp.Conn.connect(testing.allocator, tio, srv.dialer(), srv.config(), &hd);
    defer hold.close(tio);

    const a = try env.probe("localhost", srv.port());

    try expectAnswer(a, false, "protocol", "greeting", "unexpected response from the server (502 Too many connections)");
}

test "a server that closes right after the handshake is protocol/greeting" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    srv.failNextDials(1);
    var env: Env = undefined;
    try env.start(srv.dialer());
    defer env.stop();

    const a = try env.probe("localhost", srv.port());

    try expectAnswer(a, false, "protocol", "greeting", "unexpected response from the server");
}

// ---- scripted conversations ---------------------------------------------------------------

test "a refusal is quoted with the credentials scrubbed from it" {
    const sc = try startScript(&.{.{ .say = "502 go away " ++ user ++ " " ++ secret }});
    defer sc.stop();
    var env: Env = undefined;
    try env.start(sc.dialer());
    defer env.stop();

    const a = try env.probe("localhost", sc.port());

    try expectAnswer(a, false, "protocol", "greeting", "unexpected response from the server (502 go away *** ***)");
}

test "an odd answer to AUTHINFO is protocol/auth_response" {
    const sc = try startScript(&.{ .{ .say = "200 hi" }, .read, .{ .say = "400 service temporarily unavailable" } });
    defer sc.stop();
    var env: Env = undefined;
    try env.start(sc.dialer());
    defer env.stop();

    const a = try env.probe("localhost", sc.port());

    try expectAnswer(a, false, "protocol", "auth_response", "unexpected response from the server (400 service temporarily unavailable)");
}

test "a server that cannot tell the time is protocol/date" {
    const sc = try startScript(&.{ .{ .say = "200 hi" }, .read, .{ .say = "381 pass" }, .read, .{ .say = "281 ok" }, .read, .{ .say = "500 what is a date" } });
    defer sc.stop();
    var env: Env = undefined;
    try env.start(sc.dialer());
    defer env.stop();

    const a = try env.probe("localhost", sc.port());

    try expectAnswer(a, false, "protocol", "date", "unexpected response from the server (500 what is a date)");
}

test "a server that never greets times out as protocol/greeting" {
    const sc = try startScript(&.{.stall});
    defer sc.stop();
    var env: Env = undefined;
    try env.start(sc.dialer());
    defer env.stop();
    env.np.options.read_timeout_ms = 150;

    const t0 = Io.Clock.now(.awake, tio);
    const a = try env.probe("localhost", sc.port());

    try expectAnswer(a, false, "protocol", "greeting", "the server did not answer in time");
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, tio)).toMilliseconds() < 3000);
}

test "the overall deadline of 20 seconds is the prober's option" {
    const np = prober_mod.NntpProber.init(testing.allocator, undefined);
    try testing.expectEqual(@as(u32, 20_000), np.options.probe_timeout_ms);
    try testing.expectEqual(@as(u32, 10_000), np.options.connect_timeout_ms);
    try testing.expectEqual(@as(u32, 10_000), np.options.read_timeout_ms);
}

// ---- the real TLS dialer ------------------------------------------------------------------

/// A TLS dialer that trusts the test CA.
fn tlsDialer(roots: []const u8) !nntp.TlsDialer {
    return nntp.TlsDialer.init(.{ .extra_roots_pem = roots });
}

test "a TLS server with a good certificate is ok, reached by its host name" {
    const roots = try tls.testRootsPem(testing.allocator, tio);
    defer testing.allocator.free(roots);
    const srv = try nntptest.Server.startTls(testing.allocator, tio, try tls.testContext(testing.allocator, tio, "server", .{}));
    defer srv.stop();
    try srv.requireAuth(user, secret);
    var td = try tlsDialer(roots);
    defer td.deinit();
    var env: Env = undefined;
    try env.start(td.dialer());
    defer env.stop();

    const a = try env.probe("localhost", srv.port());

    try expectAnswer(a, true, "ok", "", "connected and authenticated");
}

test "certificate problems are tls/certificate_*, and no credentials are sent" {
    const roots = try tls.testRootsPem(testing.allocator, tio);
    defer testing.allocator.free(roots);
    for ([_]struct { cert: []const u8, detail: []const u8, message: []const u8 }{
        .{ .cert = "untrusted", .detail = "certificate_untrusted", .message = "the server's certificate is not trusted" },
        .{ .cert = "wronghost", .detail = "certificate_hostname", .message = "the server's certificate is not valid for this host name" },
        .{ .cert = "expired", .detail = "certificate_expired", .message = "the server's certificate has expired or is not valid yet" },
    }) |c| {
        const srv = try nntptest.Server.startTls(testing.allocator, tio, try tls.testContext(testing.allocator, tio, c.cert, .{}));
        defer srv.stop();
        try srv.requireAuth(user, secret);
        var td = try tlsDialer(roots);
        defer td.deinit();
        var env: Env = undefined;
        try env.start(td.dialer());
        defer env.stop();

        const a = try env.probe("localhost", srv.port());

        try expectAnswer(a, false, "tls", c.detail, c.message);
        try testing.expectEqual(@as(usize, 0), srv.commands().len);
        try testing.expectEqual(@as(usize, 0), srv.authAttempts());
    }
}

test "a port that speaks no TLS is tls/not_tls and the password is not sent" {
    const srv = try nntptest.Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.requireAuth(user, secret);
    var td = try nntp.TlsDialer.init(.{});
    defer td.deinit();
    var env: Env = undefined;
    try env.start(td.dialer());
    defer env.stop();

    const a = try env.probe("127.0.0.1", srv.port());

    try expectAnswer(a, false, "tls", "not_tls", "the server does not speak TLS on this port: bitnews only connects over TLS");
    for (srv.commands()) |c| try testing.expect(std.mem.indexOf(u8, c, secret) == null);
}

test "a server that hangs up during the handshake is tls/handshake" {
    const sc = try startScript(&.{.hang});
    defer sc.stop();
    var td = try nntp.TlsDialer.init(.{});
    defer td.deinit();
    var env: Env = undefined;
    try env.start(td.dialer());
    defer env.stop();

    const a = try env.probe("127.0.0.1", sc.port());

    try expectAnswer(a, false, "tls", "handshake", "the TLS handshake failed");
}

test "a port nobody listens on is tcp/refused" {
    var td = try nntp.TlsDialer.init(.{});
    defer td.deinit();
    var env: Env = undefined;
    try env.start(td.dialer());
    defer env.stop();

    const a = try env.probe("127.0.0.1", try freePort());

    try expectAnswer(a, false, "tcp", "refused", "the connection was refused: check the host and port");
}

test "an unknown host is a DNS problem with the message of its detail" {
    var td = try nntp.TlsDialer.init(.{});
    defer td.deinit();
    var env: Env = undefined;
    try env.start(td.dialer());
    defer env.stop();

    const a = try env.probe("no-such-host.invalid", 563);

    try testing.expect(!a.ok);
    try testing.expectEqualStrings("dns", a.class);
    // The detail depends on what the resolver answers: not_found for an
    // NXDOMAIN, timeout or other when there is no network.
    const want: []const u8 = if (std.mem.eql(u8, a.detail, "not_found"))
        "the host name does not exist"
    else if (std.mem.eql(u8, a.detail, "timeout"))
        "the host name lookup timed out"
    else
        "the host name could not be resolved";
    try testing.expectEqualStrings(want, a.message);
}
