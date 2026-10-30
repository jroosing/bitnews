//! Tests of the Queue (internal/queue/*_test.go): a fake Protocol, the real
//! Downloads repo on a temp database, a recording Publisher, a fake clock and
//! a fake free-space probe. Downloads are named by their ID.
const std = @import("std");
const testing = std.testing;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const event = @import("event.zig");
const tu = @import("downloads_testutil.zig");
const queue = @import("queue.zig");
const seams = @import("queue_seams.zig");
const adapters = @import("queue_adapters.zig");
const settings_service = @import("settings_service.zig");
const fsutil_space = @import("fsutil_space.zig");

const Queue = queue.Queue;

/// A set of IDs with owned copies.
const IdSet = struct {
    map: std.StringHashMapUnmanaged(void) = .empty,

    fn add(s: *IdSet, id: []const u8) !void {
        if (s.map.contains(id)) return;
        const key = try testing.allocator.dupe(u8, id);
        errdefer testing.allocator.free(key);
        try s.map.putNoClobber(testing.allocator, key, {});
    }

    fn remove(s: *IdSet, id: []const u8) void {
        if (s.map.fetchRemove(id)) |kv| testing.allocator.free(kv.key);
    }

    fn has(s: *const IdSet, id: []const u8) bool {
        return s.map.contains(id);
    }

    fn deinit(s: *IdSet) void {
        var it = s.map.keyIterator();
        while (it.next()) |k| testing.allocator.free(k.*);
        s.map.deinit(testing.allocator);
    }
};

/// Records what the Queue asks of it. Running fetches are the ones it was
/// told to start and not told to stop or abandon.
const FakeProtocol = struct {
    mu: std.Io.Mutex = .init,
    running_set: IdSet = .{},
    excluded: IdSet = .{},
    not_now: IdSet = .{},
    fail_start: IdSet = .{},
    can_start: bool = true,
    abandon_fails: bool = false,
    calls: std.ArrayList([]u8) = .empty,

    fn deinit(p: *FakeProtocol) void {
        p.running_set.deinit();
        p.excluded.deinit();
        p.not_now.deinit();
        p.fail_start.deinit();
        for (p.calls.items) |c| testing.allocator.free(c);
        p.calls.deinit(testing.allocator);
    }

    fn record(p: *FakeProtocol, comptime fmt: []const u8, args: anytype) void {
        const line = std.fmt.allocPrint(testing.allocator, fmt, args) catch @panic("oom");
        p.calls.append(testing.allocator, line) catch @panic("oom");
    }

    pub fn prepare(p: *FakeProtocol) bool {
        return p.can_start;
    }

    pub fn exclude(p: *FakeProtocol, id: []const u8) bool {
        p.mu.lockUncancelable(testing.io);
        defer p.mu.unlock(testing.io);
        return p.excluded.has(id);
    }

    pub fn running(p: *FakeProtocol, id: []const u8) bool {
        p.mu.lockUncancelable(testing.io);
        defer p.mu.unlock(testing.io);
        return p.running_set.has(id);
    }

    pub fn start(p: *FakeProtocol, d: dl.Download) anyerror!void {
        p.mu.lockUncancelable(testing.io);
        defer p.mu.unlock(testing.io);
        if (p.not_now.has(d.id)) return error.NotNow;
        if (p.fail_start.has(d.id)) return error.Boom;
        p.record("start {s} {s}", .{ d.name, @tagName(d.state) });
        try p.running_set.add(d.id);
    }

    pub fn stop(p: *FakeProtocol, id: []const u8) void {
        p.mu.lockUncancelable(testing.io);
        defer p.mu.unlock(testing.io);
        p.record("stop {s}", .{id});
        p.running_set.remove(id);
    }

    pub fn abandon(p: *FakeProtocol, id: []const u8) anyerror!void {
        p.mu.lockUncancelable(testing.io);
        defer p.mu.unlock(testing.io);
        p.record("abandon {s}", .{id});
        if (p.abandon_fails) return error.Stuck;
        p.running_set.remove(id);
    }
};

