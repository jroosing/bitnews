//! Fakes for the ports of the post-processing pipeline and the harness of its
//! tests (internal/core/postprocess_fakes_test.go): a PostProcessor on a real
//! temp database, a bus with a recorder of its events, and fake Repairer,
//! Unpacker, Finisher, FetchReporter, DeferredVolumes, FS and DirFor. Each fake
//! has fields that script its behaviour; the default is the happy path. Only
//! tests import this file.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const event = @import("event.zig");
const events_bus = @import("events_bus.zig");
const id_mod = @import("id.zig");
const ports = @import("postprocess_ports.zig");
const pp_mod = @import("postprocess.zig");
const queue_seams = @import("queue_seams.zig");
const tu = @import("downloads_testutil.zig");

pub const io = testing.io;
pub const gpa = testing.allocator;

pub fn sleepMs(ms: i64) void {
    io.sleep(.fromMilliseconds(ms), .awake) catch {};
}

fn lock(m: *Io.Mutex) void {
    m.lockUncancelable(io);
}

/// A copy of a string a port was called with, replaced by the next call.
const Saved = struct {
    mu: Io.Mutex = .init,
    value: ?[]u8 = null,

    fn set(s: *Saved, v: []const u8) void {
        lock(&s.mu);
        defer s.mu.unlock(io);
        if (s.value) |old| gpa.free(old);
        s.value = gpa.dupe(u8, v) catch null;
    }

    pub fn get(s: *Saved) []const u8 {
        lock(&s.mu);
        defer s.mu.unlock(io);
        return s.value orelse "";
    }

    pub fn deinit(s: *Saved) void {
        if (s.value) |v| gpa.free(v);
    }
};

/// The order of port calls across all fakes.
pub const Log = struct {
    mu: Io.Mutex = .init,
    calls: std.ArrayList([]const u8) = .empty,

    pub fn add(l: *Log, name: []const u8) void {
        lock(&l.mu);
        defer l.mu.unlock(io);
        l.calls.append(gpa, name) catch {};
    }

    pub fn deinit(l: *Log) void {
        l.calls.deinit(gpa);
    }

    pub fn expect(l: *Log, want: []const []const u8) !void {
        lock(&l.mu);
        defer l.mu.unlock(io);
        try testing.expectEqual(want.len, l.calls.items.len);
        for (want, l.calls.items) |w, got| try testing.expectEqualStrings(w, got);
    }

    pub fn count(l: *Log, name: []const u8) usize {
        lock(&l.mu);
        defer l.mu.unlock(io);
        var n: usize = 0;
        for (l.calls.items) |c| {
            if (std.mem.eql(u8, c, name)) n += 1;
        }
        return n;
    }

    pub fn len(l: *Log) usize {
        lock(&l.mu);
        defer l.mu.unlock(io);
        return l.calls.items.len;
    }
};

/// A repairer body that blocks until released and records how many callers
/// were inside at once.
pub const Gate = struct {
    mu: Io.Mutex = .init,
    cond: Io.Condition = .init,
    cur: usize = 0,
    max_seen: usize = 0,
    entered: usize = 0,
    released: bool = false,

    /// Blocks until `release`; `error.Canceled` when the task is cancelled.
    pub fn enter(g: *Gate) error{Canceled}!void {
        lock(&g.mu);
        defer g.mu.unlock(io);
        g.cur += 1;
        g.entered += 1;
        g.max_seen = @max(g.max_seen, g.cur);
        defer g.cur -= 1;
        while (!g.released) try g.cond.wait(io, &g.mu);
    }

    pub fn release(g: *Gate) void {
        lock(&g.mu);
        defer g.mu.unlock(io);
        g.released = true;
        g.cond.broadcast(io);
    }

    pub fn enteredCount(g: *Gate) usize {
        lock(&g.mu);
        defer g.mu.unlock(io);
        return g.entered;
    }

    pub fn maxSeen(g: *Gate) usize {
        lock(&g.mu);
        defer g.mu.unlock(io);
        return g.max_seen;
    }

    /// Waits until `n` callers have entered in all.
    pub fn waitEntered(g: *Gate, n: usize) !void {
        var waited: i64 = 0;
        while (waited < 5000) : (waited += 2) {
            if (g.enteredCount() >= n) return;
            sleepMs(2);
        }
        return error.Timeout;
    }

    /// No more than `n` callers entered, after a pause long enough for the
    /// others to show.
    pub fn expectNoMoreThan(g: *Gate, n: usize) !void {
        sleepMs(100);
        try testing.expectEqual(n, g.enteredCount());
    }
};

