//! Tests of the connection pool: Go's pool_test.go, against the fake server
//! of nntptest.zig.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const pool_mod = @import("nntp_pool.zig");
const nntptest = @import("nntptest.zig");
const types = @import("usenet_types.zig");
const diag_mod = @import("nntp_diag.zig");

const Pool = pool_mod.Pool;
const Server = nntptest.Server;

const io = testing.io;

/// Collects what the pool reports.
const Events = struct {
    mutex: Io.Mutex = .init,
    connect: usize = 0,
    auth: usize = 0,
    last_kind: ?diag_mod.Kind = null,

    fn hook(e: *Events) pool_mod.ErrorHook {
        return .{ .ctx = e, .call = call };
    }

    fn call(ctx: ?*anyopaque, ev: pool_mod.ServerError) void {
        const e: *Events = @ptrCast(@alignCast(ctx.?));
        e.mutex.lockUncancelable(io);
        defer e.mutex.unlock(io);
        switch (ev.kind) {
            .connect => e.connect += 1,
            .auth => e.auth += 1,
        }
        e.last_kind = ev.diag.kind;
    }

    fn counts(e: *Events) struct { usize, usize } {
        e.mutex.lockUncancelable(io);
        defer e.mutex.unlock(io);
        return .{ e.connect, e.auth };
    }
};

fn newPool(srv: *Server, tweak: anytype) !*Pool {
    var opts: pool_mod.Options = .{ .name = "test", .config = srv.config(), .dialer = srv.dialer(), .min_backoff_ms = 50, .max_backoff_ms = 400 };
    tweak(&opts);
    return Pool.create(testing.allocator, io, opts);
}

fn noTweak(_: *pool_mod.Options) void {}

const Collect = struct {
    n: usize = 0,
    abort: bool = false,

    fn sink(c: *Collect) types.Sink {
        return .{ .ctx = c, .line_fn = take };
    }

    fn take(ctx: *anyopaque, _: []const u8) error{Abort}!void {
        const c: *Collect = @ptrCast(@alignCast(ctx));
        c.n += 1;
        if (c.abort) return error.Abort;
    }
};

fn ms(t0: Io.Timestamp) i64 {
    return t0.durationTo(Io.Clock.now(.awake, io)).toMilliseconds();
}

test "connects lazily and reuses released connections" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    try testing.expectEqual(@as(usize, 0), srv.dials());
    const a = try p.acquire();
    a.release();
    const b = try p.acquire();
    b.release();
    try testing.expectEqual(@as(usize, 1), srv.dials());
    const s = p.stats();
    try testing.expectEqual(@as(usize, 1), s.idle);
    try testing.expectEqual(@as(usize, 0), s.in_use);
    try testing.expectEqual(@as(u64, 1), s.dials);
}

test "the connection limit is never exceeded under concurrent load" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    try srv.addArticle("a@x", &.{"data"});
    srv.setLatencyMs(5);
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.max_connections = 3;
        }
    }.t);
    defer p.destroy();
    var group: Io.Group = .init;
    const Worker = struct {
        fn run(pool: *Pool) void {
            for (0..8) |_| {
                var c: Collect = .{};
                pool.body(io, "a@x", c.sink()) catch {};
            }
        }
    };
    for (0..10) |_| try group.concurrent(io, Worker.run, .{p});
    try group.await(io);
    try testing.expect(srv.peakConns() <= 3);
    try testing.expectEqual(@as(usize, 80), srv.totalRequests());
    try testing.expect(srv.dials() <= 3);
}

test "acquire waits at the limit until a lease is released" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.max_connections = 1;
        }
    }.t);
    defer p.destroy();
    const first = try p.acquire();
    const Waiter = struct {
        fn run(pool: *Pool) pool_mod.AcquireError!pool_mod.Pool.Lease {
            return pool.acquire();
        }
    };
    var fut = try io.concurrent(Waiter.run, .{p});
    try io.sleep(.fromMilliseconds(100), .awake);
    first.release();
    const second = try fut.await(io);
    try testing.expect(second.conn == first.conn); // the same connection, handed on
    second.release();
    try testing.expectEqual(@as(usize, 1), srv.dials());
}

test "cancelling ends a waiting acquire" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.max_connections = 1;
        }
    }.t);
    defer p.destroy();
    const held = try p.acquire();
    defer held.release();
    const Waiter = struct {
        fn run(pool: *Pool) pool_mod.AcquireError!pool_mod.Pool.Lease {
            return pool.acquire();
        }
    };
    var fut = try io.concurrent(Waiter.run, .{p});
    try io.sleep(.fromMilliseconds(50), .awake);
    const t0 = Io.Clock.now(.awake, io);
    try testing.expectError(error.Canceled, fut.cancel(io));
    try testing.expect(ms(t0) < 1000);
}

