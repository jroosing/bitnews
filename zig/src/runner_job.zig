//! One fetch of one Download (internal/usenet/runner job.go): read the staged
//! NZB, plan which Articles to get, run the Article scheduler over the pools of
//! the configured Servers, write every part through the file writer, report
//! progress, save the resume state now and then, and end with the Download
//! Fetched, Failed or back in the Queue.
//!
//! Threads. A job is one task of its own (`jobMain`, started by `Runner.start`
//! in a `Group` of the job) that never takes an Io cancellation: it is
//! stopped through `Job.cancel`, which sets a flag the job and the fetchers
//! look at, and wakes it. The scheduler runs as a task of its own under the
//! job, so it can be cancelled (stopped by a pause, restarted when the Servers
//! change) without touching the job. The result sink runs on the scheduler's
//! worker tasks, concurrently; everything it touches is behind `Fetch.mu`.
//! Cancellation can be lost in std 0.17 when it comes right after a task
//! started, so every cancel of a run also sets `Job.stop`, which makes the
//! counting fetchers answer "this Server is down": the workers end at their
//! next Article and the scheduler resolves what is left by itself.
//!
//! Memory. One arena per job holds the parsed NZB, the plan and the statuses;
//! it is freed when the job ends, whatever the outcome. The writer and the
//! rate window are freed by the job too. The `Job` shell (control state only)
//! lives until the Runner reaps it.
//!
//! Lock order: `Runner.mu`, then `Job.mu`, never the other way round. In the result
//! sink `Fetch.report_mu` comes before `Fetch.mu`, and the Queue is called with
//! only `report_mu` held.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const fsutil = @import("fsutil.zig");
const filewriter = @import("filewriter.zig");
const log = @import("log.zig");
const nzb = @import("nzb.zig");
const rate_mod = @import("rate.zig");
const sched = @import("scheduler.zig");
const blob_mod = @import("runner_blob.zig");
const limiter_mod = @import("runner_limiter.zig");
const plan_mod = @import("runner_plan.zig");
const seams = @import("runner_seams.zig");
const pools_mod = @import("runner_pools.zig");
const runner = @import("runner.zig");

const Writer = filewriter.Writer;
const Plan = plan_mod.Plan;
const Status = blob_mod.Status;
const FetchDetail = seams.FetchDetail;

/// How long a Download waits in Queued after every Server turned out to be
/// unusable (a wrong password, a provider outage).
pub const default_failure_backoff_ms: i64 = 30_000;

/// Why a job was stopped. The first cause wins.
pub const Cause = enum {
    /// The Download was removed (or is held for its removal): the resume state
    /// goes too.
    removed,
    /// The Queue stopped it (paused, pre-empted, over the limit): the resume
    /// state is kept.
    deactivated,
    /// The disk guard stopped it: the resume state is kept.
    disk_full,
    /// The daemon is shutting down: the resume state is kept.
    shutdown,
};

/// One scheduler run of a job. Guarded by `Job.mu`.
pub const Run = struct {
    done: bool = false,
    result: ?sched.RunError = null,
};