/// Keeps the Queue events and the progress events as text lines.
const Recorder = struct {
    mu: std.Io.Mutex = .init,
    queue_events: std.ArrayList([]u8) = .empty,
    progress_events: std.ArrayList([]u8) = .empty,
    updated: usize = 0,

    fn deinit(r: *Recorder) void {
        for (r.queue_events.items) |l| testing.allocator.free(l);
        r.queue_events.deinit(testing.allocator);
        for (r.progress_events.items) |l| testing.allocator.free(l);
        r.progress_events.deinit(testing.allocator);
    }

    fn publisher(r: *Recorder) event.Publisher {
        return .{ .ctx = r, .publish_fn = publish };
    }

    fn publish(ctx: *anyopaque, e: event.Event) void {
        const r: *Recorder = @ptrCast(@alignCast(ctx));
        r.mu.lockUncancelable(testing.io);
        defer r.mu.unlock(testing.io);
        switch (e) {
            .queue_paused => |p| r.push(&r.queue_events, "paused {s} {s}", .{ @tagName(p.source), p.reason }),
            .queue_resumed => |s| r.push(&r.queue_events, "resumed {s}", .{@tagName(s)}),
            .download_progress => |p| r.push(&r.progress_events, "{s} {d}/{d} {d}", .{ p.id, p.done_bytes, p.total_bytes, p.bytes_per_second }),
            .download_updated => r.updated += 1,
            else => {},
        }
    }

    fn push(_: *Recorder, list: *std.ArrayList([]u8), comptime fmt: []const u8, args: anytype) void {
        const line = std.fmt.allocPrint(testing.allocator, fmt, args) catch @panic("oom");
        list.append(testing.allocator, line) catch @panic("oom");
    }

    /// Compares and clears.
    fn expect(list: *std.ArrayList([]u8), want: []const []const u8) !void {
        defer {
            for (list.items) |l| testing.allocator.free(l);
            list.clearRetainingCapacity();
        }
        try testing.expectEqual(want.len, list.items.len);
        for (want, list.items) |w, g| try testing.expectEqualStrings(w, g);
    }
};

const root = "/data/usenet/incomplete";

const Opts = struct {
    state: dl.State = .queued,
    priority: dl.Priority = .normal,
    total: i64 = 1000,
    done: i64 = 0,
    kind: dl.SourceKind = .nzb,
    paused: bool = false,
};

