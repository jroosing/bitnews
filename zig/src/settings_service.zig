//! The Settings service (internal/app/settings.go): reads and writes the
//! Settings, keeps them in memory so that the hot paths (authentication, the
//! queues) read them without touching the database, and publishes
//! settings_changed after every write.
//!
//! Memory: the Settings in effect live in a `Snapshot`, an immutable value
//! with its own arena and a reference count. `current()` hands out a counted
//! reference; the reader calls `release()` when done and may keep it as long
//! as it likes. A write builds a new snapshot, swaps the pointer and drops
//! the service's reference to the old one, which is freed when its last
//! reader lets go: a reader never sees a torn value and a write never frees
//! what a reader holds. The pointer swap and the count increment share one
//! short mutex section, so a reader cannot grab a snapshot that is being
//! freed.
//!
//! Writes are serialised by a second mutex, so two changes to different
//! settings never undo each other, and a write is stored before it is shown:
//! if the database refuses it, nothing changed.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const app_error = @import("app_error.zig");
const event = @import("event.zig");
const types = @import("settings_types.zig");
const repo_mod = @import("settings_repo.zig");
const validate = @import("settings_validate.zig").validate;

pub const Settings = types.Settings;
pub const SettingsRepo = repo_mod.SettingsRepo;

/// An immutable Settings value. Never modify `settings`; call `release` once
/// per reference obtained from `current`.
pub const Snapshot = struct {
    settings: Settings,
    arena: std.heap.ArenaAllocator,
    refs: std.atomic.Value(usize),
    gpa: Allocator,

    pub fn release(self: *Snapshot) void {
        if (self.refs.fetchSub(1, .acq_rel) == 1) {
            const gpa = self.gpa;
            self.arena.deinit();
            gpa.destroy(self);
        }
    }
};

/// Unix milliseconds; tests pass a fixed one.
pub const Clock = *const fn (io: Io) i64;

fn realClock(io: Io) i64 {
    return Io.Clock.now(.real, io).toMilliseconds();
}

/// A change: it receives the Settings in effect and a copy to modify (its
/// strings may be allocated in `scratch`). Returning an error stops the
/// change and is returned by `change`; set `fail` and return
/// `error.Rejected` for a failure the client sees.
pub const Change = struct {
    ctx: ?*anyopaque = null,
    run: *const fn (ctx: ?*anyopaque, cur: *const Settings, next: *Settings, scratch: Allocator, fail: *app_error.Failure) anyerror!void,
};

pub const Service = struct {
    gpa: Allocator,
    io: Io,
    repo: SettingsRepo,
    events: event.Publisher,
    clock: Clock = realClock,

    /// Guards `cur` and the reference count taken on it.
    cur_lock: Io.Mutex = .init,
    cur: *Snapshot,
    /// Serialises writers.
    write_lock: Io.Mutex = .init,

    /// Loads the stored Settings. The Service must not move after this
    /// (handlers keep a pointer to it); `deinit` once, after every reader has
    /// released its snapshot.
    pub fn init(gpa: Allocator, io: Io, repo: SettingsRepo, events: event.Publisher, clock: ?Clock) !Service {
        var scratch: std.heap.ArenaAllocator = .init(gpa);
        defer scratch.deinit();
        const loaded = try repo.load(scratch.allocator());
        return .{
            .gpa = gpa,
            .io = io,
            .repo = repo,
            .events = events,
            .clock = clock orelse realClock,
            .cur = try newSnapshot(gpa, loaded),
        };
    }

    pub fn deinit(self: *Service) void {
        self.cur.release();
        self.* = undefined;
    }

    /// The Settings in effect. The caller owns one reference: read
    /// `.settings`, then `release()`. Cheap and safe from any thread.
    pub fn current(self: *Service) *Snapshot {
        self.cur_lock.lockUncancelable(self.io);
        defer self.cur_lock.unlock(self.io);
        _ = self.cur.refs.fetchAdd(1, .monotonic);
        return self.cur;
    }

    /// Replaces every setting with `next`, after validating it. An invalid
    /// value is rejected with code "invalid_settings" and a message that lists
    /// every problem; nothing is stored or published then. `next` and `scratch`
    /// need to outlive the call only.
    pub fn update(self: *Service, scratch: Allocator, fail: *app_error.Failure, next: Settings) anyerror!void {
        const Replace = struct {
            fn run(ctx: ?*anyopaque, _: *const Settings, dst: *Settings, a: Allocator, _: *app_error.Failure) anyerror!void {
                const src: *const Settings = @ptrCast(@alignCast(ctx.?));
                dst.* = try src.clone(a);
            }
        };
        var value = next;
        return self.change(scratch, fail, .{ .ctx = &value, .run = Replace.run });
    }

    /// Applies `c` to a copy of the Settings in effect and stores the result
    /// like `update`, under the write lock, so two changes never undo each
    /// other. `c.run` must not call `update` or `change` (it would wait for
    /// itself); calling `current` is fine.
    pub fn change(self: *Service, scratch: Allocator, fail: *app_error.Failure, c: Change) anyerror!void {
        try self.write_lock.lock(self.io);
        defer self.write_lock.unlock(self.io);

        // Only a writer replaces `cur`, so it can be read without the other lock.
        const old = self.cur;
        var next = try old.settings.clone(scratch);
        try c.run(c.ctx, &old.settings, &next, scratch, fail);

        if (try validate(scratch, next)) |msg| {
            return fail.setLiteral(.invalid_input, "invalid_settings", msg);
        }
        try self.repo.save(scratch, next, self.clock(self.io));

        const fresh = try newSnapshot(self.gpa, next);
        {
            self.cur_lock.lockUncancelable(self.io);
            defer self.cur_lock.unlock(self.io);
            self.cur = fresh;
        }
        old.release();
        self.events.publish(.settings_changed);
    }
};

