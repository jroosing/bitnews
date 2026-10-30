//! The Usenet Server service (internal/app/server.go) without the connection
//! prober: create, get, list, update (patch) and delete, with validation,
//! defaults and the codes of the API. The connection tests come with the NNTP
//! client (Z3).
//!
//! Memory: `arena` is the request's arena. Results, failure messages and the
//! temporary copies live there; a result's `deinit(arena)` wipes the password
//! before the arena reuses the memory. A refusal is `error.Rejected` with the
//! details in the `Failure` the caller passed.

const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
const Key = @import("keyfile.zig").Key;
const Secret = @import("secret.zig").Secret;
const id_mod = @import("id.zig");
const app_error = @import("app_error.zig");
const Failure = app_error.Failure;
const types = @import("servers_types.zig");
const Server = types.Server;
const repo_mod = @import("servers_repo.zig");
const ServerRepo = repo_mod.ServerRepo;

pub const Error = repo_mod.Error;

/// What a client gives to create a Server. Zero values take the defaults:
/// port 563, 8 connections, enabled. All strings are borrowed.
pub const Spec = struct {
    name: []const u8 = "",
    host: []const u8 = "",
    port: i64 = 0,
    username: []const u8 = "",
    password: []const u8 = "",
    max_connections: i64 = 0,
    retention_days: i64 = 0,
    priority: i64 = 0,
    /// Defaults to true when null.
    enabled: ?bool = null,
};

/// The changes `update` applies; null fields are left alone. A null password
/// keeps the stored one. All strings are borrowed.
pub const Patch = struct {
    name: ?[]const u8 = null,
    host: ?[]const u8 = null,
    port: ?i64 = null,
    username: ?[]const u8 = null,
    password: ?[]const u8 = null,
    max_connections: ?i64 = null,
    retention_days: ?i64 = null,
    priority: ?i64 = null,
    enabled: ?bool = null,

    pub fn isEmpty(p: Patch) bool {
        inline for (comptime std.meta.fieldNames(Patch)) |name| {
            if (@field(p, name) != null) return false;
        }
        return true;
    }
};

/// Called after every change of the Servers (Go: core.ServersChanged), so the
/// runner can reconfigure its connection pools and SSE clients hear of it. The
/// event bus of the foundation has no such event yet; the lead wires this to
/// it. `notify` must not block.
pub const Notifier = struct {
    ctx: *anyopaque,
    notify_fn: *const fn (ctx: *anyopaque) void,

    pub fn notify(n: Notifier) void {
        n.notify_fn(n.ctx);
    }

    var discard_ctx: u8 = 0;
    fn discardFn(_: *anyopaque) void {}
    pub const discard: Notifier = .{ .ctx = &discard_ctx, .notify_fn = discardFn };
};

/// Unix milliseconds; tests pass their own.
pub const Clock = *const fn (io: Io) i64;

pub fn realClock(io: Io) i64 {
    return Io.Clock.now(.real, io).toMilliseconds();
}

pub const ServerService = struct {
    repo: ServerRepo,
    notifier: Notifier,
    io: Io,
    clock: Clock,
    /// Serializes the read-modify-write cycle of `update`.
    mutex: Io.Mutex = .init,

    /// `key` is the master key; it must outlive the service. The service must
    /// not move once its address is given to the routes.
    pub fn init(io: Io, store: *sqlite.Store, key: *const Key, notifier: Notifier, clock: Clock) ServerService {
        return .{ .repo = .{ .store = store, .key = key }, .notifier = notifier, .io = io, .clock = clock };
    }

    /// Every Server ordered by priority, then name. Servers whose password
    /// cannot be opened are included with `password_unavailable` set.
    pub fn list(self: *ServerService, arena: Allocator) Error![]Server {
        return self.repo.list(arena);
    }

    /// `error.NotFound` for an unknown ID.
    pub fn get(self: *ServerService, arena: Allocator, id: []const u8) Error!Server {
        return self.repo.get(arena, id);
    }

    /// Validates and stores a new Server. A name that is already taken is a
    /// conflict (code "name_taken").
    pub fn create(self: *ServerService, arena: Allocator, spec: Spec, f: *Failure) (Error || error{Rejected})!Server {
        var srv = try fromSpec(arena, spec);
        errdefer srv.deinit(arena);
        srv.id = try arena.dupe(u8, &id_mod.new(self.io));
        srv.created_at_ms = self.clock(self.io);
        try validate(arena, srv, f);
        self.repo.create(arena, srv) catch |err| return writeError(err, f);
        self.notifier.notify();
        return srv;
    }

    /// Validates and stores the changes to a Server. Without a password in the
    /// patch the stored password stays, even one that cannot be opened.
    pub fn update(self: *ServerService, arena: Allocator, id: []const u8, patch: Patch, f: *Failure) (Error || error{Rejected})!Server {
        if (patch.isEmpty()) return f.setLiteral(.invalid_input, "empty_patch", "nothing to change: give at least one field");
        try self.mutex.lock(self.io);
        defer self.mutex.unlock(self.io);
        var srv = try self.repo.get(arena, id);
        errdefer srv.deinit(arena);
        if (patch.name) |v| try replace(arena, &srv.name, types.trimSpace(v));
        if (patch.host) |v| try replace(arena, &srv.host, types.trimSpace(v));
        if (patch.username) |v| try replace(arena, &srv.username, v);
        if (patch.port) |v| srv.port = v;
        if (patch.max_connections) |v| srv.max_connections = v;
        if (patch.retention_days) |v| srv.retention_days = v;
        if (patch.priority) |v| srv.priority = v;
        if (patch.enabled) |v| srv.enabled = v;
        if (patch.password) |v| {
            if (srv.password) |*old| old.deinit();
            srv.password = try Secret.init(arena, v);
            srv.password_unavailable = false;
        }
        try validate(arena, srv, f);
        self.repo.update(arena, srv) catch |err| return writeError(err, f);
        self.notifier.notify();
        return srv;
    }

    /// `error.NotFound` for an unknown ID.
    pub fn delete(self: *ServerService, id: []const u8) Error!void {
        try self.repo.delete(id);
        self.notifier.notify();
    }
};

