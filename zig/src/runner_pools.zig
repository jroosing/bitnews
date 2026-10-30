//! One connection pool per enabled Server, shared by all Downloads
//! (internal/usenet/runner pools.go).
//!
//! `sync` makes the pools match the configured Servers: pools of new enabled
//! Servers are created, those of removed or disabled Servers are closed, and
//! a pool whose connection settings changed is reconfigured in place. It
//! reports whether anything the scheduler depends on changed (the set of
//! Servers, priority, retention, connection count), so running fetches restart
//! their scheduler.
//!
//! Lifetime. A fetch takes a `snapshot` of the active Servers and `release`s
//! it when its scheduler run is over. A pool that leaves the set is closed at
//! once (its workers see `PoolClosed` and count the Server as dead) but
//! destroyed only when no snapshot uses it any more, so a worker never calls
//! into freed memory. The owner of a PoolSet is the Runner; `close` waits for
//! nothing, the Runner has stopped every fetch before it calls it.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const log = @import("log.zig");
const nntp = @import("nntp.zig");
const seams = @import("runner_seams.zig");
const types = @import("usenet_types.zig");

pub const ServerSpec = seams.ServerSpec;

/// A ServerSpec with strings of its own; the password is wiped when freed.
const OwnedSpec = struct {
    name: []u8,
    host: []u8,
    user: []u8,
    pass: []u8,
    port: u16,
    priority: i64,
    max_connections: i64,
    retention_days: i64,
    enabled: bool,

    fn init(gpa: Allocator, s: ServerSpec) Allocator.Error!OwnedSpec {
        const name = try gpa.dupe(u8, s.name);
        errdefer gpa.free(name);
        const host = try gpa.dupe(u8, s.host);
        errdefer gpa.free(host);
        const user = try gpa.dupe(u8, s.user);
        errdefer gpa.free(user);
        const pass = try gpa.dupe(u8, s.pass);
        return .{
            .name = name,
            .host = host,
            .user = user,
            .pass = pass,
            .port = s.port,
            .priority = s.priority,
            .max_connections = s.max_connections,
            .retention_days = s.retention_days,
            .enabled = s.enabled,
        };
    }

    fn deinit(o: OwnedSpec, gpa: Allocator) void {
        gpa.free(o.name);
        gpa.free(o.host);
        std.crypto.secureZero(u8, o.user);
        gpa.free(o.user);
        std.crypto.secureZero(u8, o.pass);
        gpa.free(o.pass);
    }

    fn view(o: *const OwnedSpec) ServerSpec {
        return .{
            .name = o.name,
            .host = o.host,
            .port = o.port,
            .user = o.user,
            .pass = o.pass,
            .priority = o.priority,
            .max_connections = o.max_connections,
            .retention_days = o.retention_days,
            .enabled = o.enabled,
        };
    }

    fn clientConfig(o: *const OwnedSpec) nntp.Config {
        return .{ .host = o.host, .port = o.port, .user = o.user, .pass = o.pass };
    }
};

const Entry = struct {
    spec: OwnedSpec,
    pool: *nntp.Pool,
    /// Snapshots that use the pool.
    users: std.atomic.Value(u32) = .init(0),
};

/// One Server of a snapshot, as the scheduler wants it. `name` is allocated in
/// the arena of the snapshot.
pub const Active = struct {
    name: []const u8,
    priority: i64,
    retention_days: u32,
    workers: u32,
    fetcher: types.Fetcher,
    entry: *Entry,
};