fn newSnapshot(gpa: Allocator, s: Settings) Allocator.Error!*Snapshot {
    const snap = try gpa.create(Snapshot);
    errdefer gpa.destroy(snap);
    snap.* = .{ .settings = undefined, .arena = .init(gpa), .refs = .init(1), .gpa = gpa };
    errdefer snap.arena.deinit();
    snap.settings = try s.clone(snap.arena.allocator());
    return snap;
}

// ---- tests ----

const testing = std.testing;
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");

const Recorder = struct {
    count: std.atomic.Value(usize) = .init(0),

    fn publish(ctx: *anyopaque, e: event.Event) void {
        const self: *Recorder = @ptrCast(@alignCast(ctx));
        if (e == .settings_changed) _ = self.count.fetchAdd(1, .monotonic);
    }

    fn publisher(self: *Recorder) event.Publisher {
        return .{ .ctx = self, .publish_fn = publish };
    }
};

fn fixedClock(_: Io) i64 {
    return 1_700_000_000_123;
}

const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,
    rec: Recorder = .{},
    svc: Service,

    fn open(rig: *Rig) !void {
        rig.rec = .{};
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
        const path = try std.fmt.bufPrint(&path_buf, "{s}/test.db", .{buf[0..n]});
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
        errdefer rig.store.close();
        {
            const w = try rig.store.acquireWriter();
            defer w.release();
            var diag: migrate.Diagnostic = .{};
            try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
        }
        try rig.start();
    }

    /// (Re)creates the service on the same database, as a restart does.
    fn start(rig: *Rig) !void {
        rig.svc = try Service.init(testing.allocator, testing.io, .{ .store = rig.store }, rig.rec.publisher(), fixedClock);
    }

    fn close(rig: *Rig) void {
        rig.svc.deinit();
        rig.store.close();
        rig.tmp.cleanup();
    }
};

fn queuesUsenet(svc: *Service) i64 {
    const snap = svc.current();
    defer snap.release();
    return snap.settings.queues.usenet;
}

test "settings start as the defaults and load what was stored" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectEqual(1, queuesUsenet(&rig.svc));

    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var fail: app_error.Failure = .{};
    var next = Settings.defaults();
    next.queues.usenet = 3;
    try rig.svc.update(arena.allocator(), &fail, next);

    rig.svc.deinit();
    try rig.start();
    try testing.expectEqual(3, queuesUsenet(&rig.svc));
}

test "update stores, replaces the current settings and publishes settings_changed" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var fail: app_error.Failure = .{};

    var next = Settings.defaults();
    next.queues.torrent = 9;
    next.auth.allowed_hosts = &.{"bitnews.lan"};
    try rig.svc.update(arena.allocator(), &fail, next);

    const snap = rig.svc.current();
    defer snap.release();
    try testing.expectEqual(9, snap.settings.queues.torrent);
    try testing.expectEqualStrings("bitnews.lan", snap.settings.auth.allowed_hosts[0]);
    try testing.expectEqual(1, rig.rec.count.load(.monotonic));

    const r = try rig.store.acquireReader();
    defer r.release();
    var st = try sqlite.Stmt.prepare(r.db, "SELECT updated_at FROM settings WHERE key = 'settings'");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expectEqual(1_700_000_000_123, st.columnInt(0));
}

