//! The harness of the runner tests (internal/usenet/runner harness_test.go): a
//! Runner on a real database, a bus, fake NNTP servers (nntptest, plain
//! sockets), temp directories and a recorder of the bus events. Only tests
//! import this file.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const event = @import("event.zig");
const events_bus = @import("events_bus.zig");
const nntp = @import("nntp.zig");
const nntptest = @import("nntptest.zig");
const queue = @import("queue.zig");
const adapters = @import("queue_adapters.zig");
const runner = @import("runner.zig");
const seams = @import("runner_seams.zig");
const store_mod = @import("runner_store.zig");
const tu = @import("downloads_testutil.zig");

pub const epoch = tu.epoch;
pub const gpa = testing.allocator;
pub const io = testing.io;

/// Deterministic pseudo-random bytes.
pub fn payload(a: Allocator, n: usize, seed: u8) ![]u8 {
    const b = try a.alloc(u8, n);
    var x: u32 = @as(u32, seed) *% 2654435761 +% 1;
    for (b) |*c| {
        x = x *% 1664525 +% 1013904223;
        c.* = @truncate(x >> 24);
    }
    return b;
}

pub fn sleepMs(ms: i64) void {
    io.sleep(.fromMilliseconds(ms), .awake) catch {};
}

/// A clock that moves `step_ms` at every reading, to play time passing.
pub const FakeClock = struct {
    now: std.atomic.Value(i64) = .init(tu.epoch),
    step_ms: i64 = 0,

    pub fn clock(c: *FakeClock) queue.seams.Clock {
        return .{ .ctx = c, .now_fn = read };
    }

    fn read(ctx: ?*anyopaque, _: Io) i64 {
        const c: *FakeClock = @ptrCast(@alignCast(ctx.?));
        return c.now.fetchAdd(c.step_ms, .monotonic);
    }
};

/// What the recorder keeps of one event.
pub const Seen = struct {
    kind: event.Kind,
    /// The Download the event is about; empty for the others.
    id: []const u8 = "",
    state: ?dl.State = null,
    paused: bool = false,
    done: i64 = 0,
    total: i64 = 0,
    reason: ?dl.FailureReason = null,
};

/// Collects every event of the bus until `stop`.
pub const Recorder = struct {
    sub: *events_bus.Subscription,
    arena: std.heap.ArenaAllocator,
    mu: Io.Mutex = .init,
    items: std.ArrayList(Seen) = .empty,
    fut: ?Io.Future(void) = null,

    pub fn start(bus: *events_bus.Bus) !*Recorder {
        const r = try gpa.create(Recorder);
        errdefer gpa.destroy(r);
        r.* = .{ .sub = try bus.subscribe(), .arena = .init(gpa) };
        r.fut = try io.concurrent(run, .{r});
        return r;
    }

    pub fn stop(r: *Recorder) void {
        r.sub.close();
        if (r.fut) |*f| f.await(io);
        r.fut = null;
        r.sub.deinit();
        r.items.deinit(gpa);
        r.arena.deinit();
        gpa.destroy(r);
    }

    fn run(r: *Recorder) void {
        while (true) {
            const item = r.sub.next(null) catch return;
            defer item.release();
            r.record(item.event());
        }
    }

    fn record(r: *Recorder, e: event.Event) void {
        const a = r.arena.allocator();
        var s: Seen = .{ .kind = e.kind() };
        switch (e) {
            .download_added, .download_updated => |d| {
                s.id = a.dupe(u8, d.id) catch return;
                s.state = d.state;
                s.paused = d.paused;
                s.done = d.done_bytes;
                s.total = d.total_bytes;
            },
            .download_progress => |p| {
                s.id = a.dupe(u8, p.id) catch return;
                s.done = p.done_bytes;
                s.total = p.total_bytes;
            },
            .fetched, .removed => |id| s.id = a.dupe(u8, id) catch return,
            .failed => |f| {
                s.id = a.dupe(u8, f.id) catch return;
                s.reason = f.reason;
            },
            else => {},
        }
        r.mu.lockUncancelable(io);
        defer r.mu.unlock(io);
        r.items.append(gpa, s) catch {};
    }

    /// A copy of what was seen so far, in `a`.
    pub fn all(r: *Recorder, a: Allocator) ![]const Seen {
        r.mu.lockUncancelable(io);
        defer r.mu.unlock(io);
        return a.dupe(Seen, r.items.items);
    }

    /// The index of the first event of `id` matching, or null.
    pub fn firstIndex(r: *Recorder, kind: event.Kind, id: []const u8, state: ?dl.State) ?usize {
        r.mu.lockUncancelable(io);
        defer r.mu.unlock(io);
        for (r.items.items, 0..) |s, i| {
            if (s.kind != kind or !std.mem.eql(u8, s.id, id)) continue;
            if (state != null and s.state != state) continue;
            return i;
        }
        return null;
    }

    pub fn count(r: *Recorder, kind: event.Kind, id: []const u8) usize {
        r.mu.lockUncancelable(io);
        defer r.mu.unlock(io);
        var n: usize = 0;
        for (r.items.items) |s| {
            if (s.kind == kind and std.mem.eql(u8, s.id, id)) n += 1;
        }
        return n;
    }

    /// The states of the Download's added/updated events, in order, without
    /// repeats.
    pub fn states(r: *Recorder, a: Allocator, id: []const u8) ![]const dl.State {
        r.mu.lockUncancelable(io);
        defer r.mu.unlock(io);
        var out: std.ArrayList(dl.State) = .empty;
        for (r.items.items) |s| {
            if (s.kind != .download_added and s.kind != .download_updated) continue;
            if (!std.mem.eql(u8, s.id, id)) continue;
            const st = s.state.?;
            if (out.items.len > 0 and out.items[out.items.len - 1] == st) continue;
            try out.append(a, st);
        }
        return out.items;
    }
};