const H = struct {
    rig: tu.Rig,
    proto: FakeProtocol = .{},
    rec: Recorder = .{},
    q: Queue,
    // What the fakes answer.
    limit: i64 = 2,
    min_free: u64 = 0,
    free: u64 = 1 << 50,
    free_err: ?anyerror = null,
    policy_err: ?anyerror = null,
    now_ms: i64 = tu.epoch,
    woken: std.atomic.Value(usize) = .init(0),
    seq: i64 = 0,

    /// `h` must stay where it is: the Queue keeps pointers into it.
    fn init(h: *H) !void {
        h.* = .{ .rig = undefined, .q = undefined };
        try h.rig.open();
        errdefer h.rig.close();
        h.q = Queue.init(testing.allocator, testing.io, .{
            .source = .nzb,
            .downloads = adapters.downloadsFromRepo(&h.rig.repo),
            .events = h.rec.publisher(),
            .policy = .{ .ctx = h, .read_fn = readPolicy },
            .protocol = seams.Protocol.init(&h.proto),
            .free_space = .{ .ctx = h, .free_fn = freeSpace },
            .clock = .{ .ctx = h, .now_fn = clock },
            .wake = .{ .ctx = h, .wake_fn = wake },
        });
    }

    fn close(h: *H) void {
        h.q.deinit();
        h.proto.deinit();
        h.rec.deinit();
        h.rig.close();
    }

    fn readPolicy(ctx: *anyopaque, arena: Allocator) anyerror!seams.Policy {
        const h: *H = @ptrCast(@alignCast(ctx));
        if (h.policy_err) |e| return e;
        return .{ .limit = h.limit, .incomplete = try arena.dupe(u8, root), .min_free = h.min_free };
    }

    fn freeSpace(ctx: ?*anyopaque, path: []const u8) anyerror!u64 {
        const h: *H = @ptrCast(@alignCast(ctx.?));
        try testing.expectEqualStrings(root, path);
        if (h.free_err) |e| return e;
        return h.free;
    }

    fn clock(ctx: ?*anyopaque, _: std.Io) i64 {
        const h: *H = @ptrCast(@alignCast(ctx.?));
        return h.now_ms;
    }

    fn wake(ctx: ?*anyopaque) void {
        const h: *H = @ptrCast(@alignCast(ctx.?));
        _ = h.woken.fetchAdd(1, .monotonic);
    }

    /// Stores a Download named `name` (which is also its ID), Queued with
    /// normal priority by default, added in call order.
    fn add(h: *H, name: []const u8, o: Opts) ![]const u8 {
        h.seq += 1;
        const at = tu.epoch + h.seq * 1000;
        try h.rig.repo.create(.{
            .id = name,
            .name = name,
            .source_kind = o.kind,
            .state = o.state,
            .priority = o.priority,
            .paused = o.paused,
            .total_bytes = o.total,
            .done_bytes = o.done,
            .added_at = at,
            .updated_at = at,
        });
        return name;
    }

    /// Adds a Download whose fetch the fake protocol already runs.
    fn addRunning(h: *H, name: []const u8, o: Opts) !void {
        _ = try h.add(name, o);
        try h.proto.running_set.add(name);
    }

    fn state(h: *H, id: []const u8) !dl.State {
        return (try h.rig.repo.get(h.rig.arena(), id)).state;
    }

    fn doneBytes(h: *H, id: []const u8) !i64 {
        return (try h.rig.repo.get(h.rig.arena(), id)).done_bytes;
    }

    /// The names of the running fetches, sorted, must be `want`.
    fn expectRunning(h: *H, want: []const []const u8) !void {
        var names: std.ArrayList([]const u8) = .empty;
        defer names.deinit(testing.allocator);
        var it = h.proto.running_set.map.keyIterator();
        while (it.next()) |k| try names.append(testing.allocator, k.*);
        std.mem.sort([]const u8, names.items, {}, struct {
            fn lt(_: void, a: []const u8, b: []const u8) bool {
                return std.mem.lessThan(u8, a, b);
            }
        }.lt);
        try testing.expectEqual(want.len, names.items.len);
        for (want, names.items) |w, g| try testing.expectEqualStrings(w, g);
    }

    fn expectCalls(h: *H, want: []const []const u8) !void {
        try testing.expectEqual(want.len, h.proto.calls.items.len);
        for (want, h.proto.calls.items) |w, g| try testing.expectEqualStrings(w, g);
    }

    fn expectQueueEvents(h: *H, want: []const []const u8) !void {
        try Recorder.expect(&h.rec.queue_events, want);
    }

    fn expectProgress(h: *H, want: []const []const u8) !void {
        try Recorder.expect(&h.rec.progress_events, want);
    }

    /// The Pause reason shown, or null.
    fn shown(h: *H, arena: Allocator) !?queue.PauseInfo {
        return h.q.paused(arena);
    }

    fn expectShown(h: *H, reason: []const u8, detail: ?[]const u8) !void {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const info = (try h.shown(arena.allocator())) orelse return error.TestUnexpectedResult;
        try testing.expectEqualStrings(reason, info.reason);
        if (detail) |d| try testing.expectEqualStrings(d, info.detail);
    }

    fn expectNotPaused(h: *H) !void {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        try testing.expect((try h.shown(arena.allocator())) == null);
        try testing.expect(!h.q.isPaused());
    }
};

// ---- Reconcile ----