/// The control handle of a running fetch. Created by `Runner.start`, freed by
/// the Runner once the job has ended.
pub const Job = struct {
    r: *runner.Runner,
    gpa: Allocator,
    id: []u8,
    total_bytes: i64,
    /// The one task of the job.
    group: Io.Group = .init,

    mu: Io.Mutex = .init,
    /// Signalled when a run ends or the job is asked to stop or restart.
    cond: Io.Condition = .init,
    // Guarded by `mu`.
    cause: ?Cause = null,
    reschedule: bool = false,
    /// The job is past the point of cancellation: it is about to leave
    /// Fetching on its own.
    finished: bool = false,

    /// The current scheduler run must end. Set (under `mu`) by `cancel`,
    /// `restartRun` and a fatal error of the sink; the counting fetchers then
    /// answer every request with "this Server is down", so each worker ends at
    /// its next Article and the scheduler resolves what is left by itself, also
    /// when the Io lost the cancellation of the run (std 0.17 can drop one that
    /// comes right after a task started). The Articles resolved that way are
    /// ignored: they stay unresolved for the next run.
    stop: std.atomic.Value(bool) = .init(false),

    /// Guarded by `Runner.mu`: the task has returned (or is about to).
    ended: bool = false,

    pub fn create(r: *runner.Runner, d: dl.Download) Allocator.Error!*Job {
        const gpa = r.gpa;
        const j = try gpa.create(Job);
        errdefer gpa.destroy(j);
        const id = try gpa.dupe(u8, d.id);
        j.* = .{ .r = r, .gpa = gpa, .id = id, .total_bytes = d.total_bytes };
        return j;
    }

    pub fn destroy(j: *Job) void {
        const gpa = j.gpa;
        gpa.free(j.id);
        gpa.destroy(j);
    }

    /// Stops the fetch for `cause`, unless it is already finishing. Does not
    /// wait.
    pub fn cancel(j: *Job, why: Cause) void {
        const io = j.r.io;
        j.mu.lockUncancelable(io);
        defer j.mu.unlock(io);
        if (j.finished) return;
        if (j.cause == null) j.cause = why;
        j.stop.store(true, .release);
        j.cond.broadcast(io);
    }

    /// Restarts the scheduler with the current Servers.
    pub fn restartRun(j: *Job) void {
        const io = j.r.io;
        j.mu.lockUncancelable(io);
        defer j.mu.unlock(io);
        if (j.finished) return;
        j.reschedule = true;
        j.stop.store(true, .release);
        j.cond.broadcast(io);
    }

    fn causeNow(j: *Job) ?Cause {
        const io = j.r.io;
        j.mu.lockUncancelable(io);
        defer j.mu.unlock(io);
        return j.cause;
    }

    /// Marks the job as past the point of cancellation.
    fn finish(j: *Job) void {
        const io = j.r.io;
        j.mu.lockUncancelable(io);
        defer j.mu.unlock(io);
        j.finished = true;
    }

    /// Arms `stop` for a new run: set already when a stop or a restart came in
    /// meanwhile.
    fn armRun(j: *Job) void {
        const io = j.r.io;
        j.mu.lockUncancelable(io);
        defer j.mu.unlock(io);
        j.stop.store(j.cause != null or j.reschedule, .release);
    }

    /// Ends the current run's workers, for a fatal error found by the sink.
    fn stopRun(j: *Job) void {
        const io = j.r.io;
        j.mu.lockUncancelable(io);
        defer j.mu.unlock(io);
        j.stop.store(true, .release);
        j.cond.broadcast(io);
    }
};

/// The body of the job's task.
pub fn jobMain(job: *Job) void {
    const r = job.r;
    // Nothing cancels this task through the Io; the store and the file calls
    // below must not turn a stray request into a half-finished fetch.
    const prev = r.io.swapCancelProtection(.blocked);
    defer _ = r.io.swapCancelProtection(prev);
    {
        var arena: std.heap.ArenaAllocator = .init(r.gpa);
        defer arena.deinit();
        fetchOne(job, arena.allocator());
    }
    r.jobEnded(job);
}

// ---- the fetch ----

fn logWarn(r: *runner.Runner, msg: []const u8, extra: []const log.Attr) void {
    const l = r.cfg.log orelse return;
    l.warn(msg, extra);
}

/// Moves the Download to Failed (and tells the bus). `detail` is already
/// redacted.
fn failJob(job: *Job, a: Allocator, reason: dl.FailureReason, detail: []const u8) void {
    const r = job.r;
    job.finish();
    r.progress.delete(job.id) catch {};
    _ = r.q.setState(a, job.id, .{ .to = .failed, .at = r.nowMs(), .failure_reason = reason, .failure_detail = detail }) catch |err| {
        logWarn(r, "usenet runner: cannot fail download", &.{ log.Attr.string("id", job.id), log.Attr.string("err", @errorName(err)) });
        return;
    };
    r.publish(.{ .failed = .{ .id = job.id, .reason = reason, .detail = detail } });
}

