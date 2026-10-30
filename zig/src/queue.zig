//! The Queue of one Source kind (internal/queue, ADR-0008): which of its
//! Downloads should be Fetching, and why it is not starting any.
//!
//! A Queue owns the policy both protocols share: admission through
//! `queue_select.select` with the Queue's concurrency limit, Pause reasons, the
//! disk guard, holds while a Download's files are removed, picking up
//! Downloads left Fetching without a running fetch, recovery after a restart,
//! and the "change the state, then publish it" step. A protocol plugs in
//! through `seams.Protocol` and keeps only what is its own: Articles and
//! Servers for Usenet, the engine, metadata and Seeding for torrents. The
//! Queue decides WHAT runs; it never looks inside PAR2 or a torrent.
//!
//! The runner owns the control loop and calls `reconcile` on each pass; the
//! Queue calls `Config.wake` when something changed that needs another pass.
//!
//! Threads: every method may be called from any thread, except `reconcile` and
//! `recover`, which the runner calls from one thread at a time. One mutex
//! guards the small maps below and is never held across a seam call, a
//! database call or a publish. Memory: a pass works in an arena of its own;
//! the maps copy their keys (gpa) and `deinit` frees everything. The Queue
//! must not move after `init` returns it to its final place (the runner keeps
//! a pointer to it).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const event = @import("event.zig");
const log = @import("log.zig");
const order = @import("downloads_order.zig");
const rate_mod = @import("rate.zig");
const select_mod = @import("queue_select.zig");
pub const seams = @import("queue_seams.zig");

pub const select = select_mod.select;

/// The user's pause of the whole Queue. A caller may use any other reason too;
/// each one is held separately.
pub const reason_user = "user";
/// The disk guard's: the incomplete root is short of space.
pub const reason_disk_full = "disk_full";
/// The network monitor's: the protocol's bound interface or address is gone.
pub const reason_bind_unavailable = "bind_unavailable";

/// How often a running Download's done_bytes is stored when
/// `Config.done_bytes_interval_ms` is zero.
pub const default_done_bytes_interval_ms: i64 = 2000;
/// How often a running Download rechecks the free space when
/// `Config.disk_check_interval_ms` is zero.
pub const default_disk_check_interval_ms: i64 = 30_000;
/// How long a hold lasts unless `release` ends it first. It keeps a Download
/// whose removal failed half-way from staying parked for ever.
pub const hold_ttl_ms: i64 = 2 * 60 * 1000;

pub const Config = struct {
    /// The kind of the Queue's Downloads.
    source: dl.SourceKind,
    downloads: seams.Downloads,
    events: event.Publisher,
    policy: seams.PolicySource,
    protocol: seams.Protocol,
    /// Null turns the disk guard off.
    free_space: ?seams.FreeSpace = null,
    clock: seams.Clock = .{},
    wake: seams.Wake = .{},
    /// Null logs nothing.
    log: ?log.Logger = null,
    /// Throttles the progress events of one Download; zero is
    /// `rate.default_progress_interval_ms`.
    progress_interval_ms: i64 = 0,
    /// Zero is `default_done_bytes_interval_ms`.
    done_bytes_interval_ms: i64 = 0,
    /// Zero is `default_disk_check_interval_ms`.
    disk_check_interval_ms: i64 = 0,
};

/// The Pause reason to show and its detail; the strings are in the caller's
/// arena.
pub const PauseInfo = struct {
    reason: []const u8,
    detail: []const u8,
};

