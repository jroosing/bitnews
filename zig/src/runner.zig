//! The Usenet Queue runner (internal/usenet/runner, ADR-0008). It owns the
//! Usenet `Queue` (the policy: which Downloads run, Pause reasons, the disk
//! guard, holds, recovery) and gives it what is Usenet's own: the Servers with
//! one connection pool each, the jobs, the cooldown after a failure and the
//! speed limit. For every active Download it parses the staged NZB, runs the
//! Article scheduler over the configured Servers and writes the decoded parts
//! straight into the Download's working directory until the Download is
//! Fetched (`runner_job.zig`).
//!
//! Control loop. `start` runs two tasks. The events task reads the bus and
//! wakes the loop on every lifecycle event; on `removed` it also cancels the
//! job of that Download and ends its hold. The control task calls
//! `Queue.reconcile` on each wake-up and, as a fallback, every
//! `Config.poll_interval_ms` (10 s). Each pass lists the Queued and Fetching
//! NZB Downloads and applies `queue_select.select` with the Usenet limit: a
//! chosen Queued Download becomes Fetching and starts a job; a Fetching one
//! that is no longer chosen (a lower priority, a new limit, paused) is stopped
//! and moved back to Queued. A Download that vanishes from the repository, or
//! whose `removed` event arrives, has its job cancelled at once; its data is
//! left to whoever removes it.
//!
//! Servers come from `Config.servers` on every pass. There is one pool per
//! Server for the whole Runner, shared by all Downloads and reconfigured in
//! place when a Server changes (`runner_pools.zig`). Running fetches pick up a
//! changed Server set without losing work: the scheduler is restarted for the
//! Articles that are not resolved yet. Publish `servers_changed` (or call
//! `serversChanged`) after the Servers change.
//!
//! What a fetch does. All files except PAR2 recovery volumes
//! (`name.volNN+MM.par2`) are fetched; the volumes wait until Repair needs
//! them (`fetchDeferred`, a seam for post-processing). The small index `.par2`
//! is fetched like any data file. Articles no Server has are counted, not
//! failed: a Download with missing or damaged Articles still reaches Fetched
//! and the `FetchDetail` (`fetchDetailOf`) tells post-processing what is
//! lacking. A Download with no fetchable Article, or an NZB that does not
//! parse, is Failed with `source_invalid`. When no Server answered a single
//! request (wrong password, provider down) the Download is not marked Fetched
//! with everything missing: it goes back to Queued and waits
//! `Config.failure_backoff_ms` (30 s).
//!
//! bitnews never opens a PAR2 file: `par2` verifies and repairs it (Z4). The
//! only PAR2 logic here is `runner_volumes.zig`, which picks which volumes to
//! fetch from the NZB file names and a block count it is given.
//!
//! Hand-off. When a Download became Fetched the runner publishes the `fetched`
//! event and calls `Config.on_fetched`. Z3 has no post-processing, so the
//! Download simply stays Fetched; Z4 sets the hook.
//!
//! Resume. A fetch keeps a resume blob (`runner_blob.zig`: the file writer's
//! snapshot plus the Articles already resolved). It is saved every
//! `Config.save_interval_ms`, when a Download is paused or pre-empted and on
//! shutdown, and loaded the next time the Download starts, so finished
//! Articles are not fetched again. Without a store (`Config.progress` null) it
//! is kept in memory only. Without a usable blob the fetch starts over; to keep
//! the writer from creating duplicates such as `movie.mkv.1` it first deletes
//! every file directly inside the working directory except the stored `.nzb`.
//! Call `recover` once before the first pass after a restart: it moves the
//! Downloads found Fetching back to Queued.
//!
//! Threads and ownership. `create` returns a heap Runner; `start` begins the
//! tasks, `stop` ends them and every job (their resume state is saved, the
//! Downloads stay Fetching), `destroy` frees everything. All methods may be
//! called from any task. One mutex guards the job and cooldown maps and is never
//! held across a database call. Each job owns an arena freed when it ends
//! (`runner_job.zig`).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const event = @import("event.zig");
const events_bus = @import("events_bus.zig");
const log = @import("log.zig");
const nntp = @import("nntp.zig");
const nzb = @import("nzb.zig");
const queue = @import("queue.zig");
const remover = @import("remover.zig");
const sched = @import("scheduler.zig");
const job_mod = @import("runner_job.zig");
const limiter_mod = @import("runner_limiter.zig");
const pools_mod = @import("runner_pools.zig");
const seams = @import("runner_seams.zig");
const store_mod = @import("runner_store.zig");
const volumes_mod = @import("runner_volumes.zig");

