//! Dispatches the Articles of one Download to per-Server connection workers
//! and falls back along Server Priority (internal/usenet/scheduler).
//!
//! A Scheduler handles one set of Articles. It has no global state, so the
//! runner can run several at once, one per active Download. The scheduler
//! only moves Articles: it never looks at which file an Article belongs to
//! beyond its `file_key`, and a deferred (PAR2 recovery volume) file is simply
//! not handed to it until the runner asks.
//!
//! Dispatch. Every Server contributes `workers` tasks. The distinct Server
//! Priority values form levels, lowest first. An Article starts at the lowest
//! level; only workers of Servers at the Article's current level take it, in
//! (file_key, segment_number) order. Servers at the same level share load, and
//! each tries an Article at most once. When every enabled Server of the level
//! has tried the Article (absent, bad CRC, retries exhausted, retention too
//! short, or Server dead) it moves to the next level. When no level is left it
//! is `missing`, or `done` with `damaged` set if some Server delivered a copy
//! that failed its CRC check.
//!
//! Taking the next Article is O(1) amortized: the lowest level walks the
//! sorted Article list with a cursor, and Articles promoted to a higher level
//! or handed to a sibling Server go through a small per-Server min-heap.
//!
//! Errors (usenet_types.FetchError):
//!   ArticleNotFound, InvalidMessageId  the Server has tried the Article.
//!   Connection, Protocol, others       transient: the same Server is retried
//!                                      up to `max_attempts` times in all,
//!                                      waiting `Config.backoff` between
//!                                      attempts, and then counts as tried.
//!   ServerDisabled, Auth, PoolClosed   the Server is dead for the rest of the
//!                                      run: its workers exit, it counts as
//!                                      tried for every remaining Article and
//!                                      `on_server_down` fires once.
//!   A part that fails yEnc decoding counts as absent for that Server.
//!   A part that decodes but fails an announced CRC check counts as absent
//!   for that Server, but the largest such copy is kept as the best damaged
//!   copy. A part whose trailer announces no CRC is good.
//!
//! Results and back-pressure. `Config.on_result` is called once per Article
//! from the worker tasks, so it must be safe for concurrent use; the runner
//! serializes. A slow callback blocks its worker, which is the back-pressure.
//!
//! Threads and memory. `run` starts one task per worker with `Io.Group
//! .concurrent` on the `Io` it is given and returns when they have all ended.
//! Cancelling the task that runs `run` cancels the workers (a fetch in flight
//! is cancelled through the Fetcher, a waiting worker wakes up); after `run`
//! returns no task is left and nothing more is delivered. The one owner of
//! the long-lived state (the sorted Articles, per-Article flags, heaps,
//! counters, the best damaged copies) is the Scheduler, freed by `destroy`.
//! Each fetch decodes into an arena owned by the worker and reset after the
//! callback returns: the `Part` in a `Result` is valid during the callback
//! only. A damaged copy waiting for a better one lives in its own arena until
//! the Article resolves. The allocator must be safe for concurrent use.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const yenc = @import("yenc.zig");
const Part = @import("yenc_types.zig").Part;
const MinHeap = @import("scheduler_heap.zig").MinHeap;
const fetch_mod = @import("scheduler_fetch.zig");

pub const usenet_types = @import("usenet_types.zig");
pub const Fetcher = usenet_types.Fetcher;
pub const FetchError = usenet_types.FetchError;

/// How many times one Server is asked for one Article when it keeps failing
/// with transient errors.
pub const max_attempts: u32 = 3;

/// The most Servers one Scheduler supports (one bit each in the per-Article
/// tried mask).
pub const max_servers = 64;