pub const Queue = struct {
    gpa: Allocator,
    io: Io,
    cfg: Config,
    throttle: rate_mod.ProgressThrottle,

    /// Guards the four maps. Keys (and the details) are owned by `gpa`.
    mu: Io.Mutex = .init,
    /// Pause reason -> detail.
    reasons: std.StringHashMapUnmanaged([]u8) = .empty,
    /// Download ID -> when its hold ends.
    holds: std.StringHashMapUnmanaged(i64) = .empty,
    /// Download ID -> when `progress` last stored done_bytes.
    stored: std.StringHashMapUnmanaged(i64) = .empty,
    /// Download ID -> when `checkDisk` last checked a running Download.
    disk_checked: std.StringHashMapUnmanaged(i64) = .empty,

    pub fn init(gpa: Allocator, io: Io, config: Config) Queue {
        var cfg = config;
        if (cfg.progress_interval_ms <= 0) cfg.progress_interval_ms = rate_mod.default_progress_interval_ms;
        if (cfg.done_bytes_interval_ms <= 0) cfg.done_bytes_interval_ms = default_done_bytes_interval_ms;
        if (cfg.disk_check_interval_ms <= 0) cfg.disk_check_interval_ms = default_disk_check_interval_ms;
        return .{
            .gpa = gpa,
            .io = io,
            .cfg = cfg,
            .throttle = .init(gpa, io, cfg.progress_interval_ms),
        };
    }

    pub fn deinit(self: *Queue) void {
        var it = self.reasons.iterator();
        while (it.next()) |e| {
            self.gpa.free(e.key_ptr.*);
            self.gpa.free(e.value_ptr.*);
        }
        self.reasons.deinit(self.gpa);
        freeKeys(self.gpa, &self.holds);
        freeKeys(self.gpa, &self.stored);
        freeKeys(self.gpa, &self.disk_checked);
        self.throttle.deinit();
        self.* = undefined;
    }

    /// The kind of the Queue's Downloads.
    pub fn source(self: *const Queue) dl.SourceKind {
        return self.cfg.source;
    }

    // ---- the pass ----

    /// Makes the running fetches match the Queue's policy: one pass of the
    /// runner's control loop. Failures are logged and the next pass tries
    /// again. One caller at a time.
    pub fn reconcile(self: *Queue) void {
        var arena: std.heap.ArenaAllocator = .init(self.gpa);
        defer arena.deinit();
        self.pass(arena.allocator()) catch |err| self.logErr("queue: pass stopped", err);
    }

    fn pass(self: *Queue, a: Allocator) Allocator.Error!void {
        const policy = self.cfg.policy.read(a) catch |err| return self.logErr("queue: cannot read settings", err);
        var can_start = self.cfg.protocol.prepare();
        const listed = self.list(a) catch |err| return self.logErr("queue: cannot list downloads", err);

        var kept: std.ArrayList(dl.Download) = .empty;
        for (listed) |d| {
            if (self.held(d.id) or self.cfg.protocol.exclude(d.id)) continue;
            try kept.append(a, d);
        }
        const ds = kept.items;

        const sel = try select_mod.select(a, ds, policy.limit);
        var activate = sel.activate;
        var deactivate = sel.deactivate;
        if (self.diskHeld() and try self.nextFits(a, policy, ds, activate)) self.unpause(reason_disk_full);
        if (self.isPaused()) {
            activate = &.{};
            var all: std.ArrayList([]const u8) = .empty;
            for (ds) |d| if (d.state == .fetching) try all.append(a, d.id);
            deactivate = all.items;
            can_start = false;
        }
        for (deactivate) |id| self.requeue(a, id);
        // A Queued Download whose fetch is running (a torrent whose metadata
        // just arrived) but was not chosen is stopped.
        for (ds) |d| {
            if (d.state == .queued and !contains(activate, d.id) and self.cfg.protocol.running(d.id)) {
                self.cfg.protocol.stop(d.id);
            }
        }
        if (!can_start) return;

        // Fetching Downloads without a running fetch (left by a crash, or by a
        // fetch that ended without a state change) are started again.
        var chosen: std.ArrayList(dl.Download) = .empty;
        for (activate) |id| try chosen.append(a, byId(ds, id).?);
        for (ds) |d| {
            if (d.state == .fetching and !self.cfg.protocol.running(d.id) and !contains(deactivate, d.id)) {
                try chosen.append(a, d);
            }
        }
        for (chosen.items) |d| {
            switch (try self.fits(a, policy, d.total_bytes - d.done_bytes)) {
                .ok => {},
                .short => |detail| {
                    try self.pause(reason_disk_full, detail);
                    for (ds) |other| {
                        if (other.state == .fetching or self.cfg.protocol.running(other.id)) self.requeue(a, other.id);
                    }
                    return;
                },
            }
            self.start(a, d);
        }
    }

    /// Moves `d` to Fetching when it is Queued and asks the protocol to start
    /// it.
    fn start(self: *Queue, a: Allocator, d: dl.Download) void {
        var cur = d;
        if (d.state == .queued) {
            cur = self.setState(a, d.id, .{ .to = .fetching, .at = self.nowMs() }) catch |err|
                return self.logErrId("queue: cannot activate download", d.id, err);
        } else {
            // The list may be stale: a fetch that finished meanwhile left the
            // Download Fetched, and starting it again would wipe its files.
            cur = self.cfg.downloads.get(a, d.id) catch return;
            if (cur.state != .fetching or self.cfg.protocol.running(d.id)) return;
        }
        self.cfg.protocol.start(cur) catch |err| {
            if (err != error.NotNow) self.logErrId("queue: cannot start download", d.id, err);
        };
    }

    /// Stops the fetch of `id` and moves the Download back to Queued.
    fn requeue(self: *Queue, a: Allocator, id: []const u8) void {
        self.cfg.protocol.stop(id);
        _ = self.setState(a, id, .{ .to = .queued, .at = self.nowMs() }) catch |err| {
            if (err != error.NotFound and err != error.IllegalTransition) {
                self.logErrId("queue: cannot deactivate download", id, err);
            }
        };
    }

    /// The Queue's Queued and Fetching Downloads.
    fn list(self: *Queue, a: Allocator) anyerror![]dl.Download {
        var out: std.ArrayList(dl.Download) = .empty;
        for ([_]dl.State{ .queued, .fetching }) |st| {
            try out.appendSlice(a, try self.cfg.downloads.list(a, st, self.cfg.source));
        }
        return out.items;
    }

    /// Applies `c` to the stored Download and publishes `download_updated`.
    /// The result is allocated in `arena`.
    pub fn setState(self: *Queue, arena: Allocator, id: []const u8, c: dl.StateChange) anyerror!dl.Download {
        const d = try self.cfg.downloads.updateState(arena, id, c);
        self.cfg.events.publish(.{ .download_updated = d });
        return d;
    }

    /// Moves the Queue's Fetching Downloads back to Queued. Call it once at
    /// start-up, before the first pass: those Downloads were interrupted by a
    /// shutdown or a crash. Tries them all and returns the first error.
    pub fn recover(self: *Queue) anyerror!void {
        var arena: std.heap.ArenaAllocator = .init(self.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const fetching = try self.cfg.downloads.list(a, .fetching, self.cfg.source);
        var first: ?anyerror = null;
        for (fetching) |d| {
            _ = self.setState(a, d.id, .{ .to = .queued, .at = self.nowMs() }) catch |err| {
                self.logErrId("queue: cannot recover download", d.id, err);
                if (first == null) first = err;
            };
        }
        if (first) |err| return err;
    }

    // ---- Pause reasons ----

    /// Adds a Pause reason (the strings are copied). The Queue starts nothing
    /// while any reason is held, and its next pass stops the running fetches.
    /// `queue_paused` is published when the reason is new; setting the detail
    /// of a reason that is held changes it quietly.
    pub fn pause(self: *Queue, reason: []const u8, detail: []const u8) Allocator.Error!void {
        const had = try self.setReason(reason, detail);
        if (had) return;
        self.logInfo("queue paused", &.{ log.Attr.string("reason", reason), log.Attr.string("detail", detail) });
        self.cfg.events.publish(.{ .queue_paused = .{ .source = self.cfg.source, .reason = reason, .detail = detail } });
        self.cfg.wake.wake();
    }

    fn setReason(self: *Queue, reason: []const u8, detail: []const u8) Allocator.Error!bool {
        const new_detail = try self.gpa.dupe(u8, detail);
        errdefer self.gpa.free(new_detail);
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        if (self.reasons.getPtr(reason)) |old| {
            self.gpa.free(old.*);
            old.* = new_detail;
            return true;
        }
        const key = try self.gpa.dupe(u8, reason);
        errdefer self.gpa.free(key);
        try self.reasons.putNoClobber(self.gpa, key, new_detail);
        return false;
    }

    /// Releases a Pause reason. `queue_resumed` is published when it was the
    /// last one.
    pub fn unpause(self: *Queue, reason: []const u8) void {
        self.mu.lockUncancelable(self.io);
        const removed = self.reasons.fetchRemove(reason);
        const last = self.reasons.count() == 0;
        self.mu.unlock(self.io);
        const kv = removed orelse return;
        self.gpa.free(kv.key);
        self.gpa.free(kv.value);
        if (last) {
            self.logInfo("queue resumed", &.{});
            self.cfg.events.publish(.{ .queue_resumed = self.cfg.source });
        }
        self.cfg.wake.wake();
    }

    /// Whether any Pause reason is held.
    pub fn isPaused(self: *Queue) bool {
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        return self.reasons.count() > 0;
    }

    /// The Pause reason to show, or null when the Queue runs: the disk guard
    /// first, then any reason other than the user's (alphabetically), then the
    /// user's. The result is allocated in `arena`.
    pub fn paused(self: *Queue, arena: Allocator) Allocator.Error!?PauseInfo {
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        if (self.reasons.count() == 0) return null;
        var shown: ?[]const u8 = null;
        if (self.reasons.contains(reason_disk_full)) {
            shown = reason_disk_full;
        } else {
            var it = self.reasons.keyIterator();
            while (it.next()) |k| {
                if (std.mem.eql(u8, k.*, reason_user)) continue;
                if (shown == null or std.mem.lessThan(u8, k.*, shown.?)) shown = k.*;
            }
        }
        const reason = shown orelse reason_user;
        return .{
            .reason = try arena.dupe(u8, reason),
            .detail = try arena.dupe(u8, self.reasons.get(reason).?),
        };
    }

    /// Pauses the Queue with `reason_disk_full`, for a fetch that failed with
    /// "no space left".
    pub fn diskFull(self: *Queue, detail: []const u8) Allocator.Error!void {
        return self.pause(reason_disk_full, detail);
    }

    fn diskHeld(self: *Queue) bool {
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        return self.reasons.contains(reason_disk_full);
    }

    // ---- the disk guard ----

    const Fit = union(enum) {
        ok,
        /// "N bytes free, M needed", in the pass arena.
        short: []const u8,
    };

    /// Whether the Download that would be started next fits on disk again: the
    /// first one `select` chose, else the first unpaused one in queue order (a
    /// fetch that failed with "no space left" is still Fetching when the guard
    /// is checked).
    fn nextFits(self: *Queue, a: Allocator, policy: seams.Policy, ds: []const dl.Download, activate: []const []const u8) Allocator.Error!bool {
        var next: ?dl.Download = null;
        if (activate.len > 0) {
            next = byId(ds, activate[0]);
        } else {
            for (ds) |d| {
                if (d.paused) continue;
                if (next == null or order.compareQueueOrder(d, next.?) == .lt) next = d;
            }
        }
        const remaining = if (next) |n| n.total_bytes - n.done_bytes else 0;
        return (try self.fits(a, policy, remaining)) == .ok;
    }

    /// Applies the disk guard: the incomplete root must have at least
    /// `remaining` plus min_free bytes free. It passes when the guard is off or
    /// the free space cannot be read.
    fn fits(self: *Queue, a: Allocator, policy: seams.Policy, remaining: i64) Allocator.Error!Fit {
        const fs = self.cfg.free_space orelse return .ok;
        const free = fs.free(policy.incomplete) catch |err| {
            if (err != error.Unsupported) {
                self.logWarn("queue: disk guard cannot read free space", &.{
                    log.Attr.string("path", policy.incomplete),
                    log.Attr.string("err", @errorName(err)),
                });
            }
            return .ok;
        };
        const need = @as(u64, @intCast(@max(remaining, 0))) +| policy.min_free;
        if (free >= need) return .ok;
        return .{ .short = try std.fmt.allocPrint(a, "{d} bytes free, {d} needed", .{ free, need }) };
    }

    /// Applies the disk guard to a running fetch of the Download that still
    /// needs `remaining` bytes, at most once per disk check interval (the first
    /// call only starts the interval: the fetch was checked when it started).
    /// When the incomplete root no longer has `remaining` plus min_free bytes
    /// free, it pauses the Queue with `reason_disk_full`, which stops the
    /// fetches on the next pass, and returns false. `progress` calls it; call
    /// it directly only for a fetch that reports no progress.
    pub fn checkDisk(self: *Queue, id: []const u8, remaining: i64) bool {
        const now = self.nowMs();
        self.mu.lockUncancelable(self.io);
        const due = self.dueLocked(&self.disk_checked, id, now, self.cfg.disk_check_interval_ms, false);
        self.mu.unlock(self.io);
        if (!due) return true;

        var arena: std.heap.ArenaAllocator = .init(self.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const policy = self.cfg.policy.read(a) catch return true;
        const fit = self.fits(a, policy, remaining) catch return true;
        switch (fit) {
            .ok => return true,
            .short => |detail| {
                self.pause(reason_disk_full, detail) catch |err| self.logErr("queue: cannot pause for disk space", err);
                return false;
            },
        }
    }

    // ---- progress ----

    /// Reports how far a running Download of the Queue has come, for both
    /// protocols alike: it publishes a `download_progress` event at most once
    /// per progress interval per Download (the one with done == total always
    /// goes out) and stores done_bytes and total_bytes at most once per done
    /// bytes interval. Every disk check interval it applies the disk guard to
    /// the bytes still to come (`checkDisk`). Returns whether it published.
    pub fn progress(self: *Queue, id: []const u8, done: i64, total: i64, bytes_per_second: i64) bool {
        const now = self.nowMs();
        const published = self.throttle.allow(id, now, done, total);
        if (published) {
            self.cfg.events.publish(.{ .download_progress = .{
                .id = id,
                .done_bytes = done,
                .total_bytes = total,
                .bytes_per_second = bytes_per_second,
            } });
        }
        self.mu.lockUncancelable(self.io);
        const due = self.dueLocked(&self.stored, id, now, self.cfg.done_bytes_interval_ms, true);
        self.mu.unlock(self.io);
        if (due) {
            self.cfg.downloads.updateProgress(id, done, total) catch |err| {
                if (err != error.NotFound and err != error.Canceled) self.logErrId("queue: cannot store progress", id, err);
            };
        }
        _ = self.checkDisk(id, @max(total - done, 0));
        return published;
    }

    /// Drops what `progress` remembers of a Download that stopped running, so
    /// its next run publishes and stores at once.
    pub fn forgetProgress(self: *Queue, id: []const u8) void {
        self.throttle.flush(id);
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        if (self.stored.fetchRemove(id)) |kv| self.gpa.free(kv.key);
        if (self.disk_checked.fetchRemove(id)) |kv| self.gpa.free(kv.key);
    }

    /// Whether `interval_ms` has passed since the time stored for `id`; if so
    /// the time becomes `now`. An unseen `id` is remembered and is due when
    /// `due_when_unseen`. When memory runs out nothing is remembered.
    fn dueLocked(self: *Queue, map: *std.StringHashMapUnmanaged(i64), id: []const u8, now: i64, interval_ms: i64, due_when_unseen: bool) bool {
        if (map.getPtr(id)) |last| {
            if (now -| last.* < interval_ms) return false;
            last.* = now;
            return true;
        }
        if (self.gpa.dupe(u8, id)) |key| {
            map.putNoClobber(self.gpa, key, now) catch self.gpa.free(key);
        } else |_| {}
        return due_when_unseen;
    }

    // ---- holds ----

    /// Keeps the Download from being started until `release` or `hold_ttl_ms`,
    /// and abandons its running fetch, waiting until it has stopped (cancel the
    /// calling task to bound the wait). Call it before deleting the files of a
    /// Download that still exists. Returns the protocol's error if it could not
    /// abandon the fetch; the hold stays.
    pub fn hold(self: *Queue, id: []const u8) anyerror!void {
        const until = self.nowMs() +| hold_ttl_ms;
        {
            self.mu.lockUncancelable(self.io);
            defer self.mu.unlock(self.io);
            if (self.holds.getPtr(id)) |p| {
                p.* = until;
            } else {
                const key = try self.gpa.dupe(u8, id);
                errdefer self.gpa.free(key);
                try self.holds.putNoClobber(self.gpa, key, until);
            }
        }
        if (!self.cfg.protocol.running(id)) return;
        try self.cfg.protocol.abandon(id);
    }

    /// Undoes `hold`. Reports whether the Download was held.
    pub fn release(self: *Queue, id: []const u8) bool {
        self.mu.lockUncancelable(self.io);
        const removed = self.holds.fetchRemove(id);
        self.mu.unlock(self.io);
        const kv = removed orelse return false;
        self.gpa.free(kv.key);
        self.cfg.wake.wake();
        return true;
    }

    /// Whether `hold` keeps the Download from being started.
    pub fn held(self: *Queue, id: []const u8) bool {
        const now = self.nowMs();
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        const until = self.holds.get(id) orelse return false;
        if (now < until) return true;
        if (self.holds.fetchRemove(id)) |kv| self.gpa.free(kv.key);
        return false;
    }

    // ---- helpers ----

    fn nowMs(self: *const Queue) i64 {
        return self.cfg.clock.nowMs(self.io);
    }

    fn logAt(self: *const Queue, level: log.Level, msg: []const u8, extra: []const log.Attr) void {
        const l = self.cfg.log orelse return;
        var attrs: [5]log.Attr = undefined;
        attrs[0] = log.Attr.string("queue", @tagName(self.cfg.source));
        const n = @min(extra.len, attrs.len - 1);
        @memcpy(attrs[1 .. 1 + n], extra[0..n]);
        l.log(level, msg, attrs[0 .. 1 + n]);
    }

    fn logInfo(self: *const Queue, msg: []const u8, extra: []const log.Attr) void {
        self.logAt(.info, msg, extra);
    }

    fn logWarn(self: *const Queue, msg: []const u8, extra: []const log.Attr) void {
        self.logAt(.warn, msg, extra);
    }

    /// Logs a failure; a canceled call is a shutdown, not a failure.
    fn logErr(self: *const Queue, msg: []const u8, err: anyerror) void {
        if (err == error.Canceled) return;
        self.logAt(.err, msg, &.{log.Attr.string("err", @errorName(err))});
    }

    fn logErrId(self: *const Queue, msg: []const u8, id: []const u8, err: anyerror) void {
        if (err == error.Canceled) return;
        self.logAt(.warn, msg, &.{ log.Attr.string("id", id), log.Attr.string("err", @errorName(err)) });
    }
};

fn contains(ids: []const []const u8, id: []const u8) bool {
    for (ids) |x| if (std.mem.eql(u8, x, id)) return true;
    return false;
}

fn byId(ds: []const dl.Download, id: []const u8) ?dl.Download {
    for (ds) |d| if (std.mem.eql(u8, d.id, id)) return d;
    return null;
}

fn freeKeys(gpa: Allocator, map: *std.StringHashMapUnmanaged(i64)) void {
    var it = map.keyIterator();
    while (it.next()) |k| gpa.free(k.*);
    map.deinit(gpa);
}