/// Sleeps until cancelled, as a port stuck in a long tool run would.
fn stall() error{Canceled}!void {
    io.sleep(.fromSeconds(3600), .awake) catch return error.Canceled;
}

pub const Repair = struct {
    log: *Log,
    outcome: ports.RepairOutcome = .{ .has_par2 = true },
    err: ?ports.PortError = null,
    diag: ports.Diag = .{},
    gate: ?*Gate = null,
    got_dir: Saved = .{},

    pub fn port(r: *Repair) ports.Repairer {
        return .{ .ctx = r, .repair_fn = repair };
    }

    pub fn deinit(r: *Repair) void {
        r.got_dir.deinit();
    }

    fn repair(ctx: *anyopaque, _: Allocator, _: Io, dir: []const u8, diag: *ports.Diag) ports.PortError!ports.RepairOutcome {
        const r: *Repair = @ptrCast(@alignCast(ctx));
        r.log.add("repair");
        r.got_dir.set(dir);
        if (r.gate) |g| try g.enter();
        if (r.err) |e| {
            diag.* = r.diag;
            return e;
        }
        return r.outcome;
    }
};

pub const Unpack = struct {
    log: *Log,
    found: bool = false,
    detect_err: ?ports.PortError = null,
    unpack_err: ?ports.PortError = null,
    diag: ports.Diag = .{},
    /// The first Unpack call stalls until cancelled (the daemon stops in the
    /// middle of a stage); later calls succeed.
    stall_first: bool = false,
    /// Every Unpack call stalls until cancelled.
    stall_always: bool = false,
    /// A stalled call reports this error when it is cancelled, whatever it is
    /// that cancelled it (a tool that says "operation was canceled").
    after_cancel: ports.PortError = error.Canceled,
    mu: Io.Mutex = .init,
    unpack_calls: usize = 0,
    got_dir: Saved = .{},
    got_dst: Saved = .{},
    got_id: Saved = .{},

    pub fn port(u: *Unpack) ports.Unpacker {
        return .{ .ctx = u, .detect_fn = detect, .unpack_fn = unpack };
    }

    pub fn deinit(u: *Unpack) void {
        u.got_dir.deinit();
        u.got_dst.deinit();
        u.got_id.deinit();
    }

    fn detect(ctx: *anyopaque, _: Allocator, _: Io, _: []const u8, diag: *ports.Diag) ports.PortError!bool {
        const u: *Unpack = @ptrCast(@alignCast(ctx));
        u.log.add("detect");
        if (u.detect_err) |e| {
            diag.* = u.diag;
            return e;
        }
        return u.found;
    }

    fn unpack(ctx: *anyopaque, _: Allocator, _: Io, dir: []const u8, dst: []const u8, download_id: []const u8, diag: *ports.Diag) ports.PortError!ports.UnpackResult {
        const u: *Unpack = @ptrCast(@alignCast(ctx));
        u.log.add("unpack");
        u.got_dir.set(dir);
        u.got_dst.set(dst);
        u.got_id.set(download_id);
        lock(&u.mu);
        u.unpack_calls += 1;
        const first = u.unpack_calls == 1;
        u.mu.unlock(io);
        if (u.stall_always or (u.stall_first and first)) {
            stall() catch return u.after_cancel;
        }
        if (u.unpack_err) |e| {
            diag.* = u.diag;
            return e;
        }
        return .{ .files = 1 };
    }

    pub fn calls(u: *Unpack) usize {
        lock(&u.mu);
        defer u.mu.unlock(io);
        return u.unpack_calls;
    }
};

pub const Finisher = struct {
    log: *Log,
    /// Null means "/complete/<name>".
    out: ?[]const u8 = null,
    err: ?ports.PortError = null,
    diag: ports.Diag = .{},
    got_dir: Saved = .{},
    got_unpacked: Saved = .{},
    got_kind: std.atomic.Value(u8) = .init(0),

    pub fn port(f: *Finisher) ports.Finisher {
        return .{ .ctx = f, .finish_fn = finish };
    }

    pub fn deinit(f: *Finisher) void {
        f.got_dir.deinit();
        f.got_unpacked.deinit();
    }

    fn finish(ctx: *anyopaque, arena: Allocator, _: Io, d: dl.Download, dir: []const u8, unpacked: []const u8, diag: *ports.Diag) ports.PortError![]const u8 {
        const f: *Finisher = @ptrCast(@alignCast(ctx));
        f.log.add("finish");
        f.got_dir.set(dir);
        f.got_unpacked.set(unpacked);
        f.got_kind.store(@backingInt(d.source_kind), .release);
        if (f.err) |e| {
            diag.* = f.diag;
            return e;
        }
        if (f.out) |o| return o;
        return std.fmt.allocPrint(arena, "/complete/{s}", .{d.name}) catch return error.OutOfMemory;
    }

    pub fn gotKind(f: *Finisher) dl.SourceKind {
        return @fromBackingInt(@intCast(f.got_kind.load(.acquire)));
    }
};