/// Hard caps. A Server's `workers` is clamped to `max_workers_per_server`
/// (the connection limit of a Server is 100). More Articles than
/// `max_articles` is refused. Damaged copies waiting for a better one hold at
/// most `max_damaged_bytes` together: past that a new damaged copy is dropped
/// (the Article then ends up missing instead of damaged if nothing better
/// comes).
pub const max_workers_per_server: u32 = 128;
pub const max_articles: usize = 4 << 20;
pub const max_damaged_bytes: usize = 64 << 20;

/// One Usenet Server as seen by the scheduler. Strings are borrowed and must
/// outlive the Scheduler.
pub const Server = struct {
    /// Identifies the Server in Results, Stats and `on_server_down`.
    name: []const u8,
    /// The Server Priority: lower values are tried first, equal values share
    /// load.
    priority: i64 = 0,
    /// How many days back the Server keeps Articles; 0 means unlimited.
    retention_days: u32 = 0,
    /// The number of concurrent requests for this Server, normally its
    /// connection limit. 0 means 1.
    workers: u32 = 1,
    fetcher: Fetcher,
};

/// One unit of work. `message_id` is borrowed and must outlive the Scheduler
/// and every Result.
pub const Article = struct {
    /// `file_key` and `segment_number` define the dispatch order: ascending by
    /// file_key, then segment_number. The caller chooses what file_key means,
    /// for instance the file index in the NZB.
    file_key: u32,
    segment_number: u32,
    message_id: []const u8,
    /// The encoded size announced by the NZB. Carried to the Result and not
    /// interpreted.
    expected_bytes: i64 = 0,
    /// The NZB file date in unix seconds. 0 means unknown age, which never
    /// triggers a retention skip.
    date: i64 = 0,
};

pub const Status = enum {
    /// A Part was delivered. Check `Result.damaged`.
    done,
    /// No Server could supply the Article, not even a damaged copy.
    missing,
};

/// The final outcome of one Article.
pub const Result = struct {
    article: Article,
    status: Status,
    /// The decoded data: null when missing. When `damaged` it is the best copy
    /// that failed its CRC check. Valid during the `on_result` call only.
    part: ?Part = null,
    /// Set on a `done` Result when no Server delivered a copy without a CRC
    /// mismatch (a part announcing no CRC is not damaged).
    damaged: bool = false,
    /// The Server that supplied `part`; empty when missing.
    server: []const u8 = "",
    /// The last error seen for a missing Article, if any (a FetchError, a yEnc
    /// error, or error.CrcMismatch).
    err: ?anyerror = null,
};

/// Receives each Article's Result exactly once, concurrently from the worker
/// tasks. Build one with `ResultSink.init(&obj)`; `obj` has
/// `fn result(self: *T, r: Result) void`. It may block to apply back-pressure.
/// It must not swallow `error.Canceled` from an `Io` call: call `io.recancel`
/// when it saw one, or the worker does not learn that the run is cancelled.
pub const ResultSink = struct {
    ctx: *anyopaque,
    call: *const fn (ctx: *anyopaque, r: Result) void,

    pub fn init(ptr: anytype) ResultSink {
        const Ptr = @TypeOf(ptr);
        const gen = struct {
            fn call(ctx: *anyopaque, r: Result) void {
                const self: Ptr = @ptrCast(@alignCast(ctx));
                self.result(r);
            }
        };
        return .{ .ctx = ptr, .call = gen.call };
    }
};

/// Told once per Server declared dead, with the error that killed it; may run
/// concurrently with the result sink. `obj` has
/// `fn serverDown(self: *T, name: []const u8, err: FetchError) void`.
pub const ServerDownSink = struct {
    ctx: *anyopaque,
    call: *const fn (ctx: *anyopaque, name: []const u8, err: FetchError) void,

    pub fn init(ptr: anytype) ServerDownSink {
        const Ptr = @TypeOf(ptr);
        const gen = struct {
            fn call(ctx: *anyopaque, name: []const u8, err: FetchError) void {
                const self: Ptr = @ptrCast(@alignCast(ctx));
                self.serverDown(name, err);
            }
        };
        return .{ .ctx = ptr, .call = gen.call };
    }
};