/// Puts the Download back to Queued and, when `wait_ms` is positive, keeps it
/// from being activated again for that long.
fn requeueJob(job: *Job, a: Allocator, wait_ms: i64) void {
    const r = job.r;
    job.finish();
    if (wait_ms > 0) r.setCooldown(job.id, r.nowMs() +| wait_ms);
    _ = r.q.setState(a, job.id, .{ .to = .queued, .at = r.nowMs() }) catch |err| {
        logWarn(r, "usenet runner: cannot requeue download", &.{ log.Attr.string("id", job.id), log.Attr.string("err", @errorName(err)) });
    };
}

fn failMsg(job: *Job, a: Allocator, reason: dl.FailureReason, comptime fmt: []const u8, args: anytype) void {
    const detail = std.fmt.allocPrint(a, fmt, args) catch "out of memory";
    failJob(job, a, reason, detail);
}

/// The stored NZB of a Download, or why it cannot be used (the text is the
/// failure detail).
pub const Loaded = union(enum) {
    ok: nzb.Nzb,
    failed: []const u8,
};

/// Reads and parses the stored NZB of `id` into `a`; `path_out` receives the
/// path of the staged file. The error texts are the Go ones.
pub fn loadNzb(r: *runner.Runner, a: Allocator, id: []const u8, path_out: ?*[]const u8) Allocator.Error!Loaded {
    const found = r.cfg.sources.path(a, id) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return .{ .failed = try std.fmt.allocPrint(a, "find nzb: {s}", .{@errorName(err)}) },
    };
    const path = found orelse return .{ .failed = "find nzb: no stored source" };
    if (path_out) |p| p.* = path;
    const limits = nzb.default_limits;
    const bytes = Io.Dir.cwd().readFileAlloc(r.io, path, r.gpa, .limited(limits.max_bytes + 1)) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.StreamTooLong => return .{ .failed = try std.fmt.allocPrint(a, "parse nzb: {s}", .{try nzb.errorMessage(a, error.TooLarge, limits, .{})}) },
        else => return .{ .failed = try std.fmt.allocPrint(a, "open nzb: {s}", .{@errorName(err)}) },
    };
    defer r.gpa.free(bytes);
    var diag: nzb.Diag = .{};
    const parsed = nzb.parseWithLimits(a, bytes, limits, &diag) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return .{ .failed = try std.fmt.allocPrint(a, "parse nzb: {s}", .{try nzb.errorMessage(a, err, limits, diag)}) },
    };
    return .{ .ok = parsed };
}