/// An owned Server with the defaults applied and the name and host trimmed.
fn fromSpec(arena: Allocator, spec: Spec) Allocator.Error!Server {
    var srv: Server = .{
        .port = if (spec.port == 0) types.default_port else spec.port,
        .max_connections = if (spec.max_connections == 0) types.default_max_connections else spec.max_connections,
        .retention_days = spec.retention_days,
        .priority = spec.priority,
        .enabled = spec.enabled orelse true,
    };
    srv.id = try arena.dupe(u8, "");
    errdefer arena.free(srv.id);
    srv.name = try arena.dupe(u8, types.trimSpace(spec.name));
    errdefer arena.free(srv.name);
    srv.host = try arena.dupe(u8, types.trimSpace(spec.host));
    errdefer arena.free(srv.host);
    srv.username = try arena.dupe(u8, spec.username);
    errdefer arena.free(srv.username);
    if (spec.password.len > 0) srv.password = try Secret.init(arena, spec.password);
    return srv;
}

fn replace(arena: Allocator, field: *[]const u8, value: []const u8) Allocator.Error!void {
    const copy = try arena.dupe(u8, value);
    arena.free(field.*);
    field.* = copy;
}

/// The code is the first problem's; the message lists all of them. Validation
/// messages never contain credentials.
fn validate(arena: Allocator, srv: Server, f: *Failure) (error{Rejected} || Allocator.Error)!void {
    const problems = types.check(srv);
    if (!problems.any()) return;
    var aw: Io.Writer.Allocating = .init(arena);
    types.writeProblems(&aw.writer, srv, problems) catch return error.OutOfMemory;
    return f.setLiteral(.invalid_input, problems.firstCode(), aw.written());
}

fn writeError(err: Error, f: *Failure) (Error || error{Rejected}) {
    if (err == error.Conflict) return f.setLiteral(.conflict, "name_taken", "a server with this name already exists");
    return err;
}

// ---- tests ----

const testing = std.testing;
const migrate = @import("migrate.zig");

var test_now: i64 = 1_700_086_400_000;
fn testClock(_: Io) i64 {
    return test_now;
}

const Recorder = struct {
    count: usize = 0,
    fn notify(ctx: *anyopaque) void {
        const r: *Recorder = @ptrCast(@alignCast(ctx));
        r.count += 1;
    }
    fn notifier(r: *Recorder) Notifier {
        return .{ .ctx = r, .notify_fn = notify };
    }
};

const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,
    key: Key,
    rec: Recorder = .{},
    svc: ServerService,
    arena_state: std.heap.ArenaAllocator,

    fn open(rig: *Rig) !void {
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        const path = try std.fmt.allocPrint(testing.allocator, "{s}/test.db", .{buf[0..n]});
        defer testing.allocator.free(path);
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
        errdefer rig.store.close();
        {
            const w = try rig.store.acquireWriter();
            defer w.release();
            var diag: migrate.Diagnostic = .{};
            try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
        }
        rig.key = .{ .bytes = @splat(7) };
        rig.rec = .{};
        rig.svc = ServerService.init(testing.io, rig.store, &rig.key, rig.rec.notifier(), testClock);
        rig.arena_state = .init(testing.allocator);
    }

    fn close(rig: *Rig) void {
        rig.arena_state.deinit();
        rig.key.deinit();
        rig.store.close();
        rig.tmp.cleanup();
    }

    fn arena(rig: *Rig) Allocator {
        return rig.arena_state.allocator();
    }

    /// A service on the same database with another key (the daemon restarted
    /// after the key file was lost).
    fn withOtherKey(rig: *Rig, other: *const Key) ServerService {
        return ServerService.init(testing.io, rig.store, other, rig.rec.notifier(), testClock);
    }
};