test "invalid settings are rejected with their code, without changing or publishing" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var fail: app_error.Failure = .{};

    var next = Settings.defaults();
    next.queues.usenet = -1;
    try testing.expectError(error.Rejected, rig.svc.update(arena.allocator(), &fail, next));
    try testing.expectEqual(app_error.Kind.invalid_input, fail.kind);
    try testing.expectEqualStrings("invalid_settings", fail.code);
    try testing.expect(std.mem.indexOf(u8, fail.message, "usenet queue limit") != null);
    try testing.expectEqual(1, queuesUsenet(&rig.svc));
    try testing.expectEqual(0, rig.rec.count.load(.monotonic));

    // Nothing reached the database either.
    rig.svc.deinit();
    try rig.start();
    try testing.expectEqual(1, queuesUsenet(&rig.svc));
}

test "a reader keeps a consistent snapshot across updates" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var fail: app_error.Failure = .{};

    var first = Settings.defaults();
    first.auth.allowed_hosts = &.{"first.lan"};
    try rig.svc.update(arena.allocator(), &fail, first);
    const held = rig.svc.current();

    var second = Settings.defaults();
    second.auth.allowed_hosts = &.{"second.lan"};
    second.paths.usenet.complete = "/other";
    try rig.svc.update(arena.allocator(), &fail, second);

    // The old snapshot is still intact (the test allocator would report a
    // use after free) while the new one is what current() shows.
    try testing.expectEqualStrings("first.lan", held.settings.auth.allowed_hosts[0]);
    try testing.expectEqualStrings("/data/usenet/complete", held.settings.paths.usenet.complete);
    held.release();
    const now = rig.svc.current();
    defer now.release();
    try testing.expectEqualStrings("second.lan", now.settings.auth.allowed_hosts[0]);
}

const Bump = struct {
    fn setUsenet(_: ?*anyopaque, _: *const Settings, next: *Settings, _: Allocator, _: *app_error.Failure) anyerror!void {
        next.queues.usenet = 7;
    }
    fn setPort(_: ?*anyopaque, _: *const Settings, next: *Settings, _: Allocator, _: *app_error.Failure) anyerror!void {
        next.torrent.listen_port = 40000;
    }
    fn refuse(_: ?*anyopaque, _: *const Settings, next: *Settings, _: Allocator, _: *app_error.Failure) anyerror!void {
        next.queues.usenet = 7;
        return error.Refused;
    }
    fn badPort(_: ?*anyopaque, _: *const Settings, next: *Settings, _: Allocator, _: *app_error.Failure) anyerror!void {
        next.torrent.listen_port = 70000;
    }
};

test "a change that fails keeps the settings and publishes nothing" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var fail: app_error.Failure = .{};

    try testing.expectError(error.Refused, rig.svc.change(arena.allocator(), &fail, .{ .run = Bump.refuse }));
    try testing.expectEqual(1, queuesUsenet(&rig.svc));
    try testing.expectEqual(0, rig.rec.count.load(.monotonic));

    try testing.expectError(error.Rejected, rig.svc.change(arena.allocator(), &fail, .{ .run = Bump.badPort }));
    try testing.expectEqualStrings("invalid_settings", fail.code);
    try testing.expectEqual(1, queuesUsenet(&rig.svc));
}

fn changeWorker(svc: *Service, run: *const fn (?*anyopaque, *const Settings, *Settings, Allocator, *app_error.Failure) anyerror!void, errs: *std.atomic.Value(usize)) void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var fail: app_error.Failure = .{};
    svc.change(arena.allocator(), &fail, .{ .run = run }) catch {
        _ = errs.fetchAdd(1, .monotonic);
    };
}

fn readerWorker(svc: *Service, stop: *std.atomic.Value(bool), errs: *std.atomic.Value(usize)) void {
    while (!stop.load(.acquire)) {
        const snap = svc.current();
        defer snap.release();
        // The settings of a snapshot are always whole: paths never change here.
        if (!std.mem.eql(u8, snap.settings.paths.usenet.complete, "/data/usenet/complete")) {
            _ = errs.fetchAdd(1, .monotonic);
        }
    }
}

test "concurrent changes to different settings are all kept, while readers read" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var errs: std.atomic.Value(usize) = .init(0);
    var stop: std.atomic.Value(bool) = .init(false);

    const reader = try std.Thread.spawn(.{}, readerWorker, .{ &rig.svc, &stop, &errs });
    const a = try std.Thread.spawn(.{}, changeWorker, .{ &rig.svc, Bump.setUsenet, &errs });
    const b = try std.Thread.spawn(.{}, changeWorker, .{ &rig.svc, Bump.setPort, &errs });
    a.join();
    b.join();
    stop.store(true, .release);
    reader.join();

    try testing.expectEqual(0, errs.load(.monotonic));
    const snap = rig.svc.current();
    defer snap.release();
    try testing.expectEqual(7, snap.settings.queues.usenet);
    try testing.expectEqual(40000, snap.settings.torrent.listen_port);
    try testing.expectEqual(2, rig.rec.count.load(.monotonic));
}