test "reconcile starts the best ranked Downloads up to the limit" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("a", .{});
    _ = try h.add("b", .{ .priority = .high });
    _ = try h.add("c", .{});

    h.q.reconcile();

    try h.expectRunning(&.{ "a", "b" });
    try testing.expectEqual(dl.State.fetching, try h.state("a"));
    try testing.expectEqual(dl.State.fetching, try h.state("b"));
    try testing.expectEqual(dl.State.queued, try h.state("c"));
    try h.expectCalls(&.{ "start b fetching", "start a fetching" });
    try testing.expectEqual(2, h.rec.updated);
}

test "reconcile stops a Fetching Download that is no longer chosen" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    h.limit = 1;
    try h.addRunning("low", .{ .state = .fetching });
    _ = try h.add("high", .{ .priority = .high });

    h.q.reconcile();

    try h.expectRunning(&.{"high"});
    try testing.expectEqual(dl.State.queued, try h.state("low"));
}

test "reconcile restarts a Fetching Download without a running fetch" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("orphan", .{ .state = .fetching });

    h.q.reconcile();

    try h.expectRunning(&.{"orphan"});
    try testing.expectEqual(dl.State.fetching, try h.state("orphan"));
    try h.expectCalls(&.{"start orphan fetching"});
}

test "reconcile leaves a Download the protocol says is not for now" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("cooling", .{ .state = .fetching });
    try h.proto.not_now.add("cooling");

    h.q.reconcile();

    try h.expectRunning(&.{});
    try testing.expectEqual(dl.State.fetching, try h.state("cooling"));
}

test "a start that fails is logged and left for a later pass" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("broken", .{ .state = .fetching });
    _ = try h.add("fine", .{ .state = .fetching });
    try h.proto.fail_start.add("broken");

    h.q.reconcile();

    try h.expectRunning(&.{"fine"});
    try testing.expectEqual(dl.State.fetching, try h.state("broken"));
}

test "reconcile starts nothing when the protocol cannot start, but still stops" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    h.limit = 1;
    try h.addRunning("low", .{ .state = .fetching });
    _ = try h.add("high", .{ .priority = .high });
    h.proto.can_start = false;

    h.q.reconcile();

    try h.expectRunning(&.{});
    try testing.expectEqual(dl.State.queued, try h.state("low"));
}

test "reconcile ignores excluded Downloads and other kinds" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    h.limit = 1;
    _ = try h.add("seeding", .{ .state = .fetching });
    try h.proto.excluded.add("seeding");
    _ = try h.add("torrent", .{ .kind = .torrent });
    _ = try h.add("next", .{});

    h.q.reconcile();

    try h.expectRunning(&.{"next"});
    try testing.expectEqual(dl.State.fetching, try h.state("seeding"));
    try testing.expectEqual(dl.State.queued, try h.state("torrent"));
}

test "reconcile stops a Queued Download whose fetch runs but was not chosen" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    h.limit = 1;
    _ = try h.add("first", .{});
    try h.addRunning("late", .{});

    h.q.reconcile();

    try h.expectRunning(&.{"first"});
    try testing.expectEqual(dl.State.queued, try h.state("late"));
}

test "reconcile never starts a paused Download" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("paused", .{ .paused = true });

    h.q.reconcile();

    try h.expectRunning(&.{});
}

test "reconcile does nothing when the settings cannot be read" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("a", .{});
    h.policy_err = error.Boom;

    h.q.reconcile();

    try h.expectRunning(&.{});
    try testing.expectEqual(dl.State.queued, try h.state("a"));
}

test "reconcile follows a changed limit" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("a", .{});
    _ = try h.add("b", .{});
    h.limit = 1;
    h.q.reconcile();
    try h.expectRunning(&.{"a"});

    h.limit = 2;
    h.q.reconcile();
    try h.expectRunning(&.{ "a", "b" });

    h.limit = 0;
    h.q.reconcile();
    try h.expectRunning(&.{});
    try testing.expectEqual(dl.State.queued, try h.state("a"));
}

test "force Downloads run beyond the limit" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    h.limit = 0;
    _ = try h.add("forced", .{ .priority = .force });
    _ = try h.add("normal", .{});

    h.q.reconcile();

    try h.expectRunning(&.{"forced"});
}