fn mkSpec(name: []const u8) Spec {
    return .{ .name = name, .host = "news.example.com", .username = "joe", .password = "hunter2", .max_connections = 10 };
}

test "create applies the defaults and stores the Server" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};

    const got = try rig.svc.create(rig.arena(), .{ .name = "  main ", .host = " news.example.com ", .username = "joe", .password = "hunter2" }, &f);

    try testing.expectEqual(id_mod.length, got.id.len);
    try testing.expectEqualStrings("main", got.name);
    try testing.expectEqualStrings("news.example.com", got.host);
    try testing.expectEqual(@as(i64, 563), got.port);
    try testing.expectEqual(@as(i64, 8), got.max_connections);
    try testing.expect(got.enabled);
    try testing.expectEqual(test_now, got.created_at_ms);
    const stored = try rig.svc.get(rig.arena(), got.id);
    try testing.expectEqualStrings("hunter2", stored.passwordBytes());
    try testing.expectEqual(@as(usize, 1), rig.rec.count);
}

test "an explicit enabled=false is honoured" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    var sp = mkSpec("main");
    sp.enabled = false;
    const got = try rig.svc.create(rig.arena(), sp, &f);
    try testing.expect(!got.enabled);
}

test "create rejects invalid input with the code of the first problem" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const Case = struct { mut: *const fn (*Spec) void, code: []const u8 };
    const cases = [_]Case{
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.name = "";
            }
        }.f, .code = "invalid_name" },
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.host = "nntps://news.example.com";
            }
        }.f, .code = "invalid_host" },
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.host = "";
            }
        }.f, .code = "invalid_host" },
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.port = 70000;
            }
        }.f, .code = "invalid_port" },
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.max_connections = 101;
            }
        }.f, .code = "invalid_max_connections" },
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.retention_days = -1;
            }
        }.f, .code = "invalid_retention_days" },
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.priority = -1;
            }
        }.f, .code = "invalid_priority" },
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.username = "a\nb";
            }
        }.f, .code = "invalid_credentials" },
        .{ .mut = struct {
            fn f(s: *Spec) void {
                s.name = "";
                s.host = "";
            }
        }.f, .code = "invalid_name" },
    };
    for (cases) |c| {
        var sp = mkSpec("main");
        c.mut(&sp);
        var f: Failure = .{};
        try testing.expectError(error.Rejected, rig.svc.create(rig.arena(), sp, &f));
        try testing.expectEqual(app_error.Kind.invalid_input, f.kind);
        try testing.expectEqualStrings(c.code, f.code);
    }
    try testing.expectEqual(@as(usize, 0), rig.rec.count);
    try testing.expectEqual(@as(usize, 0), (try rig.svc.list(rig.arena())).len);
}

test "a validation message lists every problem and never shows the password" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    var sp = mkSpec("");
    sp.host = "";
    sp.password = "hunter2-secret";

    try testing.expectError(error.Rejected, rig.svc.create(rig.arena(), sp, &f));

    try testing.expectEqualStrings(
        "server name must be 1 to 64 characters without control characters; " ++
            "server host must be a host name or IP address, without scheme, port or path: \"\"",
        f.message,
    );
    try testing.expect(std.mem.indexOf(u8, f.message, "hunter2") == null);
}

test "a taken name is a conflict named name_taken, and publishes nothing" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    _ = try rig.svc.create(rig.arena(), mkSpec("main"), &f);
    try testing.expectEqual(@as(usize, 1), rig.rec.count);

    try testing.expectError(error.Rejected, rig.svc.create(rig.arena(), mkSpec("main"), &f));

    try testing.expectEqual(app_error.Kind.conflict, f.kind);
    try testing.expectEqualStrings("name_taken", f.code);
    try testing.expectEqualStrings("a server with this name already exists", f.message);
    try testing.expectError(error.NotFound, rig.svc.delete("nope"));
    try testing.expectEqual(@as(usize, 1), rig.rec.count);
}

test "every successful change notifies" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    const created = try rig.svc.create(rig.arena(), mkSpec("main"), &f);
    _ = try rig.svc.update(rig.arena(), created.id, .{ .name = "renamed" }, &f);
    try rig.svc.delete(created.id);
    try testing.expectEqual(@as(usize, 3), rig.rec.count);
}