pub const Reporter = struct {
    log: *Log,
    report_value: ports.FetchReport = .{},
    err: ?ports.PortError = null,

    pub fn port(r: *Reporter) ports.FetchReporter {
        return .{ .ctx = r, .report_fn = report };
    }

    fn report(ctx: *anyopaque, _: Allocator, _: Io, _: dl.Download) ports.PortError!ports.FetchReport {
        const r: *Reporter = @ptrCast(@alignCast(ctx));
        r.log.add("fetchreport");
        if (r.err) |e| return e;
        return r.report_value;
    }
};

pub const Deferred = struct {
    log: *Log,
    more: bool = false,
    err: ?ports.PortError = null,
    mu: Io.Mutex = .init,
    min_blocks: std.ArrayList(i64) = .empty,

    pub fn port(d: *Deferred) ports.DeferredVolumes {
        return .{ .ctx = d, .fetch_fn = fetch };
    }

    fn fetch(ctx: *anyopaque, _: Io, _: dl.Download, min_blocks: i64) ports.PortError!bool {
        const d: *Deferred = @ptrCast(@alignCast(ctx));
        d.log.add("deferred");
        lock(&d.mu);
        d.min_blocks.append(gpa, min_blocks) catch {};
        d.mu.unlock(io);
        if (d.err) |e| return e;
        return d.more;
    }

    pub fn deinit(d: *Deferred) void {
        d.min_blocks.deinit(gpa);
    }
};

pub const FsFake = struct {
    exists: bool = true,
    err: bool = false,
    seen: Saved = .{},

    pub fn port(f: *FsFake) pp_mod.FS {
        return .{ .ctx = f, .dir_exists_fn = dirExists };
    }

    fn dirExists(ctx: *anyopaque, _: Io, dir: []const u8) anyerror!bool {
        const f: *FsFake = @ptrCast(@alignCast(ctx));
        f.seen.set(dir);
        if (f.err) return error.InputOutput;
        return f.exists;
    }
};

/// "/work/<name>", or `fixed` when set.
pub const DirFake = struct {
    fixed: ?[]const u8 = null,

    pub fn port(f: *DirFake) pp_mod.DirFor {
        return .{ .ctx = f, .dir_fn = dirFor };
    }

    fn dirFor(ctx: *anyopaque, arena: Allocator, _: Io, d: dl.Download) anyerror![]const u8 {
        const f: *DirFake = @ptrCast(@alignCast(ctx));
        if (f.fixed) |p| return p;
        return std.fmt.allocPrint(arena, "/work/{s}", .{d.name});
    }
};

fn fixedClock(_: ?*anyopaque, _: Io) i64 {
    return 2000;
}

/// One event the bus delivered, as text: "download_updated <state>",
/// "stage <stage>", "ready <path>", "failed <reason>", else the kind.
pub const Rec = struct {
    text: []const u8,
    id: []const u8,
    kind: event.Kind,
};

pub const Opts = struct {
    concurrency: usize = 1,
    poll_ms: u32 = 3_600_000,
    deferred: bool = true,
    /// Use the fake FS (a missing directory when `fs.exists` is false).
    fs: bool = false,
    /// The clock reads 2000 ms; false uses the real one.
    fixed_clock: bool = true,
};