test "a dead connection is dropped on release and replaced on the next acquire" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    try srv.addArticle("a@x", &.{"data"});
    try srv.disconnect("a@x", 0, 1);
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    var c: Collect = .{};
    try testing.expectError(error.Connection, p.body(io, "a@x", c.sink()));
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
    try p.body(io, "a@x", c.sink());
    try testing.expectEqual(@as(usize, 2), srv.dials());
}

test "close closes idle connections, refuses later acquires and is idempotent" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    const l = try p.acquire();
    l.release();
    p.close();
    p.close();
    try testing.expect(srv.waitOpen(0, 2000));
    try testing.expectError(error.PoolClosed, p.acquire());
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
}

test "close wakes a waiting acquire with PoolClosed; a leased connection is closed on release" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.max_connections = 1;
        }
    }.t);
    defer p.destroy();
    const held = try p.acquire();
    const Waiter = struct {
        fn run(pool: *Pool) pool_mod.AcquireError!pool_mod.Pool.Lease {
            return pool.acquire();
        }
    };
    var fut = try io.concurrent(Waiter.run, .{p});
    try io.sleep(.fromMilliseconds(50), .awake);
    p.close();
    try testing.expectError(error.PoolClosed, fut.await(io));
    try testing.expectEqual(@as(usize, 1), srv.openConns());
    held.release();
    try testing.expect(srv.waitOpen(0, 2000));
}

test "connections idle for the idle timeout are closed, younger ones stay" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.idle_timeout_ms = 200;
        }
    }.t);
    defer p.destroy();
    const a = try p.acquire();
    const b = try p.acquire();
    a.release();
    try io.sleep(.fromMilliseconds(120), .awake);
    b.release();
    try io.sleep(.fromMilliseconds(130), .awake); // a is 250 ms old, b 130 ms
    try testing.expectEqual(@as(usize, 1), p.stats().idle);
    try testing.expect(srv.waitOpen(1, 2000));
    try testing.expect(srv.waitOpen(0, 3000)); // b goes too
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
}

test "dial failures back off exponentially up to the maximum, and a success resets it" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    srv.failNextDials(3);
    var events: Events = .{};
    const Tweak = struct {
        var ev: *Events = undefined;
        fn t(o: *pool_mod.Options) void {
            o.min_backoff_ms = 100;
            o.max_backoff_ms = 250;
            o.on_error = ev.hook();
        }
    };
    Tweak.ev = &events;
    const p = try newPool(srv, Tweak.t);
    defer p.destroy();
    // 1st: fails at once. 2nd waits 100 ms and fails. 3rd waits 200 ms and fails.
    try testing.expectError(error.Connection, p.acquire());
    var t0 = Io.Clock.now(.awake, io);
    try testing.expectError(error.Connection, p.acquire());
    try testing.expect(ms(t0) >= 90);
    t0 = Io.Clock.now(.awake, io);
    try testing.expectError(error.Connection, p.acquire());
    try testing.expect(ms(t0) >= 190);
    // The 4th waits 250 ms (capped, not 400) and succeeds.
    t0 = Io.Clock.now(.awake, io);
    const l = try p.acquire();
    const waited = ms(t0);
    try testing.expect(waited >= 240 and waited < 400);
    l.release();
    // Reset: after a failure the wait is the minimum again, not the cap.
    const held = try p.acquire(); // the idle connection
    srv.failNextDials(1);
    t0 = Io.Clock.now(.awake, io);
    try testing.expectError(error.Connection, p.acquire());
    try testing.expect(ms(t0) < 80);
    t0 = Io.Clock.now(.awake, io);
    const again = try p.acquire();
    const w2 = ms(t0);
    try testing.expect(w2 >= 90 and w2 < 220);
    again.release();
    held.release();
    try testing.expectEqual(@as(u64, 4), p.stats().failures);
}

test "the connect error is reported once per failure streak" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    srv.failNextDials(3);
    var events: Events = .{};
    const Tweak = struct {
        var ev: *Events = undefined;
        fn t(o: *pool_mod.Options) void {
            o.min_backoff_ms = 20;
            o.max_backoff_ms = 20;
            o.on_error = ev.hook();
        }
    };
    Tweak.ev = &events;
    const p = try newPool(srv, Tweak.t);
    defer p.destroy();
    for (0..3) |_| try testing.expectError(error.Connection, p.acquire());
    try testing.expectEqual(@as(usize, 1), events.counts()[0]);
    const l = try p.acquire();
    l.release();
    srv.failNextDials(1);
    const a = try p.acquire(); // idle connection reused: no dial
    try testing.expectError(error.Connection, p.acquire()); // a new dial fails: a new streak
    a.release();
    try testing.expectEqual(@as(usize, 2), events.counts()[0]);
}