/// The wait before retry number `attempt` (1 for the first retry) after a
/// transient error. `obj` has `fn backoff(self: *T, attempt: u32) Io.Duration`.
pub const Backoff = struct {
    ctx: ?*anyopaque = null,
    call: *const fn (ctx: ?*anyopaque, attempt: u32) Io.Duration,

    /// 200 ms times the attempt number.
    pub const default: Backoff = .{ .call = defaultCall };
    /// Retry at once; for tests.
    pub const none: Backoff = .{ .call = noneCall };

    fn defaultCall(_: ?*anyopaque, attempt: u32) Io.Duration {
        return .fromMilliseconds(200 * @as(i64, attempt));
    }

    fn noneCall(_: ?*anyopaque, _: u32) Io.Duration {
        return .fromMilliseconds(0);
    }

    pub fn init(ptr: anytype) Backoff {
        const Ptr = @TypeOf(ptr);
        const gen = struct {
            fn call(ctx: ?*anyopaque, attempt: u32) Io.Duration {
                const self: Ptr = @ptrCast(@alignCast(ctx.?));
                return self.backoff(attempt);
            }
        };
        return .{ .ctx = ptr, .call = gen.call };
    }
};

pub const Config = struct {
    /// Copied; at least one, at most `max_servers`.
    servers: []const Server,
    /// Copied and sorted by (file_key, segment_number). Equal keys keep their
    /// order.
    articles: []const Article,
    on_result: ?ResultSink = null,
    on_server_down: ?ServerDownSink = null,
    /// Unix seconds used to age Articles against Server retention, read once
    /// when `run` starts. Null reads the wall clock.
    now: ?i64 = null,
    backoff: Backoff = .default,
};

/// What one Server did.
pub const ServerStats = struct {
    name: []const u8,
    /// BODY requests, retries included.
    requests: u64,
    /// Encoded bytes received, including failed fetches.
    bytes: u64,
    /// The Server was declared dead.
    down: bool,
};

/// A snapshot of progress.
pub const Stats = struct {
    /// Delivered Articles; `damaged` of which have no valid copy.
    done: u64,
    missing: u64,
    damaged: u64,
    server_count: usize,
    server_buf: [max_servers]ServerStats,

    /// In the order of `Config.servers`.
    pub fn servers(st: *const Stats) []const ServerStats {
        return st.server_buf[0..st.server_count];
    }
};

pub const InitError = error{ NoServers, TooManyServers, TooManyArticles, OutOfMemory };

pub const RunError = error{
    /// The task running `run` was cancelled. Results already delivered stay
    /// delivered.
    Canceled,
    /// `run` was called a second time.
    AlreadyRun,
    /// The Io could not start the worker tasks.
    ConcurrencyUnavailable,
    /// Allocation failed while running; the Results are incomplete.
    OutOfMemory,
    /// Every worker ended with Articles unresolved (a bug guard).
    Stalled,
};

const State = enum(u8) { pending, inflight, resolved };

/// The scheduler's private state for one Article.
const Art = struct {
    /// A bit per server index that is done with this Article.
    tried: u64 = 0,
    level: u16 = 0,
    state: State = .pending,
    /// The last error seen, as an error code; 0 for none.
    err: ErrCode = 0,
};

const ErrCode = @Int(.unsigned, @bitSizeOf(anyerror));

/// The private state of one Server.
const Srv = struct {
    idx: u8,
    cfg: Server,
    bit: u64,
    level: u16,
    /// Seconds; 0 = unlimited.
    retain: i64,
    workers: u32,

    /// Signalled when this Server may have work.
    cond: Io.Condition = .init,
    /// Next index of the Article list for the walk of the Server's own level.
    cursor: u32 = 0,
    /// Promoted or handed-over Articles.
    heap: MinHeap = .{},
    /// Written under the scheduler's lock, read anywhere.
    dead: std.atomic.Value(bool) = .init(false),
    requests: std.atomic.Value(u64) = .init(0),
    bytes: std.atomic.Value(u64) = .init(0),
};