/// All the working state of one fetch, on the stack of `fetchOne`.
const Fetch = struct {
    r: *runner.Runner,
    job: *Job,
    a: Allocator,
    id: []const u8,
    dir: []const u8,
    plan: Plan,
    /// Parallel to `plan.articles`.
    status: []Status,
    w: *Writer = undefined,
    /// A supplement run only fetches un-deferred recovery volumes (file
    /// indexes in `volumes`) of a Download fetched before. Its progress, report
    /// and totals are not published: they would replace those of the Download's
    /// main fetch.
    supplement: bool,
    volumes: []const u32,
    total: i64 = 0,

    mu: Io.Mutex = .init,
    /// Serializes the progress reports; taken before `mu`.
    report_mu: Io.Mutex = .init,
    // Guarded by `mu`.
    /// Decoded bytes written, including restored ones.
    done: i64 = 0,
    fatal: ?anyerror = null,
    next_save_ms: i64 = 0,
    rate: rate_mod.RateWindow = undefined,
    /// Counted for this fetch only, to recognize unusable Servers.
    run_done: u64 = 0,
    run_missing: u64 = 0,

    answered: std.atomic.Value(u64) = .init(0),
    /// A progress event with done == total went out.
    final_sent: std.atomic.Value(bool) = .init(false),

    fn fatalErr(f: *Fetch) ?anyerror {
        const io = f.r.io;
        f.mu.lockUncancelable(io);
        defer f.mu.unlock(io);
        return f.fatal;
    }

    /// True when this fetch resolved Articles only as Missing and no Server
    /// ever answered a request.
    fn unusable(f: *Fetch) bool {
        const io = f.r.io;
        f.mu.lockUncancelable(io);
        defer f.mu.unlock(io);
        return f.run_done == 0 and f.run_missing > 0 and f.answered.load(.monotonic) == 0;
    }

    // ---- the sinks of the scheduler ----

    /// One Article's outcome. Runs on the scheduler's worker tasks.
    pub fn result(f: *Fetch, res: sched.Result) void {
        const r = f.r;
        const io = r.io;
        // The store and file calls below finish even when the run is being
        // cancelled; the cancellation stays pending for the worker.
        const prev = io.swapCancelProtection(.blocked);
        defer _ = io.swapCancelProtection(prev);

        // After a stop the scheduler resolves what is left as missing (or, with
        // a damaged copy in hand, as damaged) because the Servers "went down":
        // those Articles are not lost, just not fetched yet.
        if ((res.status == .missing or res.damaged) and f.job.stop.load(.acquire)) return;
        const idx = f.plan.find(res.article.file_key, res.article.segment_number) orelse return;
        var status: Status = .missing;
        var written: i64 = 0;
        if (res.status == .done) {
            if (res.part) |part| {
                if (f.w.writePart(res.article.file_key, part)) {
                    status = if (res.damaged) .damaged else .done;
                    written = @intCast(part.data.len);
                } else |err| switch (err) {
                    // A hostile or inconsistent Article counts as missing.
                    error.InvalidPart, error.SizeMismatch, error.TooLarge => logWarn(r, "usenet article rejected by the file writer", &.{
                        log.Attr.string("message_id", res.article.message_id),
                        log.Attr.string("err", @errorName(err)),
                    }),
                    else => {
                        f.mu.lockUncancelable(io);
                        if (f.fatal == null) f.fatal = err;
                        f.mu.unlock(io);
                        f.job.stopRun();
                        return;
                    },
                }
            }
        }

        f.mu.lockUncancelable(io);
        f.status[idx] = status;
        f.done += written;
        if (status == .missing) f.run_missing += 1 else f.run_done += 1;
        const now = r.nowMs();
        const save_due = now > f.next_save_ms;
        if (save_due) f.next_save_ms = now +| r.cfg.save_interval_ms;
        f.rate.add(now, f.done);
        f.mu.unlock(io);

        // Reports are taken in turn and read the bytes done at that moment, so
        // the events of concurrent workers never go back.
        f.report_mu.lockUncancelable(io);
        f.mu.lockUncancelable(io);
        const done = f.done;
        const total = f.total;
        const speed = f.rate.rate(r.nowMs());
        f.mu.unlock(io);
        if (f.supplement) {
            // A supplement run reports no progress, so it asks the disk guard
            // itself. A Queue paused for the disk does not stop it: it is not
            // one of the Queue's fetches.
            if (!r.q.checkDisk(f.id, @max(total - done, 0))) f.job.cancel(.disk_full);
        } else if (r.q.progress(f.id, done, total, speed) and total > 0 and done == total) {
            f.final_sent.store(true, .release);
        }
        f.report_mu.unlock(io);
        if (save_due) {
            f.saveBlob() catch |err| logWarn(r, "usenet runner: cannot save resume state", &.{
                log.Attr.string("id", f.id),
                log.Attr.string("err", @errorName(err)),
            });
        }
    }

    pub fn serverDown(f: *Fetch, name: []const u8, err: sched.FetchError) void {
        if (f.job.stop.load(.acquire)) return;
        logWarn(f.r, "usenet server down", &.{
            log.Attr.string("server", name),
            log.Attr.string("download", f.id),
            log.Attr.string("err", @errorName(err)),
        });
    }

    // ---- resume state ----

    /// Stores the resume state. The resolved Articles are copied before the
    /// writer snapshot is taken, so the snapshot always covers them.
    fn saveBlob(f: *Fetch) !void {
        const r = f.r;
        var tmp: std.heap.ArenaAllocator = .init(r.gpa);
        defer tmp.deinit();
        const t = tmp.allocator();

        f.mu.lockUncancelable(r.io);
        var recs: std.ArrayList(blob_mod.SegRecord) = .empty;
        recs.ensureTotalCapacity(t, f.status.len) catch |err| {
            f.mu.unlock(r.io);
            return err;
        };
        for (f.plan.articles, f.status) |art, st| {
            if (st == .unresolved) continue;
            recs.appendAssumeCapacity(.{ .file = art.file_key, .seg = art.segment_number, .status = st });
        }
        f.mu.unlock(r.io);

        const snap = try f.w.snapshot(t);
        const bytes = try blob_mod.encode(t, .{ .writer = snap, .segments = recs.items });
        try r.progress.save(f.id, bytes);
    }

    /// Handles a fetch that was stopped: the resume state is saved so the work
    /// is not lost, unless the Download was removed.
    fn stopped(f: *Fetch) void {
        const r = f.r;
        if (f.job.causeNow() == .removed) {
            r.progress.delete(f.id) catch {};
            return;
        }
        f.saveBlob() catch |err| logWarn(r, "usenet runner: cannot save resume state", &.{
            log.Attr.string("id", f.id),
            log.Attr.string("err", @errorName(err)),
        });
    }

    /// Creates the writer for the Download's directory. When the store holds a
    /// valid blob, the files and the resolved Articles are taken over;
    /// otherwise the directory is cleared (see `plan_mod.wipeDir`) and the
    /// fetch starts from scratch. For a supplement run the files of the
    /// earlier fetch are not touched: a blob is used only if it is about the
    /// volumes, and without one just leftovers of the volumes are removed.
    fn openWriter(f: *Fetch) !void {
        const r = f.r;
        var why: ?[]const u8 = null;
        const stored = r.progress.load(f.a, f.id) catch |err| blk: {
            logWarn(r, "usenet resume state unreadable, starting over", &.{
                log.Attr.string("id", f.id),
                log.Attr.string("err", @errorName(err)),
            });
            break :blk null;
        };
        if (stored) |bytes| {
            if (blob_mod.decode(f.a, bytes)) |rb| {
                if (f.supplement and !f.blobIsAbout(rb)) {
                    why = "the blob is not about the recovery volumes";
                } else {
                    const w = try Writer.init(r.gpa, r.io, f.dir, .{});
                    if (w.restore(f.a, rb.writer)) |reset| {
                        f.w = w;
                        f.adopt(rb, reset);
                        return;
                    } else |err| {
                        w.deinit();
                        why = @errorName(err);
                    }
                }
            } else |err| why = @errorName(err);
            logWarn(r, "usenet resume state unusable, starting over", &.{
                log.Attr.string("id", f.id),
                log.Attr.string("err", why orelse "unknown"),
            });
        }
        if (f.supplement) {
            try plan_mod.removeNamed(r.io, f.dir, f.plan.names);
        } else {
            try plan_mod.wipeDir(r.gpa, r.io, f.dir);
        }
        f.w = try Writer.init(r.gpa, r.io, f.dir, .{});
    }

    fn blobIsAbout(f: *Fetch, rb: blob_mod.Blob) bool {
        for (rb.segments) |s| if (f.plan.find(s.file, s.seg) == null) return false;
        return true;
    }

    /// Loads the resolved Articles of `rb` into the status, except those of
    /// files the writer had to reset or forget, and recomputes the done bytes
    /// from the writer.
    fn adopt(f: *Fetch, rb: blob_mod.Blob, reset: []const u32) void {
        // Which files the writer knows, asked once per file, not per record.
        var max_key: u32 = 0;
        for (f.plan.file_keys) |fk| max_key = @max(max_key, fk);
        const present = f.a.alloc(bool, @as(usize, max_key) + 1) catch return;
        @memset(present, false);
        for (f.plan.file_keys) |fk| {
            const fs = (f.w.ranges(f.a, fk) catch null) orelse continue;
            present[fk] = true;
            f.done += fs.doneBytes() + fs.damagedBytes();
        }
        for (rb.segments) |s| {
            const idx = f.plan.find(s.file, s.seg) orelse continue;
            if (std.mem.indexOfScalar(u32, reset, s.file) != null) continue;
            if (s.status != .missing and !present[s.file]) continue;
            f.status[idx] = s.status;
        }
    }

    // ---- outcomes ----

    fn fail(f: *Fetch, reason: dl.FailureReason, detail: []const u8) void {
        failJob(f.job, f.a, reason, detail);
    }

    /// The failure detail of an error from the writer: a refused write names
    /// the folder and the user id.
    fn describe(f: *Fetch, err: anyerror) []const u8 {
        if (fsutil.isPermissionError(err)) {
            return fsutil.writeErrorDetail(f.a, f.dir, fsutil.effectiveUid()) catch "permission denied";
        }
        return std.fmt.allocPrint(f.a, "write failed: {s}", .{@errorName(err)}) catch "write failed";
    }

    /// Finishes the files and moves the Download to Fetched.
    fn complete(f: *Fetch) void {
        const r = f.r;
        const io = r.io;
        var detail: FetchDetail = .{};
        f.mu.lockUncancelable(io);
        for (f.plan.articles, f.status) |art, st| switch (st) {
            .missing => {
                detail.missing_articles += 1;
                detail.missing_bytes +|= art.expected_bytes;
            },
            .damaged => detail.damaged_articles += 1,
            else => {},
        };
        const done_bytes = f.done;
        const total = f.total;
        f.mu.unlock(io);

        var first: ?anyerror = null;
        for (f.plan.file_keys) |fk| {
            var scratch: std.heap.ArenaAllocator = .init(r.gpa);
            defer scratch.deinit();
            const fs = (f.w.ranges(scratch.allocator(), fk) catch |err| {
                first = first orelse err;
                continue;
            }) orelse {
                detail.missing_files += 1;
                continue;
            };
            detail.damaged_bytes +|= fs.damagedBytes();
            f.w.finish(fk) catch |err| {
                first = first orelse err;
            };
        }
        f.w.sync() catch |err| {
            first = first orelse err;
        };
        f.w.close() catch |err| {
            first = first orelse err;
        };
        if (first) |err| {
            const detail_text = if (fsutil.isPermissionError(err)) f.describe(err) else std.fmt.allocPrint(f.a, "finish files: {s}", .{@errorName(err)}) catch "finish files";
            return f.fail(.internal, detail_text);
        }

        f.job.finish();
        r.progress.delete(f.id) catch {};
        if (f.supplement) {
            // Mark the volumes before the state change: a crash in between
            // leaves a Fetching Download with nothing to do, which completes.
            r.volumes.markFetched(f.id, f.volumes) catch |err| {
                return f.fail(.internal, std.fmt.allocPrint(f.a, "record fetched volumes: {s}", .{@errorName(err)}) catch "record fetched volumes");
            };
        } else {
            r.storeDoneBytes(f.a, f.id, done_bytes, f.plan.encoded);
            if (!f.final_sent.load(.acquire)) {
                r.publish(.{ .download_progress = .{ .id = f.id, .done_bytes = done_bytes, .total_bytes = @max(total, done_bytes) } });
            }
            r.q.forgetProgress(f.id);
            r.reports.save(f.id, detail) catch |err| logWarn(r, "usenet runner: cannot store fetch report", &.{
                log.Attr.string("id", f.id),
                log.Attr.string("err", @errorName(err)),
            });
        }
        _ = r.q.setState(f.a, f.id, .{ .to = .fetched, .at = r.nowMs() }) catch |err| {
            logWarn(r, "usenet runner: cannot mark download fetched", &.{ log.Attr.string("id", f.id), log.Attr.string("err", @errorName(err)) });
            return;
        };
        r.publish(.{ .fetched = f.id });
        if (r.cfg.on_fetched) |hook| hook.call(hook.ctx, f.id, f.supplement);
    }
};

