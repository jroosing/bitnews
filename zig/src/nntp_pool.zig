//! The connection pool of one Server (internal/usenet/nntp pool.go).
//!
//! Up to `max_connections` connections, opened lazily, reused while healthy,
//! closed after `idle_timeout_ms` unused. After a failed dial the pool backs
//! off (min doubling up to max); until one dial has succeeded only one runs at
//! a time, so wrong credentials are tried once, not once per connection. An
//! authentication failure disables the pool: every acquire fails with
//! `ServerDisabled` until `reconfigure` brings a new config. The scheduler
//! treats a disabled Server as dead for the rest of the run.
//!
//! Ownership: `create` returns a heap Pool. Everything it is given is copied.
//! Connections are leased (`acquire` .. `release`/`discard`); a lease must be
//! returned before `destroy`, which waits for that. `close` makes later and
//! waiting acquires fail with `PoolClosed` and closes the idle connections;
//! leased ones are closed when returned.
//!
//! Time is the real clock; tests use millisecond-sized timeouts.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const conn_mod = @import("nntp_conn.zig");
const diag_mod = @import("nntp_diag.zig");
const transport = @import("nntp_transport.zig");
const types = @import("usenet_types.zig");

const Conn = conn_mod.Conn;
const Config = conn_mod.Config;
const Diag = diag_mod.Diag;

pub const default_idle_timeout_ms = 60_000;
pub const default_min_backoff_ms = 1_000;
pub const default_max_backoff_ms = 60_000;
/// The largest `max_connections` of a Server.
pub const max_pool_connections = 100;

pub const ServerErrorKind = enum {
    /// Connecting failed. Reported once per streak of consecutive failures,
    /// not for every retry.
    connect,
    /// The Server rejected the credentials and the pool is now disabled.
    auth,
};

/// What the pool reports through `ErrorHook`. `diag` never holds
/// credentials.
pub const ServerError = struct {
    server: []const u8,
    kind: ServerErrorKind,
    diag: Diag,
};

/// Called outside the pool's lock, on the task that noticed; must not block.
pub const ErrorHook = struct {
    ctx: ?*anyopaque = null,
    call: *const fn (ctx: ?*anyopaque, e: ServerError) void,
};

pub const AcquireError = error{
    PoolClosed,
    /// The Server rejected the credentials (now or earlier).
    ServerDisabled,
    Connection,
    Protocol,
    Canceled,
    OutOfMemory,
};

pub const Options = struct {
    /// Identifies the Server in events.
    name: []const u8,
    /// Clamped to 1 through 100.
    max_connections: i64 = 8,
    config: Config,
    dialer: transport.Dialer,
    idle_timeout_ms: u32 = default_idle_timeout_ms,
    min_backoff_ms: u32 = default_min_backoff_ms,
    max_backoff_ms: u32 = default_max_backoff_ms,
    on_error: ?ErrorHook = null,
};

/// What `reconfigure` changes; null fields keep their value.
pub const Update = struct {
    max_connections: ?i64 = null,
    /// New credentials or endpoint. Applies to every later dial, retires the
    /// idle connections (leased ones on release) and enables a disabled pool
    /// again with a clean backoff.
    config: ?Config = null,
    idle_timeout_ms: ?u32 = null,
    min_backoff_ms: ?u32 = null,
    max_backoff_ms: ?u32 = null,
};

pub const Stats = struct {
    /// Open connections leased and parked.
    in_use: usize,
    idle: usize,
    /// Connection attempts, and the failed ones.
    dials: u64,
    failures: u64,
    /// The Server rejected the credentials.
    disabled: bool,
};

/// A Config with its own strings; the password is wiped when freed.
const OwnedConfig = struct {
    host: []u8,
    user: []u8,
    pass: []u8,
    port: u16,
    connect_timeout_ms: u32,
    read_timeout_ms: u32,

    fn init(gpa: Allocator, c: Config) Allocator.Error!OwnedConfig {
        const host = try gpa.dupe(u8, c.host);
        errdefer gpa.free(host);
        const user = try gpa.dupe(u8, c.user);
        errdefer gpa.free(user);
        const pass = try gpa.dupe(u8, c.pass);
        return .{ .host = host, .user = user, .pass = pass, .port = c.port, .connect_timeout_ms = c.connect_timeout_ms, .read_timeout_ms = c.read_timeout_ms };
    }

    fn deinit(o: OwnedConfig, gpa: Allocator) void {
        gpa.free(o.host);
        std.crypto.secureZero(u8, o.user);
        gpa.free(o.user);
        std.crypto.secureZero(u8, o.pass);
        gpa.free(o.pass);
    }

    fn view(o: *const OwnedConfig) Config {
        return .{ .host = o.host, .port = o.port, .user = o.user, .pass = o.pass, .connect_timeout_ms = o.connect_timeout_ms, .read_timeout_ms = o.read_timeout_ms };
    }
};

