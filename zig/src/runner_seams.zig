//! What the Usenet runner is given besides the Queue's seams (internal/usenet
//! runner Config): the Servers, the stored NZB, the persistent state of a
//! fetch (resume blob, fetch report, un-deferred volumes) and the hook where
//! post-processing starts. Each seam is a context pointer plus functions, like
//! `queue_seams`, so a test plugs in a fake.
//!
//! Lifetime rule for all of them: the object behind `ctx` outlives the
//! Runner. Strings passed to a function are borrowed for the call only;
//! strings and slices a function returns are allocated in the arena it is
//! given.
const std = @import("std");
const Allocator = std.mem.Allocator;
const log = @import("log.zig");
const servers_service = @import("servers_service.zig");

/// One configured Server as the runner needs it. The runner builds one
/// connection pool per enabled Server. Strings are borrowed.
pub const ServerSpec = struct {
    /// Identifies the Server and is unique.
    name: []const u8,
    host: []const u8,
    port: u16 = 563,
    user: []const u8 = "",
    pass: []const u8 = "",
    /// Lower values are tried first.
    priority: i64 = 0,
    max_connections: i64 = 8,
    /// 0 is unlimited retention.
    retention_days: i64 = 0,
    enabled: bool = true,

    pub fn eql(a: ServerSpec, b: ServerSpec) bool {
        return std.mem.eql(u8, a.name, b.name) and std.mem.eql(u8, a.host, b.host) and a.port == b.port and
            std.mem.eql(u8, a.user, b.user) and std.mem.eql(u8, a.pass, b.pass) and a.priority == b.priority and
            a.max_connections == b.max_connections and a.retention_days == b.retention_days and a.enabled == b.enabled;
    }
};

/// Lists the configured Servers, enabled or not. The runner reads it on every
/// pass of its control loop, so changes apply without a restart.
pub const ServerSource = struct {
    ctx: *anyopaque,
    list_fn: *const fn (ctx: *anyopaque, arena: Allocator) anyerror![]const ServerSpec,

    pub fn list(s: ServerSource, arena: Allocator) anyerror![]const ServerSpec {
        return s.list_fn(s.ctx, arena);
    }
};

/// The Servers of the Servers service, with their passwords. A Server whose
/// stored password cannot be opened is skipped with a warning (once per
/// Server), as the Go daemon does. `service` must outlive the adapter.
pub const ServiceServers = struct {
    gpa: Allocator,
    service: *servers_service.ServerService,
    log: ?log.Logger = null,
    warned: std.StringHashMapUnmanaged(void) = .empty,

    pub fn deinit(s: *ServiceServers) void {
        var it = s.warned.keyIterator();
        while (it.next()) |k| s.gpa.free(k.*);
        s.warned.deinit(s.gpa);
    }

    pub fn source(s: *ServiceServers) ServerSource {
        return .{ .ctx = s, .list_fn = list };
    }

    fn list(ctx: *anyopaque, arena: Allocator) anyerror![]const ServerSpec {
        const s: *ServiceServers = @ptrCast(@alignCast(ctx));
        const all = try s.service.list(arena);
        var out: std.ArrayList(ServerSpec) = .empty;
        for (all) |srv| {
            if (srv.password_unavailable) {
                s.warnOnce(srv.id, srv.name);
                continue;
            }
            try out.append(arena, .{
                .name = srv.name,
                .host = srv.host,
                .port = @intCast(std.math.clamp(srv.port, 1, 65535)),
                .user = srv.username,
                .pass = srv.passwordBytes(),
                .priority = srv.priority,
                .max_connections = srv.max_connections,
                .retention_days = srv.retention_days,
                .enabled = srv.enabled,
            });
        }
        return out.items;
    }

    fn warnOnce(s: *ServiceServers, id: []const u8, name: []const u8) void {
        if (s.warned.contains(id)) return;
        const key = s.gpa.dupe(u8, id) catch return;
        s.warned.putNoClobber(s.gpa, key, {}) catch return s.gpa.free(key);
        if (s.log) |l| l.warn("usenet server skipped: its stored password cannot be decrypted, enter it again", &.{log.Attr.string("server", name)});
    }
};