/// The best CRC-failing copy of an Article seen so far; owns its memory.
const Damaged = struct {
    arena: std.heap.ArenaAllocator,
    part: Part,
    server: u8,
};

/// A Result waiting to be delivered outside the lock, with the damaged copy
/// whose memory it points into.
const Out = struct {
    result: Result,
    held: ?Damaged = null,
};

pub const Scheduler = struct {
    gpa: Allocator,
    cfg: Config,
    /// Sorted by (file_key, segment_number); the index is the identity.
    articles: []Article,
    arts: []Art,
    servers: []Srv,
    /// Server indices grouped by level; `level_off[l]..level_off[l + 1]` is
    /// level l.
    level_servers: []u8,
    level_off: []u32,

    /// Guards everything below except the atomics.
    mu: Io.Mutex = .init,
    remaining: usize,
    started: bool = false,
    now: i64 = 0,
    damaged: std.AutoHashMapUnmanaged(u32, Damaged) = .empty,
    damaged_bytes: usize = 0,
    /// A failure that ends the run (an allocation inside the lock).
    fatal: ?anyerror = null,

    done: std.atomic.Value(u64) = .init(0),
    missing: std.atomic.Value(u64) = .init(0),
    damaged_n: std.atomic.Value(u64) = .init(0),

    /// Validates `cfg` and prepares a Scheduler. It lives on the heap so the
    /// tasks can point at it; call `run` once, then `destroy`.
    pub fn create(gpa: Allocator, cfg: Config) InitError!*Scheduler {
        if (cfg.servers.len == 0) return error.NoServers;
        if (cfg.servers.len > max_servers) return error.TooManyServers;
        if (cfg.articles.len > max_articles) return error.TooManyArticles;

        const s = try gpa.create(Scheduler);
        errdefer gpa.destroy(s);
        const articles = try gpa.dupe(Article, cfg.articles);
        errdefer gpa.free(articles);
        std.mem.sort(Article, articles, {}, lessArticle); // stable: equal keys keep their order
        const arts = try gpa.alloc(Art, articles.len);
        errdefer gpa.free(arts);
        @memset(arts, .{});
        const servers = try gpa.alloc(Srv, cfg.servers.len);
        errdefer gpa.free(servers);

        var prios: [max_servers]i64 = undefined;
        for (cfg.servers, 0..) |c, i| prios[i] = c.priority;
        const sorted = prios[0..cfg.servers.len];
        std.mem.sort(i64, sorted, {}, std.sort.asc(i64));
        var n_levels: usize = 0;
        for (sorted, 0..) |p, i| {
            if (i == 0 or p != sorted[n_levels - 1]) {
                sorted[n_levels] = p;
                n_levels += 1;
            }
        }
        const level_off = try gpa.alloc(u32, n_levels + 1);
        errdefer gpa.free(level_off);
        const level_servers = try gpa.alloc(u8, cfg.servers.len);
        errdefer gpa.free(level_servers);
        @memset(level_off, 0);

        for (cfg.servers, servers, 0..) |c, *sv, i| {
            const level = std.mem.indexOfScalar(i64, sorted[0..n_levels], c.priority).?;
            sv.* = .{
                .idx = @intCast(i),
                .cfg = c,
                .bit = @as(u64, 1) << @intCast(i),
                .level = @intCast(level),
                .retain = @as(i64, c.retention_days) * 86400,
                .workers = std.math.clamp(c.workers, 1, max_workers_per_server),
            };
            level_off[level + 1] += 1;
        }
        for (1..level_off.len) |l| level_off[l] += level_off[l - 1];
        const fill = try gpa.dupe(u32, level_off[0..n_levels]);
        defer gpa.free(fill);
        for (servers) |sv| {
            level_servers[fill[sv.level]] = sv.idx;
            fill[sv.level] += 1;
        }

        s.* = .{
            .gpa = gpa,
            .cfg = cfg,
            .articles = articles,
            .arts = arts,
            .servers = servers,
            .level_servers = level_servers,
            .level_off = level_off,
            .remaining = articles.len,
        };
        // The slices the caller owns are not kept.
        s.cfg.servers = &.{};
        s.cfg.articles = &.{};
        return s;
    }

    /// Frees everything. `run` must have returned.
    pub fn destroy(s: *Scheduler) void {
        const gpa = s.gpa;
        var it = s.damaged.valueIterator();
        while (it.next()) |d| d.arena.deinit();
        s.damaged.deinit(gpa);
        for (s.servers) |*sv| sv.heap.deinit(gpa);
        gpa.free(s.level_servers);
        gpa.free(s.level_off);
        gpa.free(s.servers);
        gpa.free(s.arts);
        gpa.free(s.articles);
        gpa.destroy(s);
    }

    /// Starts the workers and blocks until every Article is resolved, or the
    /// calling task is cancelled. All tasks have ended when it returns. May be
    /// called once.
    pub fn run(s: *Scheduler, io: Io) RunError!void {
        s.mu.lockUncancelable(io);
        if (s.started) {
            s.mu.unlock(io);
            return error.AlreadyRun;
        }
        s.started = true;
        s.now = s.cfg.now orelse Io.Clock.real.now(io).toSeconds();
        const remaining = s.remaining;
        s.mu.unlock(io);
        if (remaining == 0) return;

        var group: Io.Group = .init;
        for (s.servers) |*sv| {
            for (0..sv.workers) |_| {
                group.concurrent(io, workerMain, .{ s, sv, io }) catch {
                    group.cancel(io);
                    return error.ConcurrencyUnavailable;
                };
            }
        }
        group.await(io) catch return error.Canceled;

        s.mu.lockUncancelable(io);
        defer s.mu.unlock(io);
        if (s.fatal) |e| return @errorCast(e);
        if (s.remaining == 0) return;
        return error.Stalled;
    }

    /// A snapshot of the counters. Safe to call at any time.
    pub fn stats(s: *const Scheduler) Stats {
        var st: Stats = .{
            .done = s.done.load(.monotonic),
            .missing = s.missing.load(.monotonic),
            .damaged = s.damaged_n.load(.monotonic),
            .server_count = s.servers.len,
            .server_buf = undefined,
        };
        for (s.servers, 0..) |*sv, i| st.server_buf[i] = .{
            .name = sv.cfg.name,
            .requests = sv.requests.load(.monotonic),
            .bytes = sv.bytes.load(.monotonic),
            .down = sv.dead.load(.acquire),
        };
        return st;
    }

    // Everything below runs with `mu` held.

    fn lessArticle(_: void, a: Article, b: Article) bool {
        if (a.file_key != b.file_key) return a.file_key < b.file_key;
        return a.segment_number < b.segment_number;
    }

    fn wakeAll(s: *Scheduler, io: Io) void {
        for (s.servers) |*sv| sv.cond.broadcast(io);
    }

    fn fail(s: *Scheduler, io: Io, err: anyerror) void {
        if (s.fatal == null) s.fatal = err;
        s.wakeAll(io);
    }

    fn skipsByRetention(s: *const Scheduler, sv: *const Srv, i: u32) bool {
        if (sv.retain == 0) return false;
        const d = s.articles[i].date;
        if (d == 0) return false;
        return s.now - d > sv.retain;
    }

    /// The next Article `sv` should fetch, marked in flight. Articles resolved
    /// on the way (retention skips that exhaust the last level) are appended
    /// to `out`.
    fn next(s: *Scheduler, io: Io, sv: *Srv, out: *std.ArrayList(Out)) ?u32 {
        const n: u32 = @intCast(s.articles.len);
        while (true) {
            var i: u32 = undefined;
            if (sv.cursor < n and (sv.heap.len() == 0 or sv.cursor < sv.heap.peek())) {
                i = sv.cursor;
                sv.cursor += 1;
            } else if (sv.heap.len() > 0) {
                i = sv.heap.pop();
            } else return null;
            const a = &s.arts[i];
            if (a.state != .pending or a.level != sv.level or a.tried & sv.bit != 0) continue;
            if (s.skipsByRetention(sv, i)) {
                a.tried |= sv.bit;
                s.settle(io, i, true, out);
                continue;
            }
            a.state = .inflight;
            return i;
        }
    }

    /// Decides where a pending Article goes next: to the untried Servers of
    /// its level, to the next level, or to its final result. With `push`
    /// false it only promotes or resolves when no Server of the level can
    /// still take it (the Article is then known to be in every candidate's
    /// queue already). The Article is not in flight.
    fn settle(s: *Scheduler, io: Io, i: u32, push_in: bool, out: *std.ArrayList(Out)) void {
        const a = &s.arts[i];
        var push = push_in;
        while (true) {
            const members = s.level_servers[s.level_off[a.level]..s.level_off[a.level + 1]];
            var any = false;
            for (members) |m| {
                if (s.candidate(&s.servers[m], i)) {
                    any = true;
                    break;
                }
            }
            if (any) {
                a.state = .pending;
                if (push) {
                    for (members) |m| {
                        const sv = &s.servers[m];
                        if (!s.candidate(sv, i)) continue;
                        sv.heap.push(s.gpa, i) catch s.fail(io, error.OutOfMemory);
                        sv.cond.signal(io);
                    }
                }
                return;
            }
            if (a.level + 1 < s.level_off.len - 1) {
                a.level += 1;
                push = true; // the new level's queues have never seen it
                continue;
            }
            s.resolveMissing(io, i, out);
            return;
        }
    }

    fn candidate(s: *const Scheduler, sv: *const Srv, i: u32) bool {
        return !sv.dead.load(.monotonic) and s.arts[i].tried & sv.bit == 0 and !s.skipsByRetention(sv, i);
    }

    /// Finishes an Article no Server can supply (or hands over its best
    /// damaged copy).
    fn resolveMissing(s: *Scheduler, io: Io, i: u32, out: *std.ArrayList(Out)) void {
        const a = &s.arts[i];
        a.state = .resolved;
        var o: Out = .{ .result = .{
            .article = s.articles[i],
            .status = .missing,
            .err = if (a.err != 0) @errorFromInt(a.err) else null,
        } };
        a.err = 0;
        if (s.damaged.fetchRemove(i)) |kv| {
            const d = kv.value;
            s.damaged_bytes -= d.part.data.len;
            o.result = .{
                .article = o.result.article,
                .status = .done,
                .part = d.part,
                .damaged = true,
                .server = s.servers[d.server].cfg.name,
            };
            o.held = d;
            _ = s.done.fetchAdd(1, .monotonic);
            _ = s.damaged_n.fetchAdd(1, .monotonic);
        } else {
            _ = s.missing.fetchAdd(1, .monotonic);
        }
        out.append(s.gpa, o) catch {
            if (o.held) |*h| h.arena.deinit();
            s.fail(io, error.OutOfMemory);
        };
        s.resolved(io);
    }

    /// An Article reached its final state.
    fn resolved(s: *Scheduler, io: Io) void {
        s.remaining -= 1;
        if (s.remaining == 0) s.wakeAll(io);
    }

    /// Declares `sv` dead and re-examines the pending Articles of its level
    /// when nobody alive is left there to take them. Reports whether this call
    /// made the transition.
    fn markDead(s: *Scheduler, io: Io, sv: *Srv, out: *std.ArrayList(Out)) bool {
        if (sv.dead.load(.monotonic)) return false;
        sv.dead.store(true, .release);
        s.wakeAll(io);
        for (s.arts, 0..) |*a, i| {
            if (a.state == .pending and a.level == sv.level) s.settle(io, @intCast(i), false, out);
        }
        return true;
    }
};