const Ns = i96;

const IdleConn = struct { conn: *Conn, since: Ns };

pub const Pool = struct {
    gpa: Allocator,
    io: Io,
    name: []u8,
    dialer: transport.Dialer,
    on_error: ?ErrorHook,

    mutex: Io.Mutex = .init,
    /// Signalled whenever something an acquire waits for changes.
    changed: Io.Condition = .init,
    /// Wakes the janitor when the idle list changes.
    janitor_kick: Io.Condition = .init,
    janitor: ?Io.Future(void) = null,

    // Everything below is guarded by `mutex`.
    config: OwnedConfig,
    max: usize,
    idle_timeout_ms: u32,
    min_backoff_ms: u32,
    max_backoff_ms: u32,
    idle: std.ArrayList(IdleConn) = .empty,
    in_use: usize = 0,
    dialing: usize = 0,
    /// Tasks inside `acquire`, so `destroy` can wait for them.
    waiters: usize = 0,
    dials: u64 = 0,
    total_fails: u64 = 0,
    /// Consecutive dial failures.
    failures: u32 = 0,
    /// Set by a successful dial, cleared by a failure or a new config: until
    /// then only one dial runs at a time.
    proven: bool = false,
    retry_at: Ns = 0,
    disabled: bool = false,
    /// Counts config swaps; connections of an older generation are closed
    /// instead of being reused.
    gen: u32 = 0,
    closed: bool = false,

    pub fn create(gpa: Allocator, io: Io, options: Options) (Allocator.Error || error{ConcurrencyUnavailable})!*Pool {
        const p = try gpa.create(Pool);
        errdefer gpa.destroy(p);
        const name = try gpa.dupe(u8, options.name);
        errdefer gpa.free(name);
        const config = try OwnedConfig.init(gpa, options.config);
        errdefer config.deinit(gpa);
        const min_backoff = if (options.min_backoff_ms == 0) default_min_backoff_ms else options.min_backoff_ms;
        p.* = .{
            .gpa = gpa,
            .io = io,
            .name = name,
            .dialer = options.dialer,
            .on_error = options.on_error,
            .config = config,
            .max = clampMax(options.max_connections),
            .idle_timeout_ms = if (options.idle_timeout_ms == 0) default_idle_timeout_ms else options.idle_timeout_ms,
            .min_backoff_ms = min_backoff,
            .max_backoff_ms = @max(if (options.max_backoff_ms == 0) default_max_backoff_ms else options.max_backoff_ms, min_backoff),
        };
        p.janitor = io.concurrent(janitorTask, .{p}) catch return error.ConcurrencyUnavailable;
        return p;
    }

    /// Closes the pool, waits until every lease is returned and every
    /// acquire has left, and frees it. Cancel the tasks that use it first if
    /// they must not run to completion.
    pub fn destroy(p: *Pool) void {
        const io = p.io;
        p.close();
        p.mutex.lockUncancelable(io);
        while (p.in_use + p.dialing + p.waiters > 0) p.changed.waitUncancelable(io, &p.mutex);
        p.mutex.unlock(io);
        if (p.janitor) |*j| _ = j.cancel(io);
        p.idle.deinit(p.gpa);
        p.config.deinit(p.gpa);
        p.gpa.free(p.name);
        p.gpa.destroy(p);
    }

    /// Closes the idle connections and wakes every waiting acquire with
    /// `PoolClosed`. Safe to call more than once.
    pub fn close(p: *Pool) void {
        const io = p.io;
        p.mutex.lockUncancelable(io);
        if (p.closed) {
            p.mutex.unlock(io);
            return;
        }
        p.closed = true;
        var conns = takeIdle(p);
        p.changed.broadcast(io);
        p.janitor_kick.broadcast(io);
        p.mutex.unlock(io);
        closeAll(p, &conns);
    }

    pub fn stats(p: *Pool) Stats {
        const io = p.io;
        p.mutex.lockUncancelable(io);
        defer p.mutex.unlock(io);
        return .{ .in_use = p.in_use, .idle = p.idle.items.len, .dials = p.dials, .failures = p.total_fails, .disabled = p.disabled };
    }

    /// Applies new settings without a restart (see `Update`). Does nothing on
    /// a closed pool.
    pub fn reconfigure(p: *Pool, update: Update) Allocator.Error!void {
        const io = p.io;
        const new_config: ?OwnedConfig = if (update.config) |c| try OwnedConfig.init(p.gpa, c) else null;
        p.mutex.lockUncancelable(io);
        if (p.closed) {
            p.mutex.unlock(io);
            if (new_config) |n| n.deinit(p.gpa);
            return;
        }
        if (update.max_connections) |m| p.max = clampMax(m);
        if (update.idle_timeout_ms) |t| if (t > 0) {
            p.idle_timeout_ms = t;
        };
        if (update.min_backoff_ms) |t| if (t > 0) {
            p.min_backoff_ms = t;
        };
        if (update.max_backoff_ms) |t| if (t > 0) {
            p.max_backoff_ms = t;
        };
        p.max_backoff_ms = @max(p.max_backoff_ms, p.min_backoff_ms);

        var closing = std.ArrayList(*Conn).empty;
        defer closing.deinit(p.gpa);
        var old_config: ?OwnedConfig = null;
        if (new_config) |n| {
            old_config = p.config;
            p.config = n;
            p.gen += 1;
            p.disabled = false;
            p.failures = 0;
            p.retry_at = 0;
            p.proven = false;
            for (p.idle.items) |ic| closing.append(p.gpa, ic.conn) catch ic.conn.close(io);
            p.idle.clearRetainingCapacity();
        }
        // The oldest idle connections beyond the limit.
        while (p.in_use + p.idle.items.len + p.dialing > p.max and p.idle.items.len > 0) {
            const ic = p.idle.orderedRemove(0);
            closing.append(p.gpa, ic.conn) catch ic.conn.close(io);
        }
        p.janitor_kick.broadcast(io);
        p.changed.broadcast(io);
        p.mutex.unlock(io);
        if (old_config) |o| o.deinit(p.gpa);
        for (closing.items) |c| c.close(io);
    }

    // ---- leasing ----------------------------------------------------------------

    /// One connection checked out of a pool.
    pub const Lease = struct {
        pool: *Pool,
        conn: *Conn,
        gen: u32,

        /// Returns the connection to the pool (closed instead when it is
        /// dead, retired or over the limit).
        pub fn release(l: Lease) void {
            l.pool.giveBack(l.conn, l.gen, false);
        }

        /// Closes the connection instead of returning it, for one that is
        /// suspect even though it does not report dead.
        pub fn discard(l: Lease) void {
            l.pool.giveBack(l.conn, l.gen, true);
        }
    };

    /// Returns a connection, opening one when none is idle and the limit
    /// allows. At the limit it waits for a release. After a failed dial the
    /// caller whose dial failed gets that error, later callers wait out the
    /// backoff before dialing again.
    pub fn acquire(p: *Pool) AcquireError!Lease {
        const io = p.io;
        p.mutex.lockUncancelable(io);
        p.waiters += 1;
        defer {
            // Runs with the mutex held (see the returns below).
            p.waiters -= 1;
            p.changed.broadcast(io);
            p.mutex.unlock(io);
        }
        while (true) {
            if (p.closed) return error.PoolClosed;
            if (p.disabled) return error.ServerDisabled;

            var stale = std.ArrayList(*Conn).empty;
            defer stale.deinit(p.gpa);
            while (p.idle.pop()) |ic| {
                if (ic.conn.isDead()) {
                    stale.append(p.gpa, ic.conn) catch ic.conn.close(io);
                    continue;
                }
                p.in_use += 1;
                const lease: Lease = .{ .pool = p, .conn = ic.conn, .gen = p.gen };
                p.mutex.unlock(io);
                for (stale.items) |c| c.close(io);
                p.mutex.lockUncancelable(io);
                return lease;
            }

            var wait_ms: ?u64 = null;
            if (p.in_use + p.dialing < p.max) {
                const wait_ns = p.retry_at - nowNs(io);
                if ((p.failures > 0 or !p.proven) and p.dialing > 0) {
                    // One probe at a time until a dial succeeds.
                } else if (p.failures > 0 and wait_ns > 0) {
                    wait_ms = @intCast(@divFloor(wait_ns, std.time.ns_per_ms) + 1);
                } else {
                    return p.dial(io, &stale);
                }
            }
            if (stale.items.len > 0) {
                p.mutex.unlock(io);
                for (stale.items) |c| c.close(io);
                stale.clearRetainingCapacity();
                p.mutex.lockUncancelable(io);
                continue;
            }
            if (wait_ms) |ms| {
                p.changed.waitTimeout(io, &p.mutex, .{ .duration = .{ .raw = .fromMilliseconds(@intCast(ms)), .clock = .awake } }) catch |err| switch (err) {
                    error.Timeout => {},
                    error.Canceled => return error.Canceled,
                };
            } else {
                p.changed.wait(io, &p.mutex) catch return error.Canceled;
            }
        }
    }

    /// Opens one connection in a slot reserved here. Called with the mutex
    /// held, returns with it held.
    fn dial(p: *Pool, io: Io, stale: *std.ArrayList(*Conn)) AcquireError!Lease {
        const gpa = p.gpa;
        const cfg = OwnedConfig.init(gpa, p.config.view()) catch return error.OutOfMemory;
        defer cfg.deinit(gpa);
        p.dialing += 1;
        p.dials += 1;
        const gen = p.gen;
        p.mutex.unlock(io);
        for (stale.items) |c| c.close(io);
        stale.clearRetainingCapacity();

        var diag: Diag = .{};
        const result = Conn.connect(gpa, io, p.dialer, cfg.view(), &diag);

        var event: ?ServerError = null;
        p.mutex.lockUncancelable(io);
        p.dialing -= 1;
        var outcome: AcquireError!Lease = undefined;
        var to_close: ?*Conn = null;
        if (result) |conn| {
            if (p.closed) {
                to_close = conn;
                outcome = error.PoolClosed;
            } else if (p.disabled) {
                to_close = conn;
                outcome = error.ServerDisabled;
            } else if (gen != p.gen) {
                // The config changed meanwhile: the connection belongs to the
                // old Server settings, so it only serves this caller.
                p.in_use += 1;
                outcome = .{ .pool = p, .conn = conn, .gen = gen };
            } else {
                p.failures = 0;
                p.retry_at = 0;
                p.proven = true;
                p.in_use += 1;
                outcome = .{ .pool = p, .conn = conn, .gen = gen };
            }
        } else |err| {
            if (err == error.Canceled or gen != p.gen) {
                // The caller gave up, or the config was replaced meanwhile;
                // that says nothing about the current Server settings.
                outcome = mapOpenError(err);
            } else if (err == error.Auth) {
                if (!p.disabled) {
                    p.disabled = true;
                    event = .{ .server = p.name, .kind = .auth, .diag = diag };
                }
                outcome = error.ServerDisabled;
            } else {
                p.failures += 1;
                p.proven = false;
                p.total_fails += 1;
                p.retry_at = nowNs(io) + @as(Ns, p.backoffFor(p.failures)) * std.time.ns_per_ms;
                if (p.failures == 1) event = .{ .server = p.name, .kind = .connect, .diag = diag };
                outcome = mapOpenError(err);
            }
        }
        p.changed.broadcast(io);
        const hook = p.on_error;
        const name = p.name;
        p.mutex.unlock(io);
        if (event) |*e| if (hook) |h| {
            e.server = name;
            h.call(h.ctx, e.*);
        };
        if (to_close) |c| c.close(io);
        p.mutex.lockUncancelable(io);
        return outcome;
    }

    fn mapOpenError(err: conn_mod.OpenError) AcquireError {
        return switch (err) {
            error.Canceled => error.Canceled,
            error.OutOfMemory => error.OutOfMemory,
            error.Protocol => error.Protocol,
            // A bad config fails like an unreachable Server: it retries with
            // backoff until the config changes.
            error.Connection, error.InvalidConfig, error.Auth => error.Connection,
        };
    }

    /// The wait after n consecutive failures.
    fn backoffFor(p: *const Pool, n: u32) u32 {
        var d: u64 = p.min_backoff_ms;
        var i = n;
        while (i > 1 and d < p.max_backoff_ms) : (i -= 1) d *= 2;
        return @intCast(@min(d, p.max_backoff_ms));
    }

    fn giveBack(p: *Pool, conn: *Conn, gen: u32, discard: bool) void {
        const io = p.io;
        p.mutex.lockUncancelable(io);
        p.in_use -= 1;
        // Keep the connection only if it is healthy, current and within the
        // (possibly reduced) limit.
        var drop = discard or conn.isDead() or p.closed or gen != p.gen or
            p.in_use + p.idle.items.len + p.dialing >= p.max;
        if (!drop) {
            p.idle.append(p.gpa, .{ .conn = conn, .since = nowNs(io) }) catch {
                drop = true;
            };
            if (!drop) p.janitor_kick.signal(io);
        }
        p.changed.broadcast(io);
        p.mutex.unlock(io);
        if (drop) conn.close(io);
    }

    // ---- janitor -------------------------------------------------------------------

    /// Closes connections idle for `idle_timeout_ms`. Sleeps until the oldest
    /// one expires, or until the idle list changes.
    fn janitorTask(p: *Pool) void {
        const io = p.io;
        p.mutex.lockUncancelable(io);
        defer p.mutex.unlock(io);
        while (!p.closed) {
            if (p.idle.items.len == 0) {
                p.janitor_kick.wait(io, &p.mutex) catch return;
                continue;
            }
            const expiry = p.idle.items[0].since + @as(Ns, p.idle_timeout_ms) * std.time.ns_per_ms;
            const wait_ns = expiry - nowNs(io);
            if (wait_ns > 0) {
                const ms: i64 = @intCast(@divFloor(wait_ns, std.time.ns_per_ms) + 1);
                p.janitor_kick.waitTimeout(io, &p.mutex, .{ .duration = .{ .raw = .fromMilliseconds(ms), .clock = .awake } }) catch |err| switch (err) {
                    error.Timeout => {},
                    error.Canceled => return,
                };
                continue;
            }
            var expired = std.ArrayList(*Conn).empty;
            defer expired.deinit(p.gpa);
            const now = nowNs(io);
            var n: usize = 0;
            const limit = @as(Ns, p.idle_timeout_ms) * std.time.ns_per_ms;
            while (n < p.idle.items.len and now - p.idle.items[n].since >= limit) n += 1;
            for (p.idle.items[0..n]) |ic| expired.append(p.gpa, ic.conn) catch ic.conn.close(io);
            p.idle.replaceRange(p.gpa, 0, n, &.{}) catch unreachable;
            p.mutex.unlock(io);
            for (expired.items) |c| c.close(io);
            p.mutex.lockUncancelable(io);
        }
    }

    // ---- fetching ---------------------------------------------------------------------

    /// This pool as the scheduler's `Fetcher`. Valid until `destroy`.
    pub fn fetcher(p: *Pool) types.Fetcher {
        return .{ .ctx = p, .body_fn = fetchBody };
    }

    fn fetchBody(ctx: *anyopaque, io: Io, message_id: []const u8, sink: types.Sink) types.FetchError!void {
        const p: *Pool = @ptrCast(@alignCast(ctx));
        return p.body(io, message_id, sink);
    }

    /// Leases a connection, fetches the body of `message_id` into `sink` and
    /// releases the connection. A connection that failed (or whose sink
    /// refused) is closed instead of reused. Does not retry; the scheduler
    /// decides that.
    pub fn body(p: *Pool, io: Io, message_id: []const u8, sink: types.Sink) types.FetchError!void {
        const lease = try p.acquire();
        lease.conn.body(io, message_id, sink) catch |err| {
            switch (err) {
                error.ArticleNotFound, error.InvalidMessageId => lease.release(),
                else => lease.discard(),
            }
            return err;
        };
        lease.release();
    }
};

fn clampMax(n: i64) usize {
    return @intCast(std.math.clamp(n, 1, max_pool_connections));
}

fn nowNs(io: Io) Ns {
    return Io.Clock.Timestamp.now(io, .awake).raw.nanoseconds;
}

fn takeIdle(p: *Pool) std.ArrayList(*Conn) {
    var conns: std.ArrayList(*Conn) = .empty;
    for (p.idle.items) |ic| conns.append(p.gpa, ic.conn) catch ic.conn.close(p.io);
    p.idle.clearRetainingCapacity();
    return conns;
}

fn closeAll(p: *Pool, conns: *std.ArrayList(*Conn)) void {
    for (conns.items) |c| c.close(p.io);
    conns.deinit(p.gpa);
}