test "update without a password keeps the stored one" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    const created = try rig.svc.create(rig.arena(), mkSpec("main"), &f);

    const got = try rig.svc.update(rig.arena(), created.id, .{ .host = " other.example.org ", .port = 443 }, &f);

    try testing.expectEqualStrings("other.example.org", got.host);
    try testing.expectEqual(@as(i64, 443), got.port);
    const stored = try rig.svc.get(rig.arena(), created.id);
    try testing.expectEqualStrings("hunter2", stored.passwordBytes());
    try testing.expectEqualStrings("joe", stored.username);
}

test "update with a password replaces it, an empty one removes it" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    const created = try rig.svc.create(rig.arena(), mkSpec("main"), &f);

    _ = try rig.svc.update(rig.arena(), created.id, .{ .password = "new-pass" }, &f);
    try testing.expectEqualStrings("new-pass", (try rig.svc.get(rig.arena(), created.id)).passwordBytes());

    _ = try rig.svc.update(rig.arena(), created.id, .{ .password = "" }, &f);
    try testing.expect(!(try rig.svc.get(rig.arena(), created.id)).hasPassword());
}

test "update revalidates, rejects an empty patch and an unknown id" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    const created = try rig.svc.create(rig.arena(), mkSpec("main"), &f);

    try testing.expectError(error.Rejected, rig.svc.update(rig.arena(), created.id, .{}, &f));
    try testing.expectEqualStrings("empty_patch", f.code);
    try testing.expectEqualStrings("nothing to change: give at least one field", f.message);

    try testing.expectError(error.Rejected, rig.svc.update(rig.arena(), created.id, .{ .host = "http://x" }, &f));
    try testing.expectEqualStrings("invalid_host", f.code);

    try testing.expectError(error.Rejected, rig.svc.update(rig.arena(), created.id, .{ .max_connections = 0 }, &f));
    try testing.expectEqualStrings("invalid_max_connections", f.code);

    try testing.expectError(error.NotFound, rig.svc.update(rig.arena(), "nope", .{ .host = "x" }, &f));
    // Nothing of the failed patches was stored.
    try testing.expectEqualStrings("news.example.com", (try rig.svc.get(rig.arena(), created.id)).host);
}

test "update to a taken name is a conflict" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    _ = try rig.svc.create(rig.arena(), mkSpec("one"), &f);
    const two = try rig.svc.create(rig.arena(), mkSpec("two"), &f);

    try testing.expectError(error.Rejected, rig.svc.update(rig.arena(), two.id, .{ .name = "one" }, &f));
    try testing.expectEqualStrings("name_taken", f.code);
}

test "an unreadable password keeps the Server listed and is fixed by a new one" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    const created = try rig.svc.create(rig.arena(), mkSpec("main"), &f);
    var other: Key = .{ .bytes = @splat(9) };
    defer other.deinit();
    var restarted = rig.withOtherKey(&other);

    const listed = try restarted.list(rig.arena());
    try testing.expectEqual(@as(usize, 1), listed.len);
    try testing.expect(listed[0].password_unavailable);

    // A patch without a password keeps the (unreadable) stored one.
    const patched = try restarted.update(rig.arena(), created.id, .{ .name = "renamed" }, &f);
    try testing.expect(patched.password_unavailable);
    // The stored value is untouched: the original key still opens it.
    try testing.expectEqualStrings("hunter2", (try rig.svc.get(rig.arena(), created.id)).passwordBytes());

    const fixed = try restarted.update(rig.arena(), created.id, .{ .password = "fresh" }, &f);
    try testing.expect(!fixed.password_unavailable);
    try testing.expectEqualStrings("fresh", (try restarted.get(rig.arena(), created.id)).passwordBytes());
}

test "list is ordered by priority then name" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var f: Failure = .{};
    try testing.expectEqual(@as(usize, 0), (try rig.svc.list(rig.arena())).len);
    var fill = mkSpec("fill");
    fill.priority = 1;
    _ = try rig.svc.create(rig.arena(), fill, &f);
    _ = try rig.svc.create(rig.arena(), mkSpec("main"), &f);

    const got = try rig.svc.list(rig.arena());

    try testing.expectEqual(@as(usize, 2), got.len);
    try testing.expectEqualStrings("main", got[0].name);
    try testing.expectEqualStrings("fill", got[1].name);
}

test "an unknown server is not found" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectError(error.NotFound, rig.svc.get(rig.arena(), "nope"));
}

test "the patch is empty only when no field is given" {
    try testing.expect((Patch{}).isEmpty());
    try testing.expect(!(Patch{ .enabled = false }).isEmpty());
    try testing.expect(!(Patch{ .password = "" }).isEmpty());
}

test "the real clock is milliseconds since 1970" {
    try testing.expect(realClock(testing.io) > 1_700_000_000_000);
}
