//! The post-processing orchestrator (internal/core/postprocess.go). It takes a
//! Fetched Download through verify and repair, unpack and finish, and moves it
//! to ready or failed. It does none of the work itself: `par2`, `7zz` and the
//! finisher sit behind the ports of `postprocess_ports.zig`, and the fetch side
//! behind `FetchReporter` and `DeferredVolumes`.
//!
//! The pipeline of one Download (`d` is in post_processing):
//!
//!   1. The working directory (`DirFor`). When `fs` says it does not exist, a
//!      previous run got through Finish, which moved it, and stopped before
//!      ready was recorded: only Finish runs, with an empty unpacked dir.
//!   2. Usenet only: stage `verifying`, one Repair pass. Too little recovery
//!      data asks the fetch side for deferred volumes (`DeferredVolumes`) and,
//!      when it takes the Download back to fetching, ends here without a
//!      verdict: the Download is post-processed again when it is fetched again.
//!      Without a PAR2 set any damage the fetch saw is final.
//!   3. Stage `unpacking` when Detect finds archives, into `<dir>/_unpack`.
//!   4. Stage `finishing`: the Finisher cleans up and moves the result; its
//!      path becomes the Download's output.
//!
//! A pipeline failure is not an error of `process`: it is the Download's
//! failed state, with the reason and the redacted detail of
//! `postprocess_detail.zig`. `process` returns an error only when it could not
//! do its job: the Download is missing or in a state it cannot be processed
//! from, the repository failed, or the work was cancelled (the Download then
//! stays in post_processing for `recover`; "stop now, change nothing").
//!
//! Events, in this order: download_updated (post_processing), postprocess_stage
//! for each stage entered, then download_updated and ready, or download_updated
//! and failed.
//!
//! Threads: `process` and `recover` may be called from any task. `start` runs
//! three tasks (the bus reader, the control loop that starts Fetched Downloads
//! oldest first while slots are free, and one `recover`) and one task per
//! Download being worked on; `stop` ends them all. At most `concurrency`
//! Downloads are worked on at once and one mutex guards the slot count and the
//! set of Downloads being worked on; it is never held across a port, a
//! database call or a publish. Memory: an arena per Download, freed when its
//! pipeline ends; `create` allocates the PostProcessor and `destroy` frees it.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const event = @import("event.zig");
const events_bus = @import("events_bus.zig");
const log = @import("log.zig");
const queue_seams = @import("queue_seams.zig");
const repo_mod = @import("downloads_repo.zig");
const ports = @import("postprocess_ports.zig");
const detail = @import("postprocess_detail.zig");

pub const PortError = ports.PortError;
pub const Diag = ports.Diag;
pub const Failure = detail.Failure;

/// Where unpacked files go, under the Download's directory.
pub const unpack_subdir = "_unpack";
/// How often the control loop looks for Fetched Downloads besides reacting to
/// `fetched` events.
pub const default_poll_interval_ms: u32 = 10_000;

pub const Error = error{
    /// The Download is being post-processed by this PostProcessor right now.
    AlreadyPostProcessing,
} || repo_mod.Error || Allocator.Error;

/// The little the orchestrator itself needs from the filesystem.
pub const FS = struct {
    ctx: *anyopaque,
    dir_exists_fn: *const fn (ctx: *anyopaque, io: Io, dir: []const u8) anyerror!bool,

    /// Whether `dir` exists and is a directory.
    pub fn dirExists(f: FS, io: Io, dir: []const u8) anyerror!bool {
        return f.dir_exists_fn(f.ctx, io, dir);
    }
};

/// The working directory that holds a Download's fetched files. The fetch
/// side's layout is not the orchestrator's business. The path is allocated in
/// `arena`.
pub const DirFor = struct {
    ctx: *anyopaque,
    dir_fn: *const fn (ctx: *anyopaque, arena: Allocator, io: Io, d: dl.Download) anyerror![]const u8,

    pub fn dirFor(f: DirFor, arena: Allocator, io: Io, d: dl.Download) anyerror![]const u8 {
        return f.dir_fn(f.ctx, arena, io, d);
    }
};