pub const PoolSet = struct {
    gpa: Allocator,
    io: Io,
    dialer: nntp.Dialer,
    log: ?log.Logger,

    mu: Io.Mutex = .init,
    // Guarded by `mu`.
    entries: std.ArrayList(*Entry) = .empty,
    retired: std.ArrayList(*Entry) = .empty,
    closed: bool = false,

    pub fn init(gpa: Allocator, io: Io, dialer: nntp.Dialer, logger: ?log.Logger) PoolSet {
        return .{ .gpa = gpa, .io = io, .dialer = dialer, .log = logger };
    }

    /// Closes and frees every pool. No snapshot may be in use.
    pub fn deinit(s: *PoolSet) void {
        s.mu.lockUncancelable(s.io);
        s.closed = true;
        var all: std.ArrayList(*Entry) = .empty;
        all.appendSlice(s.gpa, s.entries.items) catch {};
        all.appendSlice(s.gpa, s.retired.items) catch {};
        s.entries.clearAndFree(s.gpa);
        s.retired.clearAndFree(s.gpa);
        s.mu.unlock(s.io);
        for (all.items) |e| s.destroyEntry(e);
        all.deinit(s.gpa);
    }

    fn destroyEntry(s: *PoolSet, e: *Entry) void {
        e.pool.destroy();
        e.spec.deinit(s.gpa);
        s.gpa.destroy(e);
    }

    fn onError(ctx: ?*anyopaque, e: nntp.pool.ServerError) void {
        const s: *PoolSet = @ptrCast(@alignCast(ctx.?));
        const l = s.log orelse return;
        l.warn("usenet server error", &.{
            log.Attr.string("server", e.server),
            log.Attr.string("kind", @tagName(e.kind)),
            log.Attr.string("err", e.diag.text()),
        });
    }

    fn newEntry(s: *PoolSet, spec: ServerSpec) !*Entry {
        const owned = try OwnedSpec.init(s.gpa, spec);
        errdefer owned.deinit(s.gpa);
        const e = try s.gpa.create(Entry);
        errdefer s.gpa.destroy(e);
        const pool = try nntp.Pool.create(s.gpa, s.io, .{
            .name = owned.name,
            .max_connections = owned.max_connections,
            .config = owned.clientConfig(),
            .dialer = s.dialer,
            .on_error = .{ .ctx = s, .call = onError },
        });
        e.* = .{ .spec = owned, .pool = pool };
        return e;
    }

    /// Makes the pools match `specs`. Reports whether the running schedulers
    /// must restart. A Server whose pool cannot be created is skipped (and
    /// logged); the next call tries again.
    pub fn sync(s: *PoolSet, specs: []const ServerSpec) bool {
        var changed = false;
        s.mu.lockUncancelable(s.io);
        defer s.mu.unlock(s.io);
        if (s.closed) return false;

        // Entries whose Server is gone or disabled.
        var i: usize = 0;
        while (i < s.entries.items.len) {
            const e = s.entries.items[i];
            if (find(specs, e.spec.name) != null) {
                i += 1;
                continue;
            }
            _ = s.entries.swapRemove(i);
            changed = true;
            e.pool.close();
            s.retired.append(s.gpa, e) catch {
                // Cannot remember it: destroy now only when nobody uses it.
                if (e.users.load(.acquire) == 0) s.destroyEntry(e);
            };
        }

        for (specs) |spec| {
            if (!spec.enabled) continue;
            if (s.entryOf(spec.name)) |e| {
                if (e.spec.view().eql(spec)) continue;
                const o = e.spec.view();
                if (!std.mem.eql(u8, o.host, spec.host) or o.port != spec.port or !std.mem.eql(u8, o.user, spec.user) or
                    !std.mem.eql(u8, o.pass, spec.pass) or o.max_connections != spec.max_connections)
                {
                    // Own the new strings first; the pool copies its own.
                    const fresh = OwnedSpec.init(s.gpa, spec) catch {
                        s.logWarn("usenet server change not applied: out of memory", spec.name);
                        continue;
                    };
                    e.pool.reconfigure(.{ .max_connections = spec.max_connections, .config = fresh.clientConfig() }) catch {
                        fresh.deinit(s.gpa);
                        s.logWarn("usenet server change not applied: out of memory", spec.name);
                        continue;
                    };
                    e.spec.deinit(s.gpa);
                    e.spec = fresh;
                    changed = true;
                    continue;
                }
                // Priority, retention or enabled flag only.
                changed = true;
                const fresh = OwnedSpec.init(s.gpa, spec) catch continue;
                e.spec.deinit(s.gpa);
                e.spec = fresh;
                continue;
            }
            const e = s.newEntry(spec) catch |err| {
                s.logErr("usenet server pool not created", spec.name, err);
                continue;
            };
            s.entries.append(s.gpa, e) catch {
                s.destroyEntry(e);
                continue;
            };
            changed = true;
        }
        s.reapRetiredLocked();
        return changed;
    }

    fn find(specs: []const ServerSpec, name: []const u8) ?usize {
        for (specs, 0..) |sp, i| {
            if (sp.enabled and std.mem.eql(u8, sp.name, name)) return i;
        }
        return null;
    }

    fn entryOf(s: *PoolSet, name: []const u8) ?*Entry {
        for (s.entries.items) |e| if (std.mem.eql(u8, e.spec.name, name)) return e;
        return null;
    }

    fn logWarn(s: *PoolSet, msg: []const u8, server: []const u8) void {
        if (s.log) |l| l.warn(msg, &.{log.Attr.string("server", server)});
    }

    fn logErr(s: *PoolSet, msg: []const u8, server: []const u8, err: anyerror) void {
        if (s.log) |l| l.warn(msg, &.{ log.Attr.string("server", server), log.Attr.string("err", @errorName(err)) });
    }

    /// Whether any Server is enabled.
    pub fn anyActive(s: *PoolSet) bool {
        s.mu.lockUncancelable(s.io);
        defer s.mu.unlock(s.io);
        return s.entries.items.len > 0;
    }

    /// The enabled Servers, ordered by priority then name, for one scheduler
    /// run. The slice and the names are allocated in `arena`. Every call must
    /// be paired with `release` once the run is over.
    pub fn snapshot(s: *PoolSet, arena: Allocator) Allocator.Error![]Active {
        s.mu.lockUncancelable(s.io);
        defer s.mu.unlock(s.io);
        const out = try arena.alloc(Active, s.entries.items.len);
        for (s.entries.items, out) |e, *a| {
            a.* = .{
                .name = try arena.dupe(u8, e.spec.name),
                .priority = e.spec.priority,
                .retention_days = @intCast(std.math.clamp(e.spec.retention_days, 0, std.math.maxInt(u32))),
                .workers = @intCast(std.math.clamp(e.spec.max_connections, 1, 100)),
                .fetcher = e.pool.fetcher(),
                .entry = e,
            };
        }
        // Counted only once nothing can fail any more.
        for (out) |a| _ = a.entry.users.fetchAdd(1, .acq_rel);
        std.mem.sort(Active, out, {}, struct {
            fn less(_: void, x: Active, y: Active) bool {
                if (x.priority != y.priority) return x.priority < y.priority;
                return std.mem.lessThan(u8, x.name, y.name);
            }
        }.less);
        return out;
    }

    pub fn release(s: *PoolSet, snap: []const Active) void {
        for (snap) |a| _ = a.entry.users.fetchSub(1, .acq_rel);
        s.mu.lockUncancelable(s.io);
        defer s.mu.unlock(s.io);
        s.reapRetiredLocked();
    }

    /// Destroys retired pools that nobody uses. Destroying waits for leases,
    /// which are all returned: no worker of a finished run holds one.
    fn reapRetiredLocked(s: *PoolSet) void {
        var i: usize = 0;
        while (i < s.retired.items.len) {
            const e = s.retired.items[i];
            if (e.users.load(.acquire) != 0) {
                i += 1;
                continue;
            }
            _ = s.retired.swapRemove(i);
            s.destroyEntry(e);
        }
    }

    /// Re-enables pools that an authentication failure disabled, for a retry
    /// after the failure backoff. Only a new config does that, so every
    /// disabled pool gets its own again.
    pub fn revive(s: *PoolSet) void {
        s.mu.lockUncancelable(s.io);
        defer s.mu.unlock(s.io);
        for (s.entries.items) |e| {
            if (!e.pool.stats().disabled) continue;
            e.pool.reconfigure(.{ .config = e.spec.clientConfig() }) catch {};
        }
    }
};