fn isDiskFull(err: anyerror) bool {
    return err == error.NoSpaceLeft or err == error.DiskQuota;
}

/// The whole fetch of one active Download, which is Fetching. Returns when the
/// Download is Fetched, Failed or requeued, or when the job was stopped.
fn fetchOne(job: *Job, a: Allocator) void {
    const r = job.r;
    const id = job.id;

    var nzb_path: []const u8 = "";
    const loaded = loadNzb(r, a, id, &nzb_path) catch return failJob(job, a, .internal, "out of memory");
    const n = switch (loaded) {
        .ok => |parsed| parsed,
        .failed => |why| {
            // A job that was stopped meanwhile leaves the Download alone.
            if (job.causeNow() != null) return;
            return failJob(job, a, .source_invalid, why);
        },
    };
    const rows = r.volumes.list(a, id) catch |err| {
        if (job.causeNow() != null) return;
        return failMsg(job, a, .internal, "read un-deferred volumes: {s}", .{@errorName(err)});
    };
    // A Download with un-deferred volumes was fetched before: only the
    // volumes that are not fetched yet are left to do (a supplement run).
    var pending: std.ArrayList(u32) = .empty;
    for (rows) |row| if (!row.fetched) pending.append(a, row.file_index) catch return failJob(job, a, .internal, "out of memory");
    const supplement = rows.len > 0;

    r.pools.revive();
    const plan = Plan.build(a, n, if (supplement) pending.items else null) catch return failJob(job, a, .internal, "out of memory");
    if (plan.articles.len == 0 and !supplement) return failJob(job, a, .source_invalid, "the NZB has nothing to fetch");

    const status = a.alloc(Status, plan.articles.len) catch return failJob(job, a, .internal, "out of memory");
    @memset(status, .unresolved);
    var f: Fetch = .{
        .r = r,
        .job = job,
        .a = a,
        .id = id,
        .dir = std.fs.path.dirname(nzb_path) orelse ".",
        .plan = plan,
        .status = status,
        .supplement = supplement,
        .volumes = pending.items,
    };
    f.openWriter() catch |err| {
        if (job.causeNow() != null) return;
        const detail = if (fsutil.isPermissionError(err))
            (fsutil.writeErrorDetail(a, fsutil.deepestExistingParent(r.io, f.dir), fsutil.effectiveUid()) catch "permission denied")
        else
            std.fmt.allocPrint(a, "open files: {s}", .{@errorName(err)}) catch "open files";
        return failJob(job, a, .internal, detail);
    };
    defer {
        f.w.close() catch {};
        f.w.deinit();
    }
    f.rate = rate_mod.RateWindow.init(r.gpa, rate_mod.default_window_ms) catch return failJob(job, a, .internal, "out of memory");
    defer f.rate.deinit(r.gpa);

    const now = r.nowMs();
    f.next_save_ms = now +| r.cfg.save_interval_ms;
    f.rate.add(now, f.done);
    f.total = if (job.total_bytes != 0) job.total_bytes else plan.encoded;

    // Set when a run claims to have resolved everything; a second run for
    // Articles that are still open then is a bug, not a retry.
    var claimed_done = false;
    while (true) {
        if (job.causeNow() != null) return f.stopped();
        const rest = plan.remaining(a, status) catch return f.fail(.internal, "out of memory");
        if (rest.len == 0) break;
        if (f.fatalErr() != null) break;
        if (claimed_done) {
            return f.fail(.internal, std.fmt.allocPrint(a, "fetch ended with {d} articles unresolved", .{rest.len}) catch "fetch ended with articles unresolved");
        }

        var run_arena: std.heap.ArenaAllocator = .init(r.gpa);
        defer run_arena.deinit();
        const ra = run_arena.allocator();
        const snap = r.pools.snapshot(ra) catch return f.fail(.internal, "out of memory");
        defer r.pools.release(snap);
        if (snap.len == 0) return requeueJob(job, a, 0);

        switch (runOnce(&f, ra, snap, rest)) {
            .finished => claimed_done = true,
            .restart => continue,
            .stopped => return f.stopped(),
            .failed => return,
        }
        if (f.fatalErr() != null) break;
    }

    if (f.fatalErr()) |err| {
        if (isDiskFull(err)) {
            // The disk filled up behind the guard's back.
            f.stopped();
            r.q.diskFull("the disk is full: no space left on device") catch {};
            return requeueJob(job, a, 0);
        }
        return f.fail(.internal, f.describe(err));
    }
    if (f.unusable()) {
        // No Server answered a single request: wrong password or a provider
        // outage. Marking everything Missing would burn through the Queue.
        logWarn(r, "usenet: no server answered, download waits before retrying", &.{
            log.Attr.string("id", id),
            log.Attr.int("wait_ms", r.cfg.failure_backoff_ms),
        });
        return requeueJob(job, a, r.cfg.failure_backoff_ms);
    }
    f.complete();
}