pub const Config = struct {
    downloads: repo_mod.DownloadRepo,
    /// The bus the PostProcessor listens on and publishes to.
    bus: *events_bus.Bus,
    repairer: ports.Repairer,
    unpacker: ports.Unpacker,
    finisher: ports.Finisher,
    fetch_reporter: ports.FetchReporter,
    dir_for: DirFor,
    /// Without it a recovery shortfall is final.
    deferred: ?ports.DeferredVolumes = null,
    /// Without it the working directory is not checked.
    fs: ?FS = null,
    /// How many Downloads are post-processed at once: the setting
    /// `queues.postproc_concurrency` (`effectivePostprocConcurrency`), read
    /// once at start. Zero means 1.
    concurrency: usize = 1,
    poll_interval_ms: u32 = default_poll_interval_ms,
    clock: queue_seams.Clock = .{},
    /// Null logs nothing.
    log: ?log.Logger = null,
};

/// What a pipeline decided.
const Outcome = union(enum) {
    /// The output path, from the Finisher.
    ready: []const u8,
    /// The fetch side took the Download back for recovery volumes.
    handoff,
    failed: Failure,
};

pub const PostProcessor = struct {
    gpa: Allocator,
    io: Io,
    cfg: Config,
    limit: usize,

    /// Guards `busy` and `inflight`.
    mu: Io.Mutex = .init,
    slot_cond: Io.Condition = .init,
    /// Downloads being worked on.
    busy: usize = 0,
    /// IDs (gpa) of the Downloads being worked on.
    inflight: std.StringHashMapUnmanaged(void) = .empty,

    /// Set by `stop`: ports that fail while it is set are taken to have been
    /// cancelled, whatever they say.
    stopping: std.atomic.Value(bool) = .init(false),

    /// Guards the wake-up state and the subscription.
    wake_mu: Io.Mutex = .init,
    wake_cond: Io.Condition = .init,
    pending: bool = false,
    sub: ?*events_bus.Subscription = null,
    started: bool = false,
    /// The bus reader, the control loop and `recover`.
    tasks: Io.Group = .init,
    /// One task per Download started by the control loop.
    jobs: Io.Group = .init,

    /// Returns a PostProcessor that does nothing until `process`, `recover` or
    /// `start`. Free it with `destroy`.
    pub fn create(gpa: Allocator, io: Io, cfg: Config) Allocator.Error!*PostProcessor {
        const pp = try gpa.create(PostProcessor);
        pp.* = .{ .gpa = gpa, .io = io, .cfg = cfg, .limit = @max(cfg.concurrency, 1) };
        if (pp.cfg.poll_interval_ms == 0) pp.cfg.poll_interval_ms = default_poll_interval_ms;
        return pp;
    }

    /// Stops the PostProcessor if it runs and frees it.
    pub fn destroy(pp: *PostProcessor) void {
        pp.stop();
        var it = pp.inflight.keyIterator();
        while (it.next()) |k| pp.gpa.free(k.*);
        pp.inflight.deinit(pp.gpa);
        pp.gpa.destroy(pp);
    }

    // ---- lifecycle ----

    /// Starts post-processing in the background: it first recovers Downloads
    /// left in post_processing, then handles Fetched ones as they come, from
    /// the `fetched` events and a poll. The PostProcessor subscribes to the bus
    /// before it returns, so no event after that is missed.
    pub fn start(pp: *PostProcessor) !void {
        if (pp.started) return;
        pp.started = true;
        errdefer pp.stop();
        pp.stopping.store(false, .release);
        const sub = try pp.cfg.bus.subscribe();
        pp.wake_mu.lockUncancelable(pp.io);
        pp.sub = sub;
        pp.pending = true; // the first pass looks at once
        pp.wake_mu.unlock(pp.io);
        try pp.tasks.concurrent(pp.io, eventsMain, .{pp});
        try pp.tasks.concurrent(pp.io, controlMain, .{pp});
        try pp.tasks.concurrent(pp.io, recoverMain, .{pp});
    }

    /// Ends the tasks and every Download being worked on: they stay in
    /// post_processing, with no outcome published. Returns once they have
    /// stopped.
    pub fn stop(pp: *PostProcessor) void {
        if (!pp.started) return;
        pp.stopping.store(true, .release);
        pp.wake_mu.lockUncancelable(pp.io);
        if (pp.sub) |s| s.close();
        pp.wake_cond.broadcast(pp.io);
        pp.wake_mu.unlock(pp.io);
        pp.mu.lockUncancelable(pp.io);
        pp.slot_cond.broadcast(pp.io);
        pp.mu.unlock(pp.io);
        pp.tasks.cancel(pp.io);
        pp.jobs.cancel(pp.io);
        pp.started = false;
    }

    /// Asks the control loop to look for Fetched Downloads soon. It never
    /// blocks, so the runner's `fetched` hook may call it.
    pub fn poke(pp: *PostProcessor) void {
        pp.wake_mu.lockUncancelable(pp.io);
        pp.pending = true;
        pp.wake_cond.signal(pp.io);
        pp.wake_mu.unlock(pp.io);
    }

    // ---- process and recover ----

    /// Post-processes the Fetched Download `id` to completion (see the top of
    /// the file), waiting for a free slot first. A Download that waits for a
    /// slot stays Fetched. `error.AlreadyPostProcessing` when this
    /// PostProcessor is working on it; `error.IllegalTransition` when it is
    /// not Fetched; `error.NotFound` when it does not exist; `error.Canceled`
    /// when the work was cancelled.
    pub fn process(pp: *PostProcessor, id: []const u8) Error!void {
        if (!try pp.claim(id)) return error.AlreadyPostProcessing;
        defer pp.unclaim(id);
        try pp.acquireSlot();
        defer pp.releaseSlot();
        return pp.startOne(id);
    }

    /// Post-processes again, from the first stage, every Download found in
    /// post_processing: the daemon stopped while it was being worked on. It
    /// respects the concurrency limit and returns when they are all done,
    /// with the first error if any failed to be processed.
    pub fn recover(pp: *PostProcessor) Error!void {
        var scratch: std.heap.ArenaAllocator = .init(pp.gpa);
        defer scratch.deinit();
        const a = scratch.allocator();
        var stuck: std.ArrayList([]const u8) = .empty;
        var q: repo_mod.Query = .{ .state = .post_processing };
        while (true) {
            const page = try pp.cfg.downloads.list(a, q);
            for (page.items) |d| try stuck.append(a, d.id);
            q.after = page.next orelse break;
        }

        var shared: Recovery = .{};
        var group: Io.Group = .init;
        defer group.cancel(pp.io);
        for (stuck.items) |id| {
            if (!try pp.claim(id)) continue;
            pp.acquireSlot() catch |err| {
                pp.unclaim(id);
                shared.note(pp.io, err);
                break;
            };
            group.concurrent(pp.io, resumeMain, .{ pp, id, &shared }) catch resumeMain(pp, id, &shared);
        }
        group.await(pp.io) catch |err| shared.note(pp.io, err);
        if (shared.first) |err| return err;
    }

    const Recovery = struct {
        mu: Io.Mutex = .init,
        first: ?Error = null,

        fn note(r: *Recovery, io: Io, err: Error) void {
            r.mu.lockUncancelable(io);
            defer r.mu.unlock(io);
            if (r.first == null) r.first = err;
        }
    };

    fn resumeMain(pp: *PostProcessor, id: []const u8, shared: *Recovery) void {
        const err = pp.resumeOne(id);
        pp.releaseSlot();
        pp.unclaim(id);
        err catch |e| shared.note(pp.io, e);
    }

    // ---- the tasks ----

    fn recoverMain(pp: *PostProcessor) void {
        pp.recover() catch |err| {
            if (err != error.Canceled and !pp.stopping.load(.acquire))
                pp.logError("post-processing: recover failed", &.{log.Attr.string("err", @errorName(err))});
        };
        pp.poke();
    }

    fn controlMain(pp: *PostProcessor) void {
        const io = pp.io;
        while (true) {
            pp.wake_mu.lockUncancelable(io);
            const timeout: Io.Timeout = .{ .duration = .{ .raw = .fromMilliseconds(pp.cfg.poll_interval_ms), .clock = .awake } };
            while (!pp.pending and !pp.stopping.load(.acquire)) {
                pp.wake_cond.waitTimeout(io, &pp.wake_mu, timeout) catch |err| switch (err) {
                    error.Timeout => break,
                    error.Canceled => {
                        pp.wake_mu.unlock(io);
                        return;
                    },
                };
            }
            if (pp.stopping.load(.acquire)) {
                pp.wake_mu.unlock(io);
                return;
            }
            pp.pending = false;
            pp.wake_mu.unlock(io);
            pp.dispatch();
        }
    }

    fn eventsMain(pp: *PostProcessor) void {
        const io = pp.io;
        var sub = blk: {
            pp.wake_mu.lockUncancelable(io);
            defer pp.wake_mu.unlock(io);
            break :blk pp.sub orelse return;
        };
        while (true) {
            const item = sub.next(null) catch |err| switch (err) {
                error.Timeout => continue,
                error.Canceled => break,
                error.Closed => {
                    // Closed by stop, or dropped for being slow: then a
                    // `fetched` event may have been missed, so subscribe again
                    // and look.
                    if (sub.reason() == null) break;
                    sub.deinit();
                    const fresh = pp.cfg.bus.subscribe() catch {
                        pp.wake_mu.lockUncancelable(io);
                        pp.sub = null;
                        pp.wake_mu.unlock(io);
                        return;
                    };
                    pp.wake_mu.lockUncancelable(io);
                    const stopping = pp.stopping.load(.acquire);
                    pp.sub = fresh;
                    pp.wake_mu.unlock(io);
                    sub = fresh;
                    if (stopping) fresh.close();
                    pp.poke();
                    continue;
                },
            };
            defer item.release();
            if (item.event() == .fetched) pp.poke();
        }
        pp.wake_mu.lockUncancelable(io);
        pp.sub = null;
        pp.wake_mu.unlock(io);
        sub.deinit();
    }

    /// Starts Fetched Downloads, oldest first, while there are free slots. It
    /// does not wait for them.
    fn dispatch(pp: *PostProcessor) void {
        while (!pp.stopping.load(.acquire)) {
            if (!pp.tryAcquireSlot()) return; // all slots busy
            const id = pp.claimNextFetched() orelse {
                pp.releaseSlot();
                return;
            };
            pp.jobs.concurrent(pp.io, startMain, .{ pp, id }) catch |err| {
                pp.unclaim(id);
                pp.releaseSlot();
                pp.gpa.free(id);
                pp.logError("post-processing: cannot start a task", &.{log.Attr.string("err", @errorName(err))});
                return;
            };
        }
    }

    /// The task of one Download; owns `id` (gpa).
    fn startMain(pp: *PostProcessor, id: []const u8) void {
        defer pp.gpa.free(id);
        const result = pp.startOne(id);
        pp.unclaim(id);
        pp.releaseSlot();
        if (result) {
            pp.poke(); // a slot is free: look for more
        } else |err| switch (err) {
            error.Canceled => {},
            // The Download was removed while it was being processed.
            error.NotFound => pp.logInfo("post-processing: download vanished", &.{log.Attr.string("id", id)}),
            // Left for the next poll rather than retried in a hot loop.
            else => if (!pp.stopping.load(.acquire)) pp.logError("post-processing: download not processed", &.{
                log.Attr.string("id", id),
                log.Attr.string("err", @errorName(err)),
            }),
        }
    }

    /// Claims the oldest Fetched Download nobody is working on; the ID is a
    /// copy the caller frees.
    fn claimNextFetched(pp: *PostProcessor) ?[]const u8 {
        var scratch: std.heap.ArenaAllocator = .init(pp.gpa);
        defer scratch.deinit();
        const page = pp.cfg.downloads.list(scratch.allocator(), .{ .state = .fetched, .limit = 50 }) catch |err| {
            if (err != error.Canceled and !pp.stopping.load(.acquire))
                pp.logError("post-processing: list fetched downloads", &.{log.Attr.string("err", @errorName(err))});
            return null;
        };
        for (page.items) |d| {
            const claimed = pp.claim(d.id) catch return null;
            if (!claimed) continue;
            return pp.gpa.dupe(u8, d.id) catch {
                pp.unclaim(d.id);
                return null;
            };
        }
        return null;
    }

    // ---- slots and claims ----

    /// Marks `id` as being worked on, and reports false if it already was.
    fn claim(pp: *PostProcessor, id: []const u8) Allocator.Error!bool {
        pp.mu.lockUncancelable(pp.io);
        defer pp.mu.unlock(pp.io);
        if (pp.inflight.contains(id)) return false;
        const key = try pp.gpa.dupe(u8, id);
        pp.inflight.putNoClobber(pp.gpa, key, {}) catch |err| {
            pp.gpa.free(key);
            return err;
        };
        return true;
    }

    fn unclaim(pp: *PostProcessor, id: []const u8) void {
        pp.mu.lockUncancelable(pp.io);
        defer pp.mu.unlock(pp.io);
        if (pp.inflight.fetchRemove(id)) |kv| pp.gpa.free(kv.key);
    }

    /// Waits for a free slot. A cancelled task or a stopped PostProcessor never
    /// starts new work.
    fn acquireSlot(pp: *PostProcessor) error{Canceled}!void {
        try pp.io.checkCancel();
        try pp.mu.lock(pp.io);
        defer pp.mu.unlock(pp.io);
        while (true) {
            if (pp.stopping.load(.acquire)) return error.Canceled;
            if (pp.busy < pp.limit) {
                pp.busy += 1;
                return;
            }
            try pp.slot_cond.wait(pp.io, &pp.mu);
        }
    }

    fn tryAcquireSlot(pp: *PostProcessor) bool {
        pp.mu.lockUncancelable(pp.io);
        defer pp.mu.unlock(pp.io);
        if (pp.busy >= pp.limit) return false;
        pp.busy += 1;
        return true;
    }

    fn releaseSlot(pp: *PostProcessor) void {
        pp.mu.lockUncancelable(pp.io);
        defer pp.mu.unlock(pp.io);
        pp.busy -= 1;
        pp.slot_cond.signal(pp.io);
    }

    // ---- one Download ----

    /// Moves the Fetched Download `id` to post_processing and runs it.
    fn startOne(pp: *PostProcessor, id: []const u8) Error!void {
        var arena: std.heap.ArenaAllocator = .init(pp.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const d = try pp.cfg.downloads.updateState(a, id, .{ .to = .post_processing, .at = pp.now() });
        pp.publish(.{ .download_updated = d });
        return pp.run(a, d);
    }

    /// Runs the Download `id`, which a previous run left in post_processing,
    /// again from the first stage.
    fn resumeOne(pp: *PostProcessor, id: []const u8) Error!void {
        var arena: std.heap.ArenaAllocator = .init(pp.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const d = try pp.cfg.downloads.get(a, id);
        if (d.state != .post_processing) return error.IllegalTransition;
        return pp.run(a, d);
    }

    /// Takes a Download that is in post_processing through the pipeline and
    /// records the outcome.
    fn run(pp: *PostProcessor, a: Allocator, d: dl.Download) Error!void {
        const out = try pp.pipeline(a, d);
        // Stopped: the verdict, even a good one, is not recorded. A restart
        // runs the Download again, and Finish copes with having run before.
        if (pp.stopping.load(.acquire)) return error.Canceled;
        switch (out) {
            .handoff => pp.logInfo("post-processing handed back to the fetch side for recovery volumes", &.{log.Attr.string("id", d.id)}),
            .failed => |f| try pp.fail(a, d, f),
            .ready => |path| {
                const ready = pp.cfg.downloads.updateState(a, d.id, .{ .to = .ready, .at = pp.now(), .output_path = path }) catch |err| switch (err) {
                    error.Canceled => return err,
                    else => return pp.fail(a, d, try detail.failure(a, .finishing, error.Failed, .{ .message = recordError(err) })),
                };
                pp.publish(.{ .download_updated = ready });
                pp.publish(.{ .ready = .{ .id = d.id, .output_path = path } });
            },
        }
    }

    /// The text of Go's error for a state change that was refused.
    fn recordError(err: anyerror) []const u8 {
        return switch (err) {
            error.InvalidStateChange => "core: ready without output path: invalid state change",
            error.IllegalTransition => "illegal state transition",
            error.NotFound => "not found",
            else => @errorName(err),
        };
    }

    /// Moves `d` to failed and publishes the events. It never touches the
    /// Download's data: torrents keep seeding.
    fn fail(pp: *PostProcessor, a: Allocator, d: dl.Download, f: Failure) Error!void {
        const failed = try pp.cfg.downloads.updateState(a, d.id, .{
            .to = .failed,
            .at = pp.now(),
            .failure_reason = f.reason,
            .failure_detail = f.detail,
        });
        pp.logWarn("post-processing failed", &.{
            log.Attr.string("id", d.id),
            log.Attr.string("reason", @tagName(f.reason)),
            log.Attr.string("detail", f.detail),
        });
        pp.publish(.{ .download_updated = failed });
        pp.publish(.{ .failed = .{ .id = d.id, .reason = f.reason, .detail = f.detail } });
    }

    /// Runs the stages and returns what they decided. `error.Canceled` when the
    /// work was cancelled.
    fn pipeline(pp: *PostProcessor, a: Allocator, d: dl.Download) Error!Outcome {
        const io = pp.io;
        const dir = pp.cfg.dir_for.dirFor(a, io, d) catch |err| return pp.internal(a, "resolve working directory", err);
        if (pp.cfg.fs) |fs| {
            const exists = fs.dirExists(io, dir) catch |err| return pp.internal(a, "check working directory", err);
            // A previous run got through Finish, which moved the directory,
            // and stopped before recording ready. Finish is idempotent and
            // finds the moved result again, so go straight to it.
            if (!exists) return pp.finish(a, d, dir, "");
        }
        // Torrents are verified by the torrent engine piece by piece, so only
        // Usenet Downloads go through verify and repair.
        if (d.source_kind != .torrent) {
            if (try pp.verifyAndRepair(a, d, dir)) |out| return out;
        }
        var unpacked: []const u8 = "";
        var diag: Diag = .{};
        const found = pp.cfg.unpacker.detect(a, io, dir, &diag) catch |err| return pp.portFailed(a, .unpacking, err, diag);
        if (found) {
            pp.stage(d.id, .unpacking);
            const dst = try std.fs.path.join(a, &.{ dir, unpack_subdir });
            diag = .{};
            _ = pp.cfg.unpacker.unpack(a, io, dir, dst, d.id, &diag) catch |err| return pp.portFailed(a, .unpacking, err, diag);
            unpacked = dst;
        }
        return pp.finish(a, d, dir, unpacked);
    }

    /// The finishing stage.
    fn finish(pp: *PostProcessor, a: Allocator, d: dl.Download, dir: []const u8, unpacked: []const u8) Error!Outcome {
        pp.stage(d.id, .finishing);
        var diag: Diag = .{};
        const out = pp.cfg.finisher.finish(a, pp.io, d, dir, unpacked, &diag) catch |err| return pp.portFailed(a, .finishing, err, diag);
        return .{ .ready = out };
    }

    /// The verifying stage: one Repair that checks the files against the PAR2
    /// set and repairs what it can. Without a PAR2 set any damage the fetch
    /// side saw is final. Null means "go on".
    fn verifyAndRepair(pp: *PostProcessor, a: Allocator, d: dl.Download, dir: []const u8) Error!?Outcome {
        pp.stage(d.id, .verifying);
        var diag: Diag = .{};
        const out = pp.cfg.repairer.repair(a, pp.io, dir, &diag) catch |err| switch (err) {
            // Go tells a *RecoveryShortfall (the counts are known) from the
            // bare sentinel, which is a plain verifying failure.
            error.InsufficientRecovery => if (diag.needed > 0) return try pp.shortfall(a, d, diag) else return try pp.portFailed(a, .verifying, err, diag),
            error.RepairFailed => return try pp.portFailed(a, .repairing, err, diag),
            else => return try pp.portFailed(a, .verifying, err, diag),
        };
        if (out.has_par2) return null;
        const rep = pp.cfg.fetch_reporter.report(a, pp.io, d) catch |err| return try pp.portFailed(a, .verifying, err, .{});
        if (rep.damaged()) {
            const text = try std.fmt.allocPrint(a, "missing articles: {d} missing, {d} damaged and no PAR2 set", .{ rep.missing_articles, rep.damaged_articles });
            return .{ .failed = try detail.failure(a, .verifying, error.MissingArticles, .{ .message = text }) };
        }
        return null;
    }

    /// Handles too little recovery data: a handoff when the fetch side will
    /// fetch more, the shortfall itself otherwise.
    fn shortfall(pp: *PostProcessor, a: Allocator, d: dl.Download, diag: Diag) Error!Outcome {
        const dv = pp.cfg.deferred orelse return pp.portFailed(a, .repairing, error.InsufficientRecovery, diag);
        const need = @max(diag.needed - diag.available, 1);
        const more = dv.fetchDeferred(pp.io, d, need) catch |err| return pp.portFailed(a, .repairing, err, .{});
        if (more) return .handoff;
        return pp.portFailed(a, .repairing, error.InsufficientRecovery, diag);
    }

    /// A port failed: cancelled work is not a failure, anything else is the
    /// Download's.
    fn portFailed(pp: *PostProcessor, a: Allocator, stage_: event.Stage, err: PortError, diag: Diag) Error!Outcome {
        if (err == error.Canceled or pp.stopping.load(.acquire)) return error.Canceled;
        return .{ .failed = try detail.failure(a, stage_, err, diag) };
    }

    /// A failure of the orchestrator's own steps: no stage in the text.
    fn internal(pp: *PostProcessor, a: Allocator, what: []const u8, err: anyerror) Error!Outcome {
        if (err == error.Canceled or pp.stopping.load(.acquire)) return error.Canceled;
        if (err == error.OutOfMemory) return error.OutOfMemory;
        const text = try std.fmt.allocPrint(a, "{s}: {s}", .{ what, @errorName(err) });
        return .{ .failed = .{ .reason = .internal, .detail = try detail.redact(a, text) } };
    }

    // ---- small helpers ----

    fn now(pp: *PostProcessor) i64 {
        return pp.cfg.clock.nowMs(pp.io);
    }

    fn publish(pp: *PostProcessor, e: event.Event) void {
        pp.cfg.bus.publish(e);
    }

    fn stage(pp: *PostProcessor, id: []const u8, s: event.Stage) void {
        pp.publish(.{ .postprocess_stage = .{ .id = id, .stage = s } });
    }

    fn logInfo(pp: *PostProcessor, msg: []const u8, attrs: []const log.Attr) void {
        if (pp.cfg.log) |l| l.info(msg, attrs);
    }

    fn logWarn(pp: *PostProcessor, msg: []const u8, attrs: []const log.Attr) void {
        if (pp.cfg.log) |l| l.warn(msg, attrs);
    }

    fn logError(pp: *PostProcessor, msg: []const u8, attrs: []const log.Attr) void {
        if (pp.cfg.log) |l| l.err(msg, attrs);
    }
};

test {
    _ = detail;
}