// ---- tests ----

const testing = std.testing;
const nntptest = @import("nntptest.zig");

/// A dialer that reaches one fake server, whatever host is asked for.
fn setUp() !*nntptest.Server {
    return nntptest.Server.start(testing.allocator, testing.io);
}

fn mk(name: []const u8, priority: i64, conns: i64) ServerSpec {
    return .{ .name = name, .host = "h.example", .user = "u", .pass = "p", .priority = priority, .max_connections = conns };
}

test "sync reports what the schedulers depend on, and only that" {
    const srv = try setUp();
    defer srv.stop();
    var set: PoolSet = .init(testing.allocator, testing.io, srv.dialer(), null);
    defer set.deinit();

    try testing.expect(!set.anyActive());
    try testing.expect(set.sync(&.{ mk("a", 0, 2), mk("b", 1, 3) }));
    try testing.expect(set.anyActive());
    try testing.expect(!set.sync(&.{ mk("a", 0, 2), mk("b", 1, 3) })); // nothing changed
    try testing.expect(set.sync(&.{ mk("a", 5, 2), mk("b", 1, 3) })); // priority
    try testing.expect(set.sync(&.{ mk("a", 5, 4), mk("b", 1, 3) })); // connections
    var changed = mk("a", 5, 4);
    changed.retention_days = 30;
    try testing.expect(set.sync(&.{ changed, mk("b", 1, 3) })); // retention
    var moved = changed;
    moved.pass = "new";
    try testing.expect(set.sync(&.{ moved, mk("b", 1, 3) })); // credentials
    var off = mk("b", 1, 3);
    off.enabled = false;
    try testing.expect(set.sync(&.{ moved, off })); // disabled: gone
    try testing.expect(!set.sync(&.{ moved, off }));
    try testing.expect(set.sync(&.{})); // all gone
    try testing.expect(!set.anyActive());
}