const Opts = struct {
    save_interval_ms: i64 = 10_000,
    failure_backoff_ms: i64 = 30_000,
    disk_check_interval_ms: i64 = 0,
    done_bytes_interval_ms: i64 = 0,
    progress_interval_ms: i64 = 0,
    poll_interval_ms: u32 = 50,
    volume_margin: i64 = 0,
    /// The runner keeps its state in `Harness.mem`/the database, so a second
    /// runner continues where the first stopped.
    persistent: bool = true,
    use_sqlite: bool = false,
    clock: queue.seams.Clock = .{},
    on_fetched: ?seams.FetchedHook = null,
};

/// A Server of the harness: a fake NNTP server and its spec.
const Fake = struct {
    host: []const u8,
    server: *nntptest.Server,
};

pub const Harness = struct {
    tmp: testing.TmpDir,
    root: []const u8,
    rig: tu.Rig,
    arena: std.heap.ArenaAllocator,
    bus: *events_bus.Bus,
    rec: *Recorder,
    mem: store_mod.Mem,
    sqlite_stores: store_mod.Sqlite,
    runner: ?*runner.Runner = null,
    opts: Opts = .{},

    mu: Io.Mutex = .init,
    // Guarded by `mu`.
    specs: std.ArrayList(seams.ServerSpec) = .empty,
    fakes: std.ArrayList(Fake) = .empty,
    paths: std.StringHashMapUnmanaged([]const u8) = .empty,
    limit: i64 = 1,
    free: u64 = 1 << 40,
    min_free: u64 = 5 << 30,
    seq: i64 = 0,
    fixtures: std.ArrayList(*nntptest.Fixture) = .empty,

    pub fn create() !*Harness {
        const h = try gpa.create(Harness);
        errdefer gpa.destroy(h);
        h.* = .{
            .tmp = testing.tmpDir(.{}),
            .root = undefined,
            .rig = undefined,
            .arena = .init(gpa),
            .bus = undefined,
            .rec = undefined,
            .mem = .init(gpa, io),
            .sqlite_stores = undefined,
        };
        errdefer h.tmp.cleanup();
        errdefer h.arena.deinit();
        var buf: [Io.Dir.max_path_bytes]u8 = undefined;
        const n = try h.tmp.dir.realPath(io, &buf);
        h.root = try h.arena.allocator().dupe(u8, buf[0..n]);
        try h.rig.open();
        errdefer h.rig.close();
        h.sqlite_stores = .{ .store = h.rig.store };
        h.bus = try events_bus.Bus.init(gpa, io, .{ .buffer = 100_000 });
        errdefer h.bus.deinit();
        h.rec = try Recorder.start(h.bus);
        return h;
    }

    pub fn destroy(h: *Harness) void {
        if (h.runner) |r| r.destroy();
        h.rec.stop();
        h.bus.deinit();
        for (h.fakes.items) |f| f.server.stop();
        h.fakes.deinit(gpa);
        h.specs.deinit(gpa);
        h.paths.deinit(gpa);
        for (h.fixtures.items) |fx| {
            fx.deinit();
            gpa.destroy(fx);
        }
        h.fixtures.deinit(gpa);
        h.mem.deinit();
        h.rig.close();
        h.arena.deinit();
        h.tmp.cleanup();
        gpa.destroy(h);
    }

    pub fn a(h: *Harness) Allocator {
        return h.arena.allocator();
    }

    pub fn incomplete(h: *Harness) []const u8 {
        return std.fmt.allocPrint(h.a(), "{s}/incomplete", .{h.root}) catch @panic("oom");
    }

    // ---- the fakes the runner is given ----

    fn serversList(ctx: *anyopaque, arena: Allocator) anyerror![]const seams.ServerSpec {
        const h: *Harness = @ptrCast(@alignCast(ctx));
        h.mu.lockUncancelable(io);
        defer h.mu.unlock(io);
        return arena.dupe(seams.ServerSpec, h.specs.items);
    }

    fn sourcePath(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?[]const u8 {
        const h: *Harness = @ptrCast(@alignCast(ctx));
        h.mu.lockUncancelable(io);
        defer h.mu.unlock(io);
        const p = h.paths.get(id) orelse return null;
        return try arena.dupe(u8, p);
    }

    fn readPolicy(ctx: *anyopaque, arena: Allocator) anyerror!queue.seams.Policy {
        const h: *Harness = @ptrCast(@alignCast(ctx));
        h.mu.lockUncancelable(io);
        defer h.mu.unlock(io);
        return .{ .limit = h.limit, .incomplete = try arena.dupe(u8, h.incomplete()), .min_free = h.min_free };
    }

    fn freeSpace(ctx: ?*anyopaque, _: []const u8) anyerror!u64 {
        const h: *Harness = @ptrCast(@alignCast(ctx.?));
        h.mu.lockUncancelable(io);
        defer h.mu.unlock(io);
        return h.free;
    }

    fn dial(ctx: *anyopaque, g: Allocator, i: Io, req: nntp.DialRequest) nntp.DialError!nntp.Transport {
        const h: *Harness = @ptrCast(@alignCast(ctx));
        h.mu.lockUncancelable(io);
        var found: ?*nntptest.Server = null;
        for (h.fakes.items) |f| {
            if (std.mem.eql(u8, f.host, req.host)) found = f.server;
        }
        h.mu.unlock(io);
        const srv = found orelse return error.DnsNotFound;
        return srv.dialer().dial(g, i, req);
    }

    // ---- setting up ----

    /// Adds a fake server and the Server spec for it, and returns it.
    pub fn addServer(h: *Harness, name: []const u8, priority: i64, conns: i64) !*nntptest.Server {
        const srv = try nntptest.Server.start(gpa, io);
        errdefer srv.stop();
        const host = try std.fmt.allocPrint(h.a(), "{s}.example", .{name});
        h.mu.lockUncancelable(io);
        defer h.mu.unlock(io);
        try h.fakes.append(gpa, .{ .host = host, .server = srv });
        try h.specs.append(gpa, .{
            .name = try h.a().dupe(u8, name),
            .host = host,
            .user = "u",
            .pass = "p",
            .priority = priority,
            .max_connections = conns,
        });
        return srv;
    }

    pub fn setServerEnabled(h: *Harness, index: usize, enabled: bool) void {
        h.mu.lockUncancelable(io);
        defer h.mu.unlock(io);
        h.specs.items[index].enabled = enabled;
    }

    pub fn setLimit(h: *Harness, limit: i64) void {
        h.mu.lockUncancelable(io);
        h.limit = limit;
        h.mu.unlock(io);
        h.bus.publish(.settings_changed);
    }

    pub fn setFree(h: *Harness, n: u64) void {
        h.mu.lockUncancelable(io);
        defer h.mu.unlock(io);
        h.free = n;
    }

    pub const AddOptions = struct {
        priority: dl.Priority = .normal,
        total_bytes: i64 = 0,
        paused: bool = false,
        /// Written as the staged NZB instead of the fixture's.
        nzb_bytes: ?[]const u8 = null,
        make_dir_readonly: bool = false,
    };

    /// Stores a Queued NZB Download whose NZB is the fixture's, stages the
    /// file, and announces it on the bus. Returns the ID.
    pub fn addDownload(h: *Harness, name: []const u8, fx: *const nntptest.Fixture, o: AddOptions) ![]const u8 {
        h.seq += 1;
        const id = try std.fmt.allocPrint(h.a(), "dl-{d}", .{h.seq});
        const dir = try std.fmt.allocPrint(h.a(), "{s}/{s}", .{ h.incomplete(), id });
        try Io.Dir.cwd().createDirPath(io, dir);
        const path = try std.fmt.allocPrint(h.a(), "{s}/{s}.nzb", .{ dir, name });
        try Io.Dir.cwd().writeFile(io, .{ .sub_path = path, .data = o.nzb_bytes orelse fx.nzb });
        {
            h.mu.lockUncancelable(io);
            defer h.mu.unlock(io);
            try h.paths.put(gpa, id, path);
        }
        const at = tu.epoch + h.seq * 1000;
        const d: dl.Download = .{
            .id = id,
            .name = name,
            .source_kind = .nzb,
            .state = .queued,
            .priority = o.priority,
            .queue_position = h.seq,
            .paused = o.paused,
            .total_bytes = o.total_bytes,
            .added_at = at,
            .updated_at = at,
        };
        try h.rig.repo.create(d);
        if (o.make_dir_readonly) try Io.Dir.cwd().setFilePermissions(io, dir, .fromMode(0o555), .{});
        h.bus.publish(.{ .download_added = d });
        return id;
    }

    /// Sets the paused flag as the API does, and announces it.
    pub fn setPaused(h: *Harness, id: []const u8, paused: bool) !void {
        var d = try h.get(id);
        d.paused = paused;
        try h.rig.repo.update(d);
        h.bus.publish(.{ .download_updated = try h.get(id) });
    }

    pub fn setPriority(h: *Harness, id: []const u8, p: dl.Priority) !void {
        var d = try h.get(id);
        d.priority = p;
        try h.rig.repo.update(d);
        h.bus.publish(.{ .download_updated = try h.get(id) });
    }

    /// Deletes the row and announces the removal.
    pub fn remove(h: *Harness, id: []const u8) !void {
        try h.rig.repo.delete(id);
        h.bus.publish(.{ .removed = id });
    }

    pub fn dirOf(h: *Harness, id: []const u8) []const u8 {
        return std.fmt.allocPrint(h.a(), "{s}/{s}", .{ h.incomplete(), id }) catch @panic("oom");
    }

    pub fn startRunner(h: *Harness) !*runner.Runner {
        std.debug.assert(h.runner == null);
        const r = try runner.Runner.create(gpa, io, try h.config());
        errdefer r.destroy();
        h.runner = r;
        try r.start();
        return r;
    }

    fn config(h: *Harness) !runner.Config {
        const o = h.opts;
        return .{
            .bus = h.bus,
            .downloads = adapters.downloadsFromRepo(&h.rig.repo),
            .policy = .{ .ctx = h, .read_fn = readPolicy },
            .servers = .{ .ctx = h, .list_fn = serversList },
            .sources = .{ .ctx = h, .path_fn = sourcePath },
            .dialer = .{ .ctx = h, .dial_fn = dial },
            .progress = if (!o.persistent) null else if (o.use_sqlite) h.sqlite_stores.progress() else h.mem.progress(),
            .volumes = if (!o.persistent) null else if (o.use_sqlite) h.sqlite_stores.volumes() else h.mem.volumes(),
            .reports = if (!o.persistent) null else if (o.use_sqlite) h.sqlite_stores.reports() else h.mem.reports(),
            .free_space = .{ .ctx = h, .free_fn = freeSpace },
            .clock = o.clock,
            .backoff = .none,
            .on_fetched = o.on_fetched,
            .volume_margin = o.volume_margin,
            .poll_interval_ms = o.poll_interval_ms,
            .save_interval_ms = o.save_interval_ms,
            .done_bytes_interval_ms = o.done_bytes_interval_ms,
            .disk_check_interval_ms = o.disk_check_interval_ms,
            .progress_interval_ms = o.progress_interval_ms,
            .failure_backoff_ms = o.failure_backoff_ms,
        };
    }

    /// Starts a runner after a restart: the Downloads found Fetching go back
    /// to Queued first.
    pub fn startRunnerRecovered(h: *Harness) !*runner.Runner {
        std.debug.assert(h.runner == null);
        const r = try runner.Runner.create(gpa, io, try h.config());
        errdefer r.destroy();
        h.runner = r;
        try r.recover();
        try r.start();
        return r;
    }

    /// Stops the runner and drops it, as a shutdown does.
    pub fn stopRunner(h: *Harness) void {
        if (h.runner) |r| r.destroy();
        h.runner = null;
    }

    // ---- looking ----

    pub fn get(h: *Harness, id: []const u8) !dl.Download {
        return h.rig.repo.get(h.a(), id);
    }

    pub fn stateOf(h: *Harness, id: []const u8) ?dl.State {
        const d = h.rig.repo.get(h.a(), id) catch return null;
        return d.state;
    }

    /// Waits until the Download is in `state`.
    pub fn waitState(h: *Harness, id: []const u8, state: dl.State) !dl.Download {
        var waited: i64 = 0;
        while (waited < 20_000) : (waited += 5) {
            const d = h.get(id) catch null;
            if (d) |x| {
                if (x.state == state) return x;
            }
            sleepMs(5);
        }
        std.debug.print("download {s} never reached {s}, it is {?s}\n", .{ id, @tagName(state), if (h.stateOf(id)) |s| @tagName(s) else null });
        return error.Timeout;
    }

    pub fn waitFor(comptime T: type, ctx: T, comptime cond: fn (T) bool, ms: i64) !void {
        var waited: i64 = 0;
        while (waited < ms) : (waited += 2) {
            if (cond(ctx)) return;
            sleepMs(2);
        }
        return error.Timeout;
    }

    /// Waits until the number of requests the server got stops changing for
    /// 150 ms and returns it.
    pub fn stableRequests(srv: *nntptest.Server) !usize {
        var last = srv.totalRequests();
        var quiet: i64 = 0;
        var waited: i64 = 0;
        while (waited < 10_000) : (waited += 10) {
            sleepMs(10);
            const v = srv.totalRequests();
            if (v != last) {
                last = v;
                quiet = 0;
            } else {
                quiet += 10;
                if (quiet > 150) return last;
            }
        }
        return error.Timeout;
    }

    /// Builds a Fixture of the named files; the harness frees it.
    pub fn fixture(h: *Harness, inputs: []const nntptest.FixtureInput, part_size: usize) !*nntptest.Fixture {
        const fx = try gpa.create(nntptest.Fixture);
        errdefer gpa.destroy(fx);
        fx.* = try nntptest.buildFixture(gpa, inputs, part_size, .{});
        errdefer fx.deinit();
        try h.fixtures.append(gpa, fx);
        return fx;
    }

    /// name -> content of the regular files directly in the Download's
    /// directory, without .nzb.
    pub fn readDir(h: *Harness, id: []const u8) !std.StringArrayHashMapUnmanaged([]const u8) {
        var out: std.StringArrayHashMapUnmanaged([]const u8) = .empty;
        var d = try Io.Dir.cwd().openDir(io, h.dirOf(id), .{ .iterate = true });
        defer d.close(io);
        var it = d.iterate();
        while (try it.next(io)) |e| {
            if (e.kind != .file or std.ascii.endsWithIgnoreCase(e.name, ".nzb")) continue;
            const name = try h.a().dupe(u8, e.name);
            const bytes = try d.readFileAlloc(io, e.name, h.a(), .limited(1 << 30));
            try out.put(h.a(), name, bytes);
        }
        return out;
    }

    /// The files of `files` are in the Download's directory, byte for byte,
    /// and nothing else is.
    pub fn expectFiles(h: *Harness, id: []const u8, files: []const nntptest.FixtureFile) !void {
        var dir = try h.readDir(id);
        try testing.expectEqual(files.len, dir.count());
        for (files) |f| {
            const got = dir.get(f.name) orelse {
                std.debug.print("missing file {s}\n", .{f.name});
                return error.TestUnexpectedResult;
            };
            if (!std.mem.eql(u8, f.data, got)) {
                std.debug.print("file {s} differs: {d} bytes, want {d}\n", .{ f.name, got.len, f.data.len });
                return error.TestUnexpectedResult;
            }
        }
    }
};