/// Finds the stored NZB of a Download: the path of the staged file, which
/// lives in the Download's working directory (`<incomplete>/<id>/<name>.nzb`).
/// The working directory is the parent of that path. Null when the Download
/// has no stored source.
pub const NzbSource = struct {
    ctx: *anyopaque,
    path_fn: *const fn (ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?[]const u8,

    pub fn path(s: NzbSource, arena: Allocator, id: []const u8) anyerror!?[]const u8 {
        return s.path_fn(s.ctx, arena, id);
    }
};

/// The resume state of a Download that is being fetched (table
/// usenet_progress): an opaque blob owned by `runner_blob`.
pub const ProgressStore = struct {
    ctx: *anyopaque,
    /// Null when there is none.
    load_fn: *const fn (ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?[]const u8,
    save_fn: *const fn (ctx: *anyopaque, id: []const u8, blob: []const u8) anyerror!void,
    /// Deleting an unknown ID is not an error.
    delete_fn: *const fn (ctx: *anyopaque, id: []const u8) anyerror!void,

    pub fn load(s: ProgressStore, arena: Allocator, id: []const u8) anyerror!?[]const u8 {
        return s.load_fn(s.ctx, arena, id);
    }

    pub fn save(s: ProgressStore, id: []const u8, blob: []const u8) anyerror!void {
        return s.save_fn(s.ctx, id, blob);
    }

    pub fn delete(s: ProgressStore, id: []const u8) anyerror!void {
        return s.delete_fn(s.ctx, id);
    }
};

/// What a finished fetch knows about the Articles of a Download
/// (core.FetchDetail).
pub const FetchDetail = struct {
    /// How many Articles no Server had.
    missing_articles: i64 = 0,
    /// The encoded size, from the NZB, of the missing Articles.
    missing_bytes: i64 = 0,
    /// How many Articles were written from a copy that failed its CRC.
    damaged_articles: i64 = 0,
    /// The decoded size of the damaged ranges.
    damaged_bytes: i64 = 0,
    /// Files none of whose Articles arrived, so the writer never created them.
    missing_files: i64 = 0,

    pub fn report(d: FetchDetail) FetchReport {
        return .{ .missing_articles = d.missing_articles, .damaged_articles = d.damaged_articles };
    }
};

/// The part of a FetchDetail post-processing looks at (core.FetchReport).
pub const FetchReport = struct {
    missing_articles: i64 = 0,
    damaged_articles: i64 = 0,

    pub fn damaged(r: FetchReport) bool {
        return r.missing_articles > 0 or r.damaged_articles > 0;
    }
};

/// Keeps the FetchDetail of Fetched Downloads (table usenet_reports), so
/// post-processing still finds it after a restart.
pub const ReportStore = struct {
    ctx: *anyopaque,
    save_fn: *const fn (ctx: *anyopaque, id: []const u8, detail: FetchDetail) anyerror!void,
    /// Null when there is none.
    load_fn: *const fn (ctx: *anyopaque, id: []const u8) anyerror!?FetchDetail,

    pub fn save(s: ReportStore, id: []const u8, detail: FetchDetail) anyerror!void {
        return s.save_fn(s.ctx, id, detail);
    }

    pub fn load(s: ReportStore, id: []const u8) anyerror!?FetchDetail {
        return s.load_fn(s.ctx, id);
    }
};

/// One un-deferred PAR2 recovery volume: its index in the stored NZB's files
/// and whether it has been fetched.
pub const VolumeRow = struct {
    file_index: u32,
    fetched: bool,
};

/// Which recovery volumes Repair asked for (table usenet_undeferred), so the
/// choice survives a restart.
pub const VolumeStore = struct {
    ctx: *anyopaque,
    list_fn: *const fn (ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror![]const VolumeRow,
    /// Records the volumes as un-deferred and not fetched yet; a volume that
    /// is recorded already keeps its fetched flag.
    want_fn: *const fn (ctx: *anyopaque, id: []const u8, file_indexes: []const u32) anyerror!void,
    mark_fetched_fn: *const fn (ctx: *anyopaque, id: []const u8, file_indexes: []const u32) anyerror!void,

    pub fn list(s: VolumeStore, arena: Allocator, id: []const u8) anyerror![]const VolumeRow {
        return s.list_fn(s.ctx, arena, id);
    }

    pub fn want(s: VolumeStore, id: []const u8, file_indexes: []const u32) anyerror!void {
        return s.want_fn(s.ctx, id, file_indexes);
    }

    pub fn markFetched(s: VolumeStore, id: []const u8, file_indexes: []const u32) anyerror!void {
        return s.mark_fetched_fn(s.ctx, id, file_indexes);
    }
};

/// The one place where post-processing starts (Go: the `Fetched` event the
/// post-processor listens to). The runner calls it after the Download became
/// Fetched and the `fetched` event went out, on the job's task; it must not
/// block. `supplement` is true when the fetch only got recovery volumes that
/// Repair asked for (the Download was Fetched before). Z3 has no
/// post-processing, so the default is no hook and the Download stays Fetched;
/// Z4 sets one.
pub const FetchedHook = struct {
    ctx: *anyopaque,
    call: *const fn (ctx: *anyopaque, id: []const u8, supplement: bool) void,
};

// ---- tests ----

const testing = std.testing;
const tu = @import("downloads_testutil.zig");
const keyfile = @import("keyfile.zig");
const servers_tests_notify = servers_service.Notifier.discard;

test "the Servers service becomes specs with passwords, skipping a Server whose password is unreadable" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    var key: keyfile.Key = .{ .bytes = @splat(7) };
    defer key.deinit();
    var svc = servers_service.ServerService.init(testing.io, rig.store, &key, servers_tests_notify, servers_service.realClock);
    var f: @import("app_error.zig").Failure = .{};
    _ = try svc.create(a, .{ .name = "main", .host = "news.example.com", .username = "joe", .password = "hunter2", .max_connections = 10, .priority = 2 }, &f);
    _ = try svc.create(a, .{ .name = "off", .host = "off.example.com", .enabled = false }, &f);
    var adapter: ServiceServers = .{ .gpa = testing.allocator, .service = &svc };
    defer adapter.deinit();

    const specs = try adapter.source().list(a);

    try testing.expectEqual(@as(usize, 2), specs.len);
    var main: ?ServerSpec = null;
    for (specs) |s| if (std.mem.eql(u8, s.name, "main")) {
        main = s;
    };
    try testing.expectEqualStrings("news.example.com", main.?.host);
    try testing.expectEqualStrings("joe", main.?.user);
    try testing.expectEqualStrings("hunter2", main.?.pass);
    try testing.expectEqual(@as(u16, 563), main.?.port);
    try testing.expectEqual(@as(i64, 10), main.?.max_connections);
    try testing.expectEqual(@as(i64, 2), main.?.priority);
    try testing.expect(main.?.enabled);

    // The key changed: the password cannot be opened and the Server is skipped.
    var other: keyfile.Key = .{ .bytes = @splat(9) };
    defer other.deinit();
    var restarted = servers_service.ServerService.init(testing.io, rig.store, &other, servers_tests_notify, servers_service.realClock);
    var skipping: ServiceServers = .{ .gpa = testing.allocator, .service = &restarted };
    defer skipping.deinit();
    const left = try skipping.source().list(a);
    for (left) |s| try testing.expect(!std.mem.eql(u8, s.name, "main"));
    _ = try skipping.source().list(a); // warned once, not twice
    try testing.expectEqual(@as(u32, 1), skipping.warned.count());
}

test "a spec equals itself and differs by any field" {
    const base: ServerSpec = .{ .name = "a", .host = "h" };
    try testing.expect(base.eql(base));
    var x = base;
    x.port = 1;
    try testing.expect(!base.eql(x));
    x = base;
    x.pass = "p";
    try testing.expect(!base.eql(x));
    x = base;
    x.enabled = false;
    try testing.expect(!base.eql(x));
}