test "recover moves the Queue's Fetching Downloads back to Queued" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("mine", .{ .state = .fetching });
    _ = try h.add("torrent", .{ .state = .fetching, .kind = .torrent });

    try h.q.recover();

    try testing.expectEqual(dl.State.queued, try h.state("mine"));
    try testing.expectEqual(dl.State.fetching, try h.state("torrent"));
}

// ---- Pause reasons ----

test "a paused Queue stops its fetches and starts nothing" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    try h.addRunning("running", .{ .state = .fetching });
    _ = try h.add("waiting", .{});

    try h.q.pause(queue.reason_user, "");
    h.q.reconcile();

    try h.expectRunning(&.{});
    try testing.expectEqual(dl.State.queued, try h.state("running"));
    try h.expectShown(queue.reason_user, null);
    try h.expectQueueEvents(&.{"paused nzb user"});
}

test "each Pause reason is held separately" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("a", .{});

    try h.q.pause(queue.reason_user, "");
    try h.q.pause(queue.reason_bind_unavailable, "wg0 is down");
    h.q.unpause(queue.reason_bind_unavailable);
    h.q.reconcile();

    try h.expectRunning(&.{});
    try h.expectShown(queue.reason_user, null);

    h.q.unpause(queue.reason_user);
    h.q.reconcile();

    try h.expectRunning(&.{"a"});
    try h.expectQueueEvents(&.{ "paused nzb user", "paused nzb bind_unavailable", "resumed nzb" });
}

test "paused shows the most specific reason" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    try h.expectNotPaused();

    try h.q.pause(queue.reason_user, "");
    try h.q.pause(queue.reason_bind_unavailable, "wg0 is down");
    try h.expectShown(queue.reason_bind_unavailable, "wg0 is down");

    try h.q.pause("aaa", "first alphabetically");
    try h.expectShown("aaa", "first alphabetically");

    try h.q.pause(queue.reason_disk_full, "full");
    try h.expectShown(queue.reason_disk_full, "full");
}

test "pause and unpause are idempotent and wake the runner" {
    var h: H = undefined;
    try h.init();
    defer h.close();

    try h.q.pause(queue.reason_user, "");
    try h.q.pause(queue.reason_user, "");
    h.q.unpause(queue.reason_user);
    h.q.unpause(queue.reason_user);

    try h.expectQueueEvents(&.{ "paused nzb user", "resumed nzb" });
    try testing.expectEqual(2, h.woken.load(.monotonic));
    try h.expectNotPaused();
}

test "pausing a held reason again changes its detail without a new event" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    try h.q.pause(queue.reason_bind_unavailable, "one");
    try h.q.pause(queue.reason_bind_unavailable, "two");
    try h.expectShown(queue.reason_bind_unavailable, "two");
    try h.expectQueueEvents(&.{"paused nzb bind_unavailable"});
}

// ---- holds ----

test "hold abandons the fetch and keeps the Download from starting until release" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    try h.addRunning("a", .{ .state = .fetching });

    try h.q.hold("a");
    h.q.reconcile();

    try h.expectRunning(&.{});
    try testing.expectEqualStrings("abandon a", h.proto.calls.items[0]);
    try testing.expect(h.q.held("a"));

    try testing.expect(h.q.release("a"));
    try testing.expect(!h.q.release("a"));
    h.q.reconcile();

    try h.expectRunning(&.{"a"});
    try testing.expect(!h.q.held("a"));
}

test "a hold expires after two minutes" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("a", .{});
    try h.q.hold("a");

    h.now_ms += 2 * 60 * 1000 - 1;
    try testing.expect(h.q.held("a"));
    h.now_ms += 2;
    h.q.reconcile();

    try h.expectRunning(&.{"a"});
}

test "hold of a Download without a fetch abandons nothing" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("a", .{});

    try h.q.hold("a");

    try h.expectCalls(&.{});
}