test "an authentication failure disables the pool and stops all dials; a new config enables it" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    try srv.requireAuth("alice", "right");
    var events: Events = .{};
    const Tweak = struct {
        var ev: *Events = undefined;
        fn t(o: *pool_mod.Options) void {
            o.config.pass = "wrong";
            o.on_error = ev.hook();
        }
    };
    Tweak.ev = &events;
    const p = try newPool(srv, Tweak.t);
    defer p.destroy();
    try testing.expectError(error.ServerDisabled, p.acquire());
    try testing.expectError(error.ServerDisabled, p.acquire());
    try testing.expect(p.stats().disabled);
    try testing.expectEqual(@as(usize, 1), srv.authAttempts());
    try testing.expectEqual(@as(usize, 1), events.counts()[1]);
    try testing.expectEqual(diag_mod.Kind.auth, events.last_kind.?);

    // Without a config the pool stays disabled; with one it is enabled again.
    try p.reconfigure(.{ .max_connections = 4 });
    try testing.expectError(error.ServerDisabled, p.acquire());
    try p.reconfigure(.{ .config = srv.config() });
    try testing.expect(!p.stats().disabled);
    const l = try p.acquire();
    l.release();
}

test "wrong credentials are tried once, not once per connection" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    srv.failAuth(true);
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.max_connections = 5;
        }
    }.t);
    defer p.destroy();
    const Worker = struct {
        fn run(pool: *Pool) void {
            if (pool.acquire()) |l| l.release() else |_| {}
        }
    };
    var group: Io.Group = .init;
    for (0..5) |_| try group.concurrent(io, Worker.run, .{p});
    try group.await(io);
    try testing.expectEqual(@as(usize, 1), srv.authAttempts());
}

test "after the first success connections open in parallel up to the limit" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.max_connections = 4;
        }
    }.t);
    defer p.destroy();
    var leases: [4]Pool.Lease = undefined;
    for (&leases) |*l| l.* = try p.acquire();
    try testing.expectEqual(@as(usize, 4), srv.peakConns());
    try testing.expectEqual(@as(usize, 4), p.stats().in_use);
    for (leases) |l| l.release();
    try testing.expectEqual(@as(usize, 4), p.stats().idle);
}

test "a dial the caller cancelled is not a server failure" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    srv.setLatencyMs(0);
    var events: Events = .{};
    const Tweak = struct {
        var ev: *Events = undefined;
        fn t(o: *pool_mod.Options) void {
            o.on_error = ev.hook();
        }
    };
    Tweak.ev = &events;
    // A server that accepts but never greets: the dial hangs until cancelled.
    const scripted = @import("nntp_scripted.zig");
    const hang = try scripted.Script.start(testing.allocator, io, .{ .handler = struct {
        fn run(_: ?*anyopaque, s: *scripted.Session) void {
            s.stall();
        }
    }.run });
    defer hang.stop();
    var cfg = srv.config();
    cfg.read_timeout_ms = 30_000;
    const p = try Pool.create(testing.allocator, io, .{ .name = "hang", .config = cfg, .dialer = hang.dialer(), .on_error = events.hook() });
    defer p.destroy();
    const Waiter = struct {
        fn run(pool: *Pool) pool_mod.AcquireError!pool_mod.Pool.Lease {
            return pool.acquire();
        }
    };
    var fut = try io.concurrent(Waiter.run, .{p});
    try io.sleep(.fromMilliseconds(100), .awake);
    try testing.expectError(error.Canceled, fut.cancel(io));
    try testing.expectEqual(@as(u64, 0), p.stats().failures);
    try testing.expectEqual(@as(usize, 0), events.counts()[0]);
}

test "reconfigure: a lower limit closes idle connections now and leased ones on release" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.max_connections = 4;
        }
    }.t);
    defer p.destroy();
    var leases: [4]Pool.Lease = undefined;
    for (&leases) |*l| l.* = try p.acquire();
    leases[0].release();
    leases[1].release();
    try p.reconfigure(.{ .max_connections = 1 });
    // 2 leased + 2 idle > 1: both idle go at once.
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
    try testing.expect(srv.waitOpen(2, 2000));
    leases[2].release(); // 1 still leased, at the limit: closed
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
    try testing.expect(srv.waitOpen(1, 2000));
    leases[3].release(); // the last one fits
    try testing.expectEqual(@as(usize, 1), p.stats().idle);
}