/// The state of one worker task: its arena and its list of Results to deliver.
const Worker = struct {
    s: *Scheduler,
    sv: *Srv,
    io: Io,
    /// Holds the Part of the fetch in progress; reset after each Article, or
    /// handed to the damaged-copy table.
    arena: std.heap.ArenaAllocator,
    out: std.ArrayList(Out) = .empty,

    fn run(w: *Worker) void {
        const s = w.s;
        const sv = w.sv;
        const io = w.io;
        while (true) {
            // A callback that swallowed the cancellation is caught here.
            io.checkCancel() catch return;
            s.mu.lockUncancelable(io);
            if (s.fatal != null or s.remaining == 0 or sv.dead.load(.monotonic)) {
                s.mu.unlock(io);
                return;
            }
            const got = s.next(io, sv, &w.out);
            if (got == null and w.out.items.len == 0) {
                sv.cond.wait(io, &s.mu) catch {
                    s.mu.unlock(io); // the wait relocks even when cancelled
                    return;
                };
                s.mu.unlock(io);
                continue;
            }
            s.mu.unlock(io);
            w.emit();
            if (got) |i| if (!w.process(i)) return;
        }
    }

    /// Delivers the queued Results, outside the lock, and frees the damaged
    /// copies they point into.
    fn emit(w: *Worker) void {
        for (w.out.items) |*o| {
            if (w.s.cfg.on_result) |sink| sink.call(sink.ctx, o.result);
            if (o.held) |*h| h.arena.deinit();
        }
        w.out.clearRetainingCapacity();
    }

    fn resetArena(w: *Worker) void {
        // Keep one buffer for the next Article, but do not pin a huge one.
        _ = w.arena.reset(.{ .retain_with_limit = 2 << 20 });
    }

    /// Fetches Article `i`, which this worker holds in flight, and records the
    /// outcome. Returns false when the task was cancelled: the cancellation is
    /// delivered once, so the worker must end instead of taking another
    /// Article.
    fn process(w: *Worker, i: u32) bool {
        const s = w.s;
        const sv = w.sv;
        const id = s.articles[i].message_id;
        var last_err: ?anyerror = null;
        var attempt: u32 = 1;
        while (true) : (attempt += 1) {
            _ = sv.requests.fetchAdd(1, .monotonic);
            const f = fetch_mod.fetch(w.io, sv.cfg.fetcher, w.arena.allocator(), id);
            _ = sv.bytes.fetchAdd(f.wire, .monotonic);
            switch (f.outcome) {
                .cancelled => return false,
                .valid => {
                    w.finishValid(i, f.part);
                    return true;
                },
                .damaged => {
                    w.finishTried(i, f.part, f.err);
                    return true;
                },
                .dead => {
                    w.finishDead(i, f.err.?);
                    return true;
                },
                .transient => {
                    // A Fetcher that reported a connection error although it
                    // was cancelled must not make the Article look tried.
                    w.io.checkCancel() catch return false;
                    last_err = f.err;
                    if (attempt < max_attempts) {
                        if (!w.backoff(attempt)) return false;
                        w.resetArena();
                        continue;
                    }
                },
                .absent => last_err = f.err,
            }
            w.finishTried(i, null, last_err);
            return true;
        }
    }

    /// Waits before the next attempt; false when cancelled.
    fn backoff(w: *Worker, attempt: u32) bool {
        const b = w.s.cfg.backoff;
        const d = b.call(b.ctx, attempt);
        if (d.nanoseconds <= 0) {
            w.io.checkCancel() catch return false;
            return true;
        }
        w.io.sleep(d, .awake) catch return false;
        return true;
    }

    /// Resolves the Article with a good copy from this Server.
    fn finishValid(w: *Worker, i: u32, part: Part) void {
        const s = w.s;
        const io = w.io;
        s.mu.lockUncancelable(io);
        s.arts[i].state = .resolved;
        s.arts[i].err = 0;
        var old: ?Damaged = null;
        if (s.damaged.fetchRemove(i)) |kv| {
            s.damaged_bytes -= kv.value.part.data.len;
            old = kv.value;
        }
        _ = s.done.fetchAdd(1, .monotonic);
        s.resolved(io);
        s.mu.unlock(io);
        if (old) |*d| d.arena.deinit();
        if (s.cfg.on_result) |sink| sink.call(sink.ctx, .{
            .article = s.articles[i],
            .status = .done,
            .part = part,
            .server = w.sv.cfg.name,
        });
        w.resetArena();
    }

    /// Records that this Server is done with Article `i` (keeping a damaged
    /// copy if it is the best so far) and moves the Article on.
    fn finishTried(w: *Worker, i: u32, damaged: ?Part, err: ?anyerror) void {
        const s = w.s;
        const io = w.io;
        const gpa = s.gpa;
        var old: ?Damaged = null;
        var kept = false;
        s.mu.lockUncancelable(io);
        s.arts[i].tried |= w.sv.bit;
        if (damaged) |p| kept = w.keepDamaged(i, p, &old);
        if (err) |e| s.arts[i].err = @intFromError(e);
        s.settle(io, i, true, &w.out);
        s.mu.unlock(io);
        w.emit();
        if (old) |*d| d.arena.deinit();
        if (kept) {
            w.arena = .init(gpa); // the old one now holds the damaged copy
        } else {
            w.resetArena();
        }
    }

    /// Stores `p` (which lives in the worker's arena) as the best damaged copy
    /// of Article `i` when it beats the one kept and fits the cap. The caller
    /// holds the lock; a copy it replaces is returned in `old` to be freed
    /// outside the lock.
    fn keepDamaged(w: *Worker, i: u32, p: Part, old: *?Damaged) bool {
        const s = w.s;
        const new: Damaged = .{ .arena = w.arena, .part = p, .server = w.sv.idx };
        if (s.damaged.getPtr(i)) |d| {
            if (p.data.len <= d.part.data.len) return false;
            if (s.damaged_bytes - d.part.data.len + p.data.len > max_damaged_bytes) return false;
            old.* = d.*;
            s.damaged_bytes = s.damaged_bytes - d.part.data.len + p.data.len;
            d.* = new;
            return true;
        }
        if (s.damaged_bytes + p.data.len > max_damaged_bytes) return false;
        s.damaged.put(s.gpa, i, new) catch return false;
        s.damaged_bytes += p.data.len;
        return true;
    }

    /// Handles a Server that turned out to be unusable.
    fn finishDead(w: *Worker, i: u32, err: anyerror) void {
        const s = w.s;
        const io = w.io;
        s.mu.lockUncancelable(io);
        const first = s.markDead(io, w.sv, &w.out);
        s.arts[i].tried |= w.sv.bit;
        s.arts[i].err = @intFromError(err);
        s.settle(io, i, true, &w.out);
        s.mu.unlock(io);
        w.emit();
        if (first) if (s.cfg.on_server_down) |sink| sink.call(sink.ctx, w.sv.cfg.name, @errorCast(err));
        w.resetArena();
    }
};

fn workerMain(s: *Scheduler, sv: *Srv, io: Io) void {
    var w: Worker = .{ .s = s, .sv = sv, .io = io, .arena = .init(s.gpa) };
    defer {
        w.arena.deinit();
        for (w.out.items) |*o| if (o.held) |*h| h.arena.deinit();
        w.out.deinit(s.gpa);
    }
    w.run();
}

test {
    _ = @import("scheduler_heap.zig");
}