test "hold returns the protocol's error and the hold stays" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    try h.addRunning("a", .{ .state = .fetching });
    h.proto.abandon_fails = true;

    try testing.expectError(error.Stuck, h.q.hold("a"));
    try testing.expect(h.q.held("a"));
}

// ---- the disk guard ----

test "a Download that does not fit pauses the Queue for disk_full" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    h.min_free = 100;
    try h.addRunning("other", .{ .state = .fetching });
    _ = try h.add("big", .{ .priority = .high, .total = 5000 });
    h.free = 1000;

    h.q.reconcile();

    try h.expectRunning(&.{});
    try testing.expectEqual(dl.State.queued, try h.state("other"));
    try h.expectShown(queue.reason_disk_full, "1000 bytes free, 5100 needed");
    try h.expectQueueEvents(&.{"paused nzb disk_full"});
}

test "the disk guard releases itself once the next Download fits" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("big", .{ .total = 5000 });
    h.free = 1000;
    h.q.reconcile();
    try h.expectQueueEvents(&.{"paused nzb disk_full"});

    h.free = 10000;
    h.q.reconcile();

    try h.expectRunning(&.{"big"});
    try h.expectQueueEvents(&.{"resumed nzb"});
}

test "the disk guard does not announce a resume while the user still pauses" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    _ = try h.add("big", .{ .total = 5000 });
    h.free = 1000;
    h.q.reconcile();
    try h.q.pause(queue.reason_user, "");
    try h.expectQueueEvents(&.{ "paused nzb disk_full", "paused nzb user" });

    h.free = 10000;
    h.q.reconcile();

    try h.expectRunning(&.{});
    try h.expectQueueEvents(&.{});
    try h.expectShown(queue.reason_user, null);
}

test "the disk guard passes when free space cannot be read" {
    inline for (.{ error.Unsupported, error.Boom }) |err| {
        var h: H = undefined;
        try h.init();
        defer h.close();
        _ = try h.add("a", .{ .total = 5000 });
        h.free_err = err;

        h.q.reconcile();

        try h.expectRunning(&.{"a"});
    }
}

test "without a free-space probe the disk guard is off" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    h.q.cfg.free_space = null;
    _ = try h.add("a", .{ .total = 1 << 60 });

    h.q.reconcile();

    try h.expectRunning(&.{"a"});
}

test "progress rechecks the disk of a running fetch once per interval" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    const id = try h.add("a", .{ .state = .fetching });
    h.free = 1000;
    _ = h.q.progress(id, 0, 5000, 0); // the fetch starts; the Queue checked the disk then

    _ = h.q.progress(id, 100, 5000, 0);
    try h.expectNotPaused(); // not due yet

    h.now_ms += queue.default_disk_check_interval_ms;
    _ = h.q.progress(id, 200, 5000, 0);

    try h.expectShown(queue.reason_disk_full, "1000 bytes free, 4800 needed");
}

test "checkDisk pauses the Queue when a running fetch no longer fits" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    const id = try h.add("a", .{ .state = .fetching });
    h.free = 1000;

    try testing.expect(h.q.checkDisk(id, 5000)); // the first call only starts the interval
    h.now_ms += queue.default_disk_check_interval_ms;
    try testing.expect(h.q.checkDisk(id, 500));
    try testing.expect(h.q.checkDisk(id, 5000)); // not due again yet
    h.now_ms += queue.default_disk_check_interval_ms;
    try testing.expect(!h.q.checkDisk(id, 5000));

    try h.expectShown(queue.reason_disk_full, null);
}

test "diskFull pauses the Queue" {
    var h: H = undefined;
    try h.init();
    defer h.close();

    try h.q.diskFull("no space left on device");

    try h.expectShown(queue.reason_disk_full, "no space left on device");
}

test "a disk_full pause holds while the Download still would not fit" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    try h.addRunning("big", .{ .state = .fetching, .total = 1000 });
    h.free = 10;

    try h.q.diskFull("no space left on device"); // the fetch failed with ENOSPC
    h.q.reconcile();

    try h.expectShown(queue.reason_disk_full, null);
    try h.expectRunning(&.{});
}