pub const Harness = struct {
    rig: tu.Rig,
    bus: *events_bus.Bus,
    sub: *events_bus.Subscription,
    log: Log = .{},
    repair: Repair,
    unpack: Unpack,
    finisher: Finisher,
    reporter: Reporter,
    deferred: Deferred,
    fs: FsFake = .{},
    dirs: DirFake = .{},
    pp: *pp_mod.PostProcessor,
    seen: std.ArrayList(Rec) = .empty,
    arena_state: std.heap.ArenaAllocator,
    added: i64 = 0,

    /// Builds the rig; `h` must stay where it is until `deinit`.
    pub fn init(h: *Harness, opts: Opts) !void {
        try h.rig.open();
        errdefer h.rig.close();
        h.log = .{};
        h.repair = .{ .log = &h.log };
        h.unpack = .{ .log = &h.log };
        h.finisher = .{ .log = &h.log };
        h.reporter = .{ .log = &h.log };
        h.deferred = .{ .log = &h.log };
        h.fs = .{};
        h.dirs = .{};
        h.seen = .empty;
        h.added = 0;
        h.arena_state = .init(gpa);
        errdefer h.arena_state.deinit();
        h.bus = try events_bus.Bus.init(gpa, io, .{});
        errdefer h.bus.deinit();
        h.sub = try h.bus.subscribe();
        errdefer h.sub.deinit();
        h.pp = try pp_mod.PostProcessor.create(gpa, io, .{
            .downloads = h.rig.repo,
            .bus = h.bus,
            .repairer = h.repair.port(),
            .unpacker = h.unpack.port(),
            .finisher = h.finisher.port(),
            .fetch_reporter = h.reporter.port(),
            .deferred = if (opts.deferred) h.deferred.port() else null,
            .fs = if (opts.fs) h.fs.port() else null,
            .dir_for = h.dirs.port(),
            .concurrency = opts.concurrency,
            .poll_interval_ms = opts.poll_ms,
            .clock = if (opts.fixed_clock) .{ .now_fn = fixedClock } else .{},
        });
    }

    pub fn deinit(h: *Harness) void {
        h.pp.destroy();
        h.sub.deinit();
        h.bus.deinit();
        h.seen.deinit(gpa);
        h.arena_state.deinit();
        h.rig.close();
        h.log.deinit();
        h.deferred.deinit();
        h.repair.deinit();
        h.unpack.deinit();
        h.finisher.deinit();
        h.fs.seen.deinit();
    }

    fn a(h: *Harness) Allocator {
        return h.arena_state.allocator();
    }

    /// Stores a Download in the given state and returns its ID. Downloads are
    /// "added" one millisecond apart, so they are processed in the order added.
    pub fn add(h: *Harness, name: []const u8, state: dl.State, kind: dl.SourceKind) ![]const u8 {
        h.added += 1;
        const raw = id_mod.new(io);
        const id = try h.a().dupe(u8, &raw);
        try h.rig.repo.create(.{
            .id = id,
            .name = name,
            .source_kind = kind,
            .state = state,
            .added_at = tu.epoch + h.added,
            .updated_at = tu.epoch + h.added,
        });
        return id;
    }

    pub fn get(h: *Harness, id: []const u8) !dl.Download {
        return h.rig.repo.get(h.rig.arena(), id);
    }

    pub fn stateOf(h: *Harness, id: []const u8) ?dl.State {
        const d = h.rig.repo.get(h.rig.arena(), id) catch return null;
        return d.state;
    }

    /// Waits until the Download has the state.
    pub fn eventually(h: *Harness, id: []const u8, want: dl.State) !void {
        var waited: i64 = 0;
        while (waited < 5000) : (waited += 5) {
            if (h.stateOf(id)) |s| if (s == want) return;
            sleepMs(5);
        }
        std.debug.print("download {s} never reached {s}, it is {?s}\n", .{ id, @tagName(want), if (h.stateOf(id)) |s| @tagName(s) else null });
        return error.Timeout;
    }

    /// The events published since the last call, in order.
    pub fn events(h: *Harness) ![]const Rec {
        const start = h.seen.items.len;
        while (true) {
            const item = h.sub.next(1) catch break;
            defer item.release();
            const ev = item.event();
            var text: []const u8 = @tagName(ev.kind());
            var id: []const u8 = "";
            switch (ev) {
                .download_updated => |d| {
                    text = try std.fmt.allocPrint(h.a(), "download_updated {s}", .{@tagName(d.state)});
                    id = d.id;
                },
                .postprocess_stage => |p| {
                    text = try std.fmt.allocPrint(h.a(), "stage {s}", .{@tagName(p.stage)});
                    id = p.id;
                },
                .ready => |r| {
                    text = try std.fmt.allocPrint(h.a(), "ready {s}", .{r.output_path});
                    id = r.id;
                },
                .failed => |f| {
                    text = try std.fmt.allocPrint(h.a(), "failed {s} {s}", .{ @tagName(f.reason), f.detail });
                    id = f.id;
                },
                else => {},
            }
            try h.seen.append(gpa, .{ .text = text, .id = try h.a().dupe(u8, id), .kind = ev.kind() });
        }
        return h.seen.items[start..];
    }

    /// Whether any event delivered so far is of the kind.
    pub fn sawKind(h: *Harness, k: event.Kind) !bool {
        _ = try h.events();
        for (h.seen.items) |r| if (r.kind == k) return true;
        return false;
    }
};