const RunOutcome = enum {
    /// Every Article of the run is resolved, or a fatal error ended it.
    finished,
    /// The Servers changed: run again for what is left.
    restart,
    /// The job was stopped.
    stopped,
    /// The fetch failed and the Download was moved to Failed.
    failed,
};

fn schedTask(job: *Job, run: *Run, s: *sched.Scheduler) void {
    const io = job.r.io;
    const res = s.run(io);
    job.mu.lockUncancelable(io);
    run.result = if (res) |_| null else |err| err;
    run.done = true;
    job.cond.broadcast(io);
    job.mu.unlock(io);
}

/// Wraps a Fetcher of one Server with the counting and the speed limit.
fn countingFetchers(f: *Fetch, ra: Allocator, snap: []const pools_mod.Active) Allocator.Error![]limiter_mod.CountingFetcher {
    const out = try ra.alloc(limiter_mod.CountingFetcher, snap.len);
    for (out, snap) |*c, s| c.* = .{
        .inner = s.fetcher,
        .limiter = &f.r.limiter,
        .answered = &f.answered,
        .stop = &f.job.stop,
    };
    return out;
}

/// Runs the scheduler once over `rest` and the Servers of `snap`.
fn runOnce(f: *Fetch, ra: Allocator, snap: []const pools_mod.Active, rest: []const sched.Article) RunOutcome {
    const r = f.r;
    const job = f.job;
    const io = r.io;
    var run: Run = .{};
    const fetchers = countingFetchers(f, ra, snap) catch {
        f.fail(.internal, "out of memory");
        return .failed;
    };
    const n = @min(snap.len, sched.max_servers);
    if (snap.len > n) logWarn(r, "usenet: more servers than the scheduler supports, the last ones are not used", &.{log.Attr.int("limit", sched.max_servers)});
    const servers = ra.alloc(sched.Server, n) catch {
        f.fail(.internal, "out of memory");
        return .failed;
    };
    for (servers, snap[0..n], fetchers[0..n]) |*sv, s, *c| sv.* = .{
        .name = s.name,
        .priority = s.priority,
        .retention_days = s.retention_days,
        .workers = s.workers,
        .fetcher = c.fetcher(),
    };
    const s = sched.Scheduler.create(r.gpa, .{
        .servers = servers,
        .articles = rest,
        .on_result = .init(f),
        .on_server_down = .init(f),
        .backoff = r.cfg.backoff,
    }) catch |err| {
        f.fail(.internal, std.fmt.allocPrint(f.a, "cannot start the fetch: {s}", .{@errorName(err)}) catch "cannot start the fetch");
        return .failed;
    };
    defer s.destroy();

    job.armRun();
    var fut = io.concurrent(schedTask, .{ job, &run, s }) catch |err| {
        f.fail(.internal, std.fmt.allocPrint(f.a, "cannot start the fetch: {s}", .{@errorName(err)}) catch "cannot start the fetch");
        return .failed;
    };

    job.mu.lockUncancelable(io);
    while (!run.done and job.cause == null and !job.reschedule) job.cond.waitUncancelable(io, &job.mu);
    const finished = run.done;
    job.mu.unlock(io);
    if (finished) {
        fut.await(io);
    } else {
        // The cancellation interrupts the fetches in flight. Should the Io
        // lose it, `stop` ends the run at the next Article.
        job.stop.store(true, .release);
        fut.cancel(io);
    }

    job.mu.lockUncancelable(io);
    const res = run.result;
    const cause = job.cause;
    const restart = job.reschedule;
    job.reschedule = false;
    job.mu.unlock(io);

    if (f.fatalErr() != null) return .finished;
    if (cause != null) return .stopped;
    if (restart) return .restart;
    if (res == null) return .finished;
    f.fail(.internal, std.fmt.allocPrint(f.a, "fetch stopped: {s}", .{@errorName(res.?)}) catch "fetch stopped");
    return .failed;
}