pub const Job = job_mod.Job;
pub const Cause = job_mod.Cause;
pub const FetchDetail = seams.FetchDetail;
pub const FetchReport = seams.FetchReport;
pub const ServerSpec = seams.ServerSpec;
pub const default_poll_interval_ms: u32 = 10_000;
pub const default_save_interval_ms: i64 = 10_000;
pub const default_failure_backoff_ms = job_mod.default_failure_backoff_ms;
/// How often the "no enabled server" warning is logged.
const noservers_log_interval_ms: i64 = 60_000;

pub const Config = struct {
    /// The bus the runner listens on and publishes to.
    bus: *events_bus.Bus,
    downloads: queue.seams.Downloads,
    /// The Usenet Queue's limit, incomplete root and minimum free space.
    policy: queue.seams.PolicySource,
    servers: seams.ServerSource,
    sources: seams.NzbSource,
    /// Opens the secure transports of the NNTP pools (one TlsDialer per
    /// daemon); tests pass a plain one.
    dialer: nntp.Dialer,
    /// Null keeps the resume state in memory only.
    progress: ?seams.ProgressStore = null,
    /// Null keeps the un-deferred volumes in memory only.
    volumes: ?seams.VolumeStore = null,
    /// Null keeps the fetch reports in memory only.
    reports: ?seams.ReportStore = null,
    /// Null turns the disk guard off.
    free_space: ?queue.seams.FreeSpace = queue.seams.FreeSpace.system,
    clock: queue.seams.Clock = .{},
    /// Null logs nothing.
    log: ?log.Logger = null,
    /// The scheduler's retry backoff.
    backoff: sched.Backoff = .default,
    /// Where post-processing starts; see `seams.FetchedHook`.
    on_fetched: ?seams.FetchedHook = null,
    /// The speed limit in bytes per second that applies now (zero: none),
    /// read at start and on every `settings_changed`. Null leaves the limit
    /// to `setSpeedLimit`.
    speed: ?SpeedSource = null,
    /// Recovery blocks fetched beyond what Repair needs. Zero means
    /// `volumes_mod.default_margin`; a negative value means none.
    volume_margin: i64 = 0,

    /// The control loop's fallback poll.
    poll_interval_ms: u32 = default_poll_interval_ms,
    /// How often resume state is saved.
    save_interval_ms: i64 = default_save_interval_ms,
    /// How often Download.done_bytes is written (zero: the Queue's default).
    done_bytes_interval_ms: i64 = 0,
    /// How often a running Download rechecks free space (zero: the Queue's
    /// default).
    disk_check_interval_ms: i64 = 0,
    /// The progress event throttle (zero: the Queue's default).
    progress_interval_ms: i64 = 0,
    /// How long a Download waits in Queued after every Server turned out to be
    /// unusable.
    failure_backoff_ms: i64 = default_failure_backoff_ms,
};

pub const SpeedSource = struct {
    ctx: *anyopaque,
    read_fn: *const fn (ctx: *anyopaque) i64,
};