// ---- progress ----

test "progress is published throttled, and the last one always" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    const id = try h.add("a", .{ .state = .fetching });

    try testing.expect(h.q.progress(id, 100, 1000, 50));
    try testing.expect(!h.q.progress(id, 200, 1000, 50)); // too soon
    try testing.expect(h.q.progress(id, 1000, 1000, 50)); // the final value always goes out
    h.now_ms += 1000;
    try testing.expect(h.q.progress(id, 500, 1000, 50));

    try h.expectProgress(&.{ "a 100/1000 50", "a 1000/1000 50", "a 500/1000 50" });
}

test "progress stores done_bytes at most once per interval" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    const id = try h.add("a", .{ .state = .fetching });

    _ = h.q.progress(id, 100, 1000, 0);
    _ = h.q.progress(id, 300, 1000, 0);
    try testing.expectEqual(100, try h.doneBytes(id)); // the second write waits for the interval

    h.now_ms += 3000;
    _ = h.q.progress(id, 400, 1000, 0);
    try testing.expectEqual(400, try h.doneBytes(id));
}

test "progress for a Download that is gone is not an error" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    try testing.expect(h.q.progress("ghost", 1, 10, 0));
}

test "forgetProgress starts the Download afresh" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    const id = try h.add("a", .{ .state = .fetching });
    _ = h.q.progress(id, 100, 1000, 0);

    h.q.forgetProgress(id);

    try testing.expect(h.q.progress(id, 150, 1000, 0)); // a new run publishes at once
    try testing.expectEqual(150, try h.doneBytes(id)); // and stores at once
}

// ---- threads ----

test "pause, progress and hold from many threads keep the maps sound" {
    var h: H = undefined;
    try h.init();
    defer h.close();
    const id = try h.add("a", .{ .state = .fetching });
    const Worker = struct {
        fn run(hh: *H, n: usize, name: []const u8) void {
            for (0..200) |i| {
                _ = hh.q.progress(name, @intCast(i), 1000, 1);
                if (i % 7 == 0) hh.q.pause("user", "x") catch {};
                if (i % 11 == 0) hh.q.unpause("user");
                _ = hh.q.held(name);
                if (n % 2 == 0 and i % 13 == 0) hh.q.forgetProgress(name);
            }
        }
    };
    var threads: [4]std.Thread = undefined;
    for (&threads, 0..) |*t, i| t.* = try std.Thread.spawn(.{}, Worker.run, .{ &h, i, id });
    for (threads) |t| t.join();
    h.q.unpause("user");
    try h.expectNotPaused();
}

// ---- the real seams ----

test "the Settings policy reads the limit, the incomplete root and min_free of its kind" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    var svc = try settings_service.Service.init(testing.allocator, testing.io, .{ .store = rig.store }, event.Publisher.discard, null);
    defer svc.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();

    var usenet: adapters.SettingsPolicy = .{ .service = &svc, .kind = .nzb };
    const u = try usenet.source().read(arena.allocator());
    try testing.expectEqual(1, u.limit);
    try testing.expectEqualStrings("/data/usenet/incomplete", u.incomplete);
    try testing.expectEqual(5 << 30, u.min_free);

    var torrent: adapters.SettingsPolicy = .{ .service = &svc, .kind = .torrent };
    const t = try torrent.source().read(arena.allocator());
    try testing.expectEqual(5, t.limit);
    try testing.expectEqualStrings("/data/torrents", t.incomplete);
}

test "the system free-space probe reads the real filesystem" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &buf);
    const free = seams.FreeSpace.system.free(buf[0..n]) catch |err| switch (err) {
        error.Unsupported => return error.SkipZigTest,
        else => return err,
    };
    try testing.expect(free > 0);
    try testing.expectError(error.FileNotFound, seams.FreeSpace.system.free("/nonexistent/bitnews/queue"));
    _ = fsutil_space;
}