test "reconfigure: a higher limit lets waiting acquires dial more" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, struct {
        fn t(o: *pool_mod.Options) void {
            o.max_connections = 1;
        }
    }.t);
    defer p.destroy();
    const held = try p.acquire();
    defer held.release();
    const Waiter = struct {
        fn run(pool: *Pool) pool_mod.AcquireError!pool_mod.Pool.Lease {
            return pool.acquire();
        }
    };
    var fut = try io.concurrent(Waiter.run, .{p});
    try io.sleep(.fromMilliseconds(50), .awake);
    try p.reconfigure(.{ .max_connections = 2 });
    const second = try fut.await(io);
    second.release();
    try testing.expectEqual(@as(usize, 2), srv.dials());
}

test "reconfigure with a new config retires old connections and applies to later dials" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    const leased = try p.acquire();
    const parked = try p.acquire();
    parked.release();
    try p.reconfigure(.{ .config = srv.config() });
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
    leased.release(); // an old generation: closed, not parked
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
    const fresh = try p.acquire();
    fresh.release();
    try testing.expectEqual(@as(usize, 3), srv.dials());
    try testing.expectEqual(@as(usize, 1), p.stats().idle);
}

test "max_connections is clamped to 1 through 100" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    for ([_]struct { given: i64, want: usize }{ .{ .given = 0, .want = 1 }, .{ .given = -5, .want = 1 }, .{ .given = 7, .want = 7 }, .{ .given = 1000, .want = 100 } }) |case| {
        const p = try Pool.create(testing.allocator, io, .{ .name = "t", .max_connections = case.given, .config = srv.config(), .dialer = srv.dialer() });
        defer p.destroy();
        try testing.expectEqual(case.want, p.max);
        try p.reconfigure(.{ .max_connections = 1000 });
        try testing.expectEqual(@as(usize, 100), p.max);
    }
}

test "reconfigure after close does nothing" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    p.close();
    try p.reconfigure(.{ .max_connections = 9, .config = srv.config() });
    try testing.expectError(error.PoolClosed, p.acquire());
}

// ---- the fetcher ----------------------------------------------------------------------

test "the fetcher streams the article and keeps the connection for reuse" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    try srv.addArticle("a@x", &.{ "one", "two" });
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    const f = p.fetcher();
    var c: Collect = .{};
    try f.body(io, "a@x", c.sink());
    try f.body(io, "a@x", c.sink());
    try testing.expectEqual(@as(usize, 4), c.n);
    try testing.expectEqual(@as(usize, 1), srv.dials());
    try testing.expectEqual(@as(usize, 1), p.stats().idle);
}

test "the fetcher keeps the connection after not found, drops it after a failure" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    try srv.addArticle("a@x", &.{ "one", "two", "three" });
    try srv.disconnect("a@x", 1, 1);
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    const f = p.fetcher();
    var c: Collect = .{};
    try testing.expectError(error.ArticleNotFound, f.body(io, "nobody@x", c.sink()));
    try testing.expectEqual(@as(usize, 1), p.stats().idle);
    try testing.expectError(error.Connection, f.body(io, "a@x", c.sink()));
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
    try f.body(io, "a@x", c.sink());
    try testing.expectEqual(@as(usize, 2), srv.dials());
}

test "the fetcher drops the connection when the sink refuses, and reports Aborted" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    try srv.addArticle("a@x", &.{ "one", "two", "three" });
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    var c: Collect = .{ .abort = true };
    try testing.expectError(error.Aborted, p.fetcher().body(io, "a@x", c.sink()));
    try testing.expectEqual(@as(usize, 0), p.stats().idle);
    try testing.expectError(error.InvalidMessageId, p.fetcher().body(io, "a b", c.sink()));
    try testing.expectEqual(@as(usize, 1), p.stats().idle); // that one needed no connection use
}

test "the fetcher reports pool-level failures as their class" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    srv.failAuth(true);
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    var c: Collect = .{};
    try testing.expectError(error.ServerDisabled, p.fetcher().body(io, "a@x", c.sink()));
    p.close();
    try testing.expectError(error.PoolClosed, p.fetcher().body(io, "a@x", c.sink()));
}

test "a connect failure reaches the fetcher as Connection" {
    const srv = try Server.start(testing.allocator, io);
    defer srv.stop();
    srv.failNextDials(1);
    const p = try newPool(srv, noTweak);
    defer p.destroy();
    var c: Collect = .{};
    try testing.expectError(error.Connection, p.fetcher().body(io, "a@x", c.sink()));
}