pub const Runner = struct {
    gpa: Allocator,
    io: Io,
    cfg: Config,
    q: queue.Queue,
    pools: pools_mod.PoolSet,
    limiter: limiter_mod.Limiter,
    /// The state kept when the Config gives no store.
    mem: store_mod.Mem,
    progress: seams.ProgressStore,
    volumes: seams.VolumeStore,
    reports: seams.ReportStore,
    adapter: Adapter,

    /// Guards `jobs` and `cooldown`; also the condition `ended_cond` waits on.
    mu: Io.Mutex = .init,
    ended_cond: Io.Condition = .init,
    /// Download ID -> job; the key is the job's own `id`.
    jobs: std.StringHashMapUnmanaged(*Job) = .empty,
    /// Download ID (gpa) -> when it may be activated again, unix ms.
    cooldown: std.StringHashMapUnmanaged(i64) = .empty,
    no_servers_logged_ms: i64 = 0,

    /// Guards the wake-up state and the subscription.
    wake_mu: Io.Mutex = .init,
    wake_cond: Io.Condition = .init,
    pending: bool = false,
    stopping: bool = false,
    sub: ?*events_bus.Subscription = null,
    started: bool = false,
    tasks: Io.Group = .init,

    /// Returns a Runner (and its Queue) that does nothing until `start`. Free
    /// it with `destroy`.
    pub fn create(gpa: Allocator, io: Io, cfg: Config) Allocator.Error!*Runner {
        const r = try gpa.create(Runner);
        r.* = .{
            .gpa = gpa,
            .io = io,
            .cfg = cfg,
            .q = undefined,
            .pools = .init(gpa, io, cfg.dialer, cfg.log),
            .limiter = .init(io),
            .mem = .init(gpa, io),
            .progress = undefined,
            .volumes = undefined,
            .reports = undefined,
            .adapter = .{ .r = r },
        };
        r.progress = cfg.progress orelse r.mem.progress();
        r.volumes = cfg.volumes orelse r.mem.volumes();
        r.reports = cfg.reports orelse r.mem.reports();
        r.q = queue.Queue.init(gpa, io, .{
            .source = .nzb,
            .downloads = cfg.downloads,
            .events = cfg.bus.publisher(),
            .policy = cfg.policy,
            .protocol = queue.seams.Protocol.init(&r.adapter),
            .free_space = cfg.free_space,
            .clock = cfg.clock,
            .wake = .{ .ctx = r, .wake_fn = wakeFn },
            .log = cfg.log,
            .progress_interval_ms = cfg.progress_interval_ms,
            .done_bytes_interval_ms = cfg.done_bytes_interval_ms,
            .disk_check_interval_ms = cfg.disk_check_interval_ms,
        });
        return r;
    }

    /// Stops the runner if it runs and frees everything.
    pub fn destroy(r: *Runner) void {
        r.stop();
        r.pools.deinit();
        r.q.deinit();
        r.mem.deinit();
        var c = r.cooldown.keyIterator();
        while (c.next()) |k| r.gpa.free(k.*);
        r.cooldown.deinit(r.gpa);
        r.jobs.deinit(r.gpa);
        r.gpa.destroy(r);
    }

    /// The Usenet Queue: its Pause reasons, holds and recovery. The pointer is
    /// valid until `destroy`.
    pub fn queueOf(r: *Runner) *queue.Queue {
        return &r.q;
    }

    /// Moves the Downloads found Fetching back to Queued. Call it once after a
    /// restart, before `start`.
    pub fn recover(r: *Runner) anyerror!void {
        return r.q.recover();
    }

    /// The runner as the data remover's `FetchHolder`: `hold` stops the fetch
    /// of a Download and keeps it stopped, `release` undoes it (`Queue.hold`).
    pub fn fetchHolder(r: *Runner) remover.FetchHolder {
        return .{ .ctx = r, .hold_fn = holdFn, .release_fn = releaseFn };
    }

    fn holdFn(ctx: ?*anyopaque, id: []const u8) anyerror!void {
        const r: *Runner = @ptrCast(@alignCast(ctx.?));
        return r.q.hold(id);
    }

    fn releaseFn(ctx: ?*anyopaque, id: []const u8) void {
        const r: *Runner = @ptrCast(@alignCast(ctx.?));
        _ = r.q.release(id);
    }

    /// Limits the bytes received from the Servers, all Downloads together, to
    /// `bytes_per_second`. Zero or less removes the limit. It applies at once,
    /// also to running fetches.
    pub fn setSpeedLimit(r: *Runner, bytes_per_second: i64) void {
        r.limiter.setRate(bytes_per_second);
    }

    fn syncSpeed(r: *Runner) void {
        if (r.cfg.speed) |src| r.setSpeedLimit(src.read_fn(src.ctx));
    }

    /// Asks for a control pass soon: the Servers, the settings or the
    /// Downloads changed.
    pub fn poke(r: *Runner) void {
        r.wake_mu.lockUncancelable(r.io);
        r.pending = true;
        r.wake_cond.signal(r.io);
        r.wake_mu.unlock(r.io);
    }

    /// The Servers changed: the next pass reads them again (the same as the
    /// `servers_changed` event, which the runner also listens to).
    pub fn serversChanged(r: *Runner) void {
        r.poke();
    }

    fn wakeFn(ctx: ?*anyopaque) void {
        const r: *Runner = @ptrCast(@alignCast(ctx.?));
        r.poke();
    }

    // ---- lifecycle ----

    /// Starts the control loop and the event reader. The runner subscribes to
    /// the bus before it returns, so no event after that is missed.
    pub fn start(r: *Runner) !void {
        if (r.started) return;
        r.started = true;
        errdefer r.stop();
        r.syncSpeed();
        const sub = try r.cfg.bus.subscribe();
        r.wake_mu.lockUncancelable(r.io);
        r.stopping = false;
        r.sub = sub;
        r.wake_mu.unlock(r.io);
        try r.tasks.concurrent(r.io, eventsMain, .{r});
        try r.tasks.concurrent(r.io, controlMain, .{r});
    }

    /// Ends the control loop and every fetch. Fetches still running stay
    /// Fetching (call `recover` after a restart); their resume state is saved.
    /// Returns once every fetch has stopped.
    pub fn stop(r: *Runner) void {
        if (!r.started) return;
        r.wake_mu.lockUncancelable(r.io);
        r.stopping = true;
        if (r.sub) |s| s.close();
        r.wake_cond.broadcast(r.io);
        r.wake_mu.unlock(r.io);
        r.tasks.cancel(r.io);
        r.shutdownJobs();
        r.started = false;
    }

    fn shutdownJobs(r: *Runner) void {
        r.mu.lockUncancelable(r.io);
        var it = r.jobs.valueIterator();
        while (it.next()) |j| j.*.cancel(.shutdown);
        while (r.anyRunningLocked()) r.ended_cond.waitUncancelable(r.io, &r.mu);
        r.mu.unlock(r.io);
        r.reapEnded();
    }

    fn anyRunningLocked(r: *Runner) bool {
        var it = r.jobs.valueIterator();
        while (it.next()) |j| if (!j.*.ended) return true;
        return false;
    }

    // ---- the tasks ----

    fn controlMain(r: *Runner) void {
        const io = r.io;
        r.q.reconcile();
        while (true) {
            r.wake_mu.lockUncancelable(io);
            const timeout: Io.Timeout = .{ .duration = .{ .raw = .fromMilliseconds(r.cfg.poll_interval_ms), .clock = .awake } };
            while (!r.pending and !r.stopping) {
                r.wake_cond.waitTimeout(io, &r.wake_mu, timeout) catch |err| switch (err) {
                    error.Timeout => break,
                    error.Canceled => {
                        r.wake_mu.unlock(io);
                        return;
                    },
                };
            }
            if (r.stopping) {
                r.wake_mu.unlock(io);
                return;
            }
            r.pending = false;
            r.wake_mu.unlock(io);
            r.q.reconcile();
        }
    }

    fn eventsMain(r: *Runner) void {
        const io = r.io;
        var sub = blk: {
            r.wake_mu.lockUncancelable(io);
            defer r.wake_mu.unlock(io);
            break :blk r.sub orelse return;
        };
        while (true) {
            const item = sub.next(null) catch |err| switch (err) {
                error.Timeout => continue,
                error.Canceled => break,
                error.Closed => {
                    // Closed by stop, or dropped for being slow: then events
                    // were missed and the next pass must look at everything.
                    if (sub.reason() == null) break;
                    sub.deinit();
                    const fresh = r.cfg.bus.subscribe() catch {
                        r.wake_mu.lockUncancelable(io);
                        r.sub = null;
                        r.wake_mu.unlock(io);
                        return;
                    };
                    r.wake_mu.lockUncancelable(io);
                    const stopping = r.stopping;
                    r.sub = fresh;
                    r.wake_mu.unlock(io);
                    sub = fresh;
                    if (stopping) fresh.close();
                    r.poke();
                    continue;
                },
            };
            defer item.release();
            switch (item.event()) {
                .download_progress => continue,
                .settings_changed => r.syncSpeed(),
                .removed => |id| {
                    r.cancelJob(id, .removed);
                    _ = r.q.release(id);
                },
                else => {},
            }
            r.poke();
        }
        r.wake_mu.lockUncancelable(io);
        r.sub = null;
        r.wake_mu.unlock(io);
        sub.deinit();
    }

    // ---- what jobs and the pass use ----

    pub fn nowMs(r: *Runner) i64 {
        return r.cfg.clock.nowMs(r.io);
    }

    pub fn publish(r: *Runner, e: event.Event) void {
        r.cfg.bus.publish(e);
    }

    pub fn jobEnded(r: *Runner, job: *Job) void {
        r.mu.lockUncancelable(r.io);
        job.ended = true;
        r.ended_cond.broadcast(r.io);
        r.mu.unlock(r.io);
        r.poke();
    }

    /// Cancels the job of the Download, if any.
    pub fn cancelJob(r: *Runner, id: []const u8, why: Cause) void {
        r.mu.lockUncancelable(r.io);
        defer r.mu.unlock(r.io);
        if (r.jobs.get(id)) |j| j.cancel(why);
    }

    pub fn setCooldown(r: *Runner, id: []const u8, until_ms: i64) void {
        r.mu.lockUncancelable(r.io);
        defer r.mu.unlock(r.io);
        if (r.cooldown.getPtr(id)) |p| {
            p.* = until_ms;
            return;
        }
        const key = r.gpa.dupe(u8, id) catch return;
        r.cooldown.putNoClobber(r.gpa, key, until_ms) catch r.gpa.free(key);
    }

    fn coolingDown(r: *Runner, id: []const u8) bool {
        const now = r.nowMs();
        r.mu.lockUncancelable(r.io);
        defer r.mu.unlock(r.io);
        const until = r.cooldown.get(id) orelse return false;
        return now < until;
    }

    /// Writes the Download's done bytes, and its total bytes when they are
    /// still unknown (zero), to the repository. It uses `updateProgress`,
    /// which writes only those columns: a whole-row update here could
    /// overwrite a concurrent edit of the priority, category or paused flag.
    pub fn storeDoneBytes(r: *Runner, a: Allocator, id: []const u8, done: i64, total: i64) void {
        const cur = r.q.cfg.downloads.get(a, id) catch return;
        const t = if (cur.total_bytes != 0) 0 else total;
        r.q.cfg.downloads.updateProgress(id, done, t) catch |err| {
            if (err != error.NotFound and err != error.Canceled) {
                if (r.cfg.log) |l| l.warn("usenet runner: cannot store done bytes", &.{ log.Attr.string("id", id), log.Attr.string("err", @errorName(err)) });
            }
        };
    }

    /// Frees the jobs whose task has ended.
    fn reapEnded(r: *Runner) void {
        var done: std.ArrayList(*Job) = .empty;
        defer done.deinit(r.gpa);
        r.mu.lockUncancelable(r.io);
        var it = r.jobs.valueIterator();
        while (it.next()) |j| if (j.*.ended) done.append(r.gpa, j.*) catch break;
        for (done.items) |j| _ = r.jobs.remove(j.id);
        r.mu.unlock(r.io);
        // The task has set `ended` as its last act of substance; awaiting it
        // returns as soon as the task function has returned.
        const prev = r.io.swapCancelProtection(.blocked);
        defer _ = r.io.swapCancelProtection(prev);
        for (done.items) |j| {
            j.group.await(r.io) catch {};
            j.destroy();
        }
    }

    // ---- the Protocol the Queue drives (queue.seams.Protocol) ----

    /// The Protocol of this runner for its Queue; already wired by `create`.
    pub fn protocol(r: *Runner) queue.seams.Protocol {
        return queue.seams.Protocol.init(&r.adapter);
    }

    /// The method names of `queue.seams.Protocol` (start and stop) are the
    /// runner's lifecycle names too, so the Queue gets this adapter.
    const Adapter = struct {
        r: *Runner,

        pub fn prepare(a: *Adapter) bool {
            return a.r.prepare();
        }

        pub fn exclude(a: *Adapter, id: []const u8) bool {
            return a.r.exclude(id);
        }

        pub fn running(a: *Adapter, id: []const u8) bool {
            return a.r.running(id);
        }

        pub fn start(a: *Adapter, d: dl.Download) anyerror!void {
            return a.r.startJob(d);
        }

        /// Cancels the job; its resume state is kept.
        pub fn stop(a: *Adapter, id: []const u8) void {
            a.r.cancelJob(id, .deactivated);
        }

        pub fn abandon(a: *Adapter, id: []const u8) anyerror!void {
            return a.r.abandon(id);
        }
    };

    /// Syncs the Servers, cancels the jobs whose Download left the Queue,
    /// expires cooldowns, and reports whether any Server can take a fetch.
    pub fn prepare(r: *Runner) bool {
        r.syncServers();
        r.reapEnded();
        r.dropStaleJobs();
        const now = r.nowMs();
        {
            r.mu.lockUncancelable(r.io);
            defer r.mu.unlock(r.io);
            var expired: std.ArrayList([]const u8) = .empty;
            defer expired.deinit(r.gpa);
            var it = r.cooldown.iterator();
            while (it.next()) |e| {
                if (now >= e.value_ptr.*) expired.append(r.gpa, e.key_ptr.*) catch break;
            }
            for (expired.items) |k| if (r.cooldown.fetchRemove(k)) |kv| r.gpa.free(kv.key);
        }
        if (r.pools.anyActive()) return true;
        if (r.anyQueued()) r.logNoServers(now);
        return false;
    }

    /// Keeps a Download that cools down after a failure out of the pass, so it
    /// stays Queued until its cooldown ends.
    pub fn exclude(r: *Runner, id: []const u8) bool {
        return r.coolingDown(id);
    }

    /// Whether a job of the Download runs.
    pub fn running(r: *Runner, id: []const u8) bool {
        r.mu.lockUncancelable(r.io);
        defer r.mu.unlock(r.io);
        const j = r.jobs.get(id) orelse return false;
        return !j.ended;
    }

    /// Starts the fetch of `d`, which the Queue has moved to Fetching.
    /// `error.NotNow` while the Download cools down, is held, or its previous
    /// job has not ended yet.
    fn startJob(r: *Runner, d: dl.Download) anyerror!void {
        if (r.coolingDown(d.id)) return error.NotNow;
        r.reapEnded();
        const job = try Job.create(r, d);
        errdefer job.destroy();
        {
            r.mu.lockUncancelable(r.io);
            defer r.mu.unlock(r.io);
            // Checked under `mu`, which `running` takes too: a hold either
            // sees this job or is seen here.
            if (r.q.held(d.id) or r.jobs.contains(d.id)) return error.NotNow;
            try r.jobs.putNoClobber(r.gpa, job.id, job);
        }
        job.group.concurrent(r.io, job_mod.jobMain, .{job}) catch |err| {
            r.mu.lockUncancelable(r.io);
            _ = r.jobs.remove(job.id);
            r.mu.unlock(r.io);
            return err;
        };
    }

    /// Cancels the job as if the Download were removed (its resume state goes)
    /// and waits until it has stopped; cancel the calling task to bound the
    /// wait.
    pub fn abandon(r: *Runner, id: []const u8) anyerror!void {
        r.mu.lockUncancelable(r.io);
        defer r.mu.unlock(r.io);
        const j = r.jobs.get(id) orelse return;
        j.cancel(.removed);
        while (true) {
            const cur = r.jobs.get(id);
            if (cur == null or cur.? != j or cur.?.ended) return;
            try r.ended_cond.wait(r.io, &r.mu);
        }
    }

    /// Cancels, as removed, the jobs whose Download is gone, is no longer
    /// Queued or Fetching, or is held.
    fn dropStaleJobs(r: *Runner) void {
        var arena: std.heap.ArenaAllocator = .init(r.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        var ids: std.ArrayList([]const u8) = .empty;
        {
            r.mu.lockUncancelable(r.io);
            defer r.mu.unlock(r.io);
            var it = r.jobs.keyIterator();
            while (it.next()) |k| {
                const copy = a.dupe(u8, k.*) catch return;
                ids.append(a, copy) catch return;
            }
        }
        for (ids.items) |id| {
            const d = r.q.cfg.downloads.get(a, id) catch |err| {
                if (err == error.NotFound) r.cancelJob(id, .removed);
                continue; // cannot tell otherwise: leave it for the next pass
            };
            if ((d.state == .queued or d.state == .fetching) and !r.q.held(id)) continue;
            r.cancelJob(id, .removed);
        }
    }

    fn anyQueued(r: *Runner) bool {
        var arena: std.heap.ArenaAllocator = .init(r.gpa);
        defer arena.deinit();
        for ([_]dl.State{ .queued, .fetching }) |st| {
            const page = r.q.cfg.downloads.list(arena.allocator(), st, .nzb) catch continue;
            if (page.len > 0) return true;
        }
        return false;
    }

    fn logNoServers(r: *Runner, now: i64) void {
        if (now - r.no_servers_logged_ms < noservers_log_interval_ms and r.no_servers_logged_ms != 0) return;
        r.no_servers_logged_ms = now;
        if (r.cfg.log) |l| l.warn("usenet runner: no enabled server is configured, downloads stay queued", &.{});
    }

    /// Reads the Servers and updates the pools and, if the Server set changed,
    /// the running fetches.
    fn syncServers(r: *Runner) void {
        var arena: std.heap.ArenaAllocator = .init(r.gpa);
        defer arena.deinit();
        const specs = r.cfg.servers.list(arena.allocator()) catch |err| {
            if (err != error.Canceled) {
                if (r.cfg.log) |l| l.err("usenet runner: cannot read servers", &.{log.Attr.string("err", @errorName(err))});
            }
            return;
        };
        if (!r.pools.sync(specs)) return;
        r.mu.lockUncancelable(r.io);
        defer r.mu.unlock(r.io);
        var it = r.jobs.valueIterator();
        while (it.next()) |j| j.*.restartRun();
    }

    // ---- fetch reports ----

    /// What the finished fetch of the Download knew about its Articles; null
    /// when it has none (not fetched yet, or fetched by a daemon that kept no
    /// report).
    pub fn fetchDetailOf(r: *Runner, id: []const u8) anyerror!?FetchDetail {
        return r.reports.load(id);
    }

    /// The part of the detail post-processing looks at.
    pub fn fetchReport(r: *Runner, id: []const u8) anyerror!?FetchReport {
        const d = (try r.fetchDetailOf(id)) orelse return null;
        return d.report();
    }

    // ---- deferred recovery volumes ----

    const Deferred = struct {
        /// NZB file indexes of the volumes not fetched yet, in file order.
        idx: []const u32,
        n: nzb.Nzb,
    };

    /// The recovery volumes of the Download that are not fetched yet (deferred,
    /// or un-deferred and pending), in file order. Volumes without Articles are
    /// not offered.
    fn deferredFiles(r: *Runner, a: Allocator, id: []const u8) anyerror!Deferred {
        const loaded = try job_mod.loadNzb(r, a, id, null);
        const n = switch (loaded) {
            .ok => |parsed| parsed,
            .failed => return error.InvalidSource,
        };
        const rows = try r.volumes.list(a, id);
        var idx: std.ArrayList(u32) = .empty;
        for (n.files, 0..) |f, i| {
            if (!f.deferred() or f.segments.len == 0) continue;
            var fetched = false;
            for (rows) |row| if (row.file_index == i and row.fetched) {
                fetched = true;
            };
            if (!fetched) try idx.append(a, @intCast(i));
        }
        return .{ .idx = idx.items, .n = n };
    }

    /// The recovery volumes (PAR2 files named `name.volNN+MM.par2`) of the
    /// Download that have not been fetched, in NZB order. The indexes `undefer`
    /// takes are indexes into this list. It parses the stored NZB. An unknown
    /// Download is `error.NotFound`. Allocated in `a`.
    pub fn listDeferred(r: *Runner, a: Allocator, id: []const u8) anyerror![]const nzb.File {
        _ = try r.q.cfg.downloads.get(a, id);
        const def = try r.deferredFiles(a, id);
        const out = try a.alloc(nzb.File, def.idx.len);
        for (out, def.idx) |*f, i| f.* = def.n.files[i];
        return out;
    }

    /// Records that the volumes at `indexes` (indexes into the list
    /// `listDeferred` returns) are wanted. The Download must be in
    /// PostProcessing (`error.IllegalTransition` otherwise); moving it back to
    /// Fetching is the caller's job (see `fetchDeferred`). The next fetch of
    /// the Download then fetches only these volumes: its other Articles were
    /// resolved already. The choice is persisted. Undefer of a volume that is
    /// wanted already changes nothing.
    pub fn undefer(r: *Runner, id: []const u8, indexes: []const u32) anyerror!void {
        var arena: std.heap.ArenaAllocator = .init(r.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const d = try r.q.cfg.downloads.get(a, id);
        if (d.state != .post_processing) return error.IllegalTransition;
        const def = try r.deferredFiles(a, id);
        const chosen = try a.alloc(u32, indexes.len);
        for (chosen, indexes) |*c, i| {
            if (i >= def.idx.len) return error.IndexOutOfRange;
            c.* = def.idx[i];
        }
        std.mem.sort(u32, chosen, {}, std.sort.asc(u32));
        var n: usize = 0;
        for (chosen) |c| {
            if (n > 0 and chosen[n - 1] == c) continue;
            chosen[n] = c;
            n += 1;
        }
        try r.volumes.want(id, chosen[0..n]);
    }

    /// Called by post-processing when Repair needs `min_blocks` more recovery
    /// blocks than are on disk (the number comes from `par2`'s own report).
    /// Un-defers the cheapest volumes that cover `min_blocks` plus the margin
    /// and moves the Download from PostProcessing back to Fetching, so the next
    /// pass fetches just those volumes. Returns false, and changes nothing,
    /// when all deferred volumes together cannot cover `min_blocks`.
    pub fn fetchDeferred(r: *Runner, id: []const u8, min_blocks: i64) anyerror!bool {
        var arena: std.heap.ArenaAllocator = .init(r.gpa);
        defer arena.deinit();
        return volumes_mod.fetchDeferred(arena.allocator(), .{
            .ctx = r,
            .list_fn = stepList,
            .undefer_fn = stepUndefer,
            .requeue_fn = stepRequeue,
        }, id, min_blocks, r.cfg.volume_margin);
    }

    fn stepList(ctx: *anyopaque, a: Allocator, id: []const u8) anyerror![]const nzb.File {
        const r: *Runner = @ptrCast(@alignCast(ctx));
        return r.listDeferred(a, id);
    }

    fn stepUndefer(ctx: *anyopaque, id: []const u8, indexes: []const u32) anyerror!void {
        const r: *Runner = @ptrCast(@alignCast(ctx));
        return r.undefer(id, indexes);
    }

    fn stepRequeue(ctx: *anyopaque, id: []const u8) anyerror!void {
        const r: *Runner = @ptrCast(@alignCast(ctx));
        var arena: std.heap.ArenaAllocator = .init(r.gpa);
        defer arena.deinit();
        _ = try r.q.setState(arena.allocator(), id, .{ .to = .fetching, .at = r.nowMs() });
    }
};