test "a snapshot lists the Servers by priority then name and keeps a removed pool alive until released" {
    const srv = try setUp();
    defer srv.stop();
    var set: PoolSet = .init(testing.allocator, testing.io, srv.dialer(), null);
    defer set.deinit();
    _ = set.sync(&.{ mk("zeta", 0, 2), mk("beta", 1, 2), mk("alpha", 1, 7) });
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();

    const snap = try set.snapshot(arena.allocator());

    try testing.expectEqual(@as(usize, 3), snap.len);
    try testing.expectEqualStrings("zeta", snap[0].name);
    try testing.expectEqualStrings("alpha", snap[1].name);
    try testing.expectEqualStrings("beta", snap[2].name);
    try testing.expectEqual(@as(u32, 7), snap[1].workers);

    // The Server goes away while a run uses it: its pool is closed, not freed.
    try testing.expect(set.sync(&.{ mk("beta", 1, 2), mk("alpha", 1, 7) }));
    try testing.expectEqual(@as(usize, 1), set.retired.items.len);
    const Sink = struct {
        fn line(_: *anyopaque, _: []const u8) error{Abort}!void {}
    };
    var dummy: u8 = 0;
    try testing.expectError(error.PoolClosed, snap[0].fetcher.body(testing.io, "x@y", .{ .ctx = &dummy, .line_fn = Sink.line }));
    set.release(snap);
    try testing.expectEqual(@as(usize, 0), set.retired.items.len);
}

test "revive gives a disabled pool its credentials again" {
    const srv = try setUp();
    defer srv.stop();
    srv.failAuth(true);
    var set: PoolSet = .init(testing.allocator, testing.io, srv.dialer(), null);
    defer set.deinit();
    _ = set.sync(&.{mk("a", 0, 1)});
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const snap = try set.snapshot(arena.allocator());
    defer set.release(snap);
    const Sink = struct {
        fn line(_: *anyopaque, _: []const u8) error{Abort}!void {}
    };
    var dummy: u8 = 0;
    const sink: types.Sink = .{ .ctx = &dummy, .line_fn = Sink.line };
    // The pool reports a refused login as a disabled Server, from the first call on.
    try testing.expectError(error.ServerDisabled, snap[0].fetcher.body(testing.io, "x@y", sink));
    try testing.expect(set.entries.items[0].pool.stats().disabled);
    try testing.expectError(error.ServerDisabled, snap[0].fetcher.body(testing.io, "x@y", sink));

    srv.failAuth(false);
    set.revive();

    try testing.expectError(error.ArticleNotFound, snap[0].fetcher.body(testing.io, "x@y", sink));
}
