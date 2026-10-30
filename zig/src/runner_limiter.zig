//! The speed limit of the Usenet queue (internal/usenet/runner limiter.go) and
//! the Fetcher wrapper that applies it.
//!
//! The limiter is a token bucket without burst, shared by all connections of
//! all Downloads. Each call reserves the time its bytes take at the current
//! rate and waits for its turn, so the long-run rate equals the limit however
//! many connections call it. The bytes counted are the ones received from the
//! network (the lines of the Article bodies plus their line terminators).
//!
//! The wrapper also remembers that a Server answered a request, which is how
//! the runner tells a Server that is down or rejects the login from one that
//! simply lacks the Articles.
const std = @import("std");
const Io = std.Io;
const types = @import("usenet_types.zig");

/// Delays shorter than this are carried as debt: a very high limit does not
/// cost a timer per line.
const min_sleep_ns: i96 = 2 * std.time.ns_per_ms;

pub const Limiter = struct {
    io: Io,
    mu: Io.Mutex = .init,
    /// Bytes per second; 0 or less means unlimited.
    rate: i64 = 0,
    /// When the bytes reserved so far have been "sent" (monotonic ns).
    next_ns: i96 = 0,

    pub fn init(io: Io) Limiter {
        return .{ .io = io };
    }

    /// Applies at once, also to running fetches. Zero or less removes the
    /// limit.
    pub fn setRate(l: *Limiter, bytes_per_second: i64) void {
        l.mu.lockUncancelable(l.io);
        defer l.mu.unlock(l.io);
        l.rate = bytes_per_second;
        l.next_ns = 0;
    }

    /// Reserves `n` bytes and sleeps until it is their turn. Canceled when the
    /// calling task is cancelled.
    pub fn wait(l: *Limiter, n: usize) Io.Cancelable!void {
        l.mu.lockUncancelable(l.io);
        if (l.rate <= 0) {
            l.mu.unlock(l.io);
            return;
        }
        const now = Io.Clock.now(.awake, l.io).nanoseconds;
        if (l.next_ns < now) l.next_ns = now;
        const delay = l.next_ns - now;
        const cost: f64 = @as(f64, @floatFromInt(n)) / @as(f64, @floatFromInt(l.rate)) * std.time.ns_per_s;
        l.next_ns += @intFromFloat(@min(cost, 1e18));
        l.mu.unlock(l.io);

        if (delay < min_sleep_ns) return l.io.checkCancel();
        return l.io.sleep(.{ .nanoseconds = delay }, .awake);
    }
};

/// Counts answers and applies the speed limit around one Server's Fetcher.
/// One per (run, Server); lives as long as the run.
pub const CountingFetcher = struct {
    inner: types.Fetcher,
    limiter: *Limiter,
    /// Bumped when the Server answered a request: an Article, or "not there".
    answered: *std.atomic.Value(u64),
    /// Set when the run is being stopped: every new fetch answers "this
    /// Server is down", so the workers end at their next Article and the
    /// scheduler resolves what is left, also when the Io lost the cancellation
    /// of the run (std 0.17 can drop one that comes right after a task
    /// started). Answering Canceled instead would leave the other workers
    /// waiting for an Article nobody finishes.
    stop: *const std.atomic.Value(bool),

    pub fn fetcher(c: *CountingFetcher) types.Fetcher {
        return .{ .ctx = c, .body_fn = body };
    }

    const LimitedSink = struct {
        outer: types.Sink,
        limiter: *Limiter,
        io: Io,

        fn line(ctx: *anyopaque, bytes: []const u8) error{Abort}!void {
            const s: *LimitedSink = @ptrCast(@alignCast(ctx));
            s.limiter.wait(bytes.len + 2) catch {
                // Not swallowing the cancellation: re-arm it so the worker
                // learns that its run is over.
                s.io.recancel();
                return error.Abort;
            };
            return s.outer.line(bytes);
        }
    };

    fn body(ctx: *anyopaque, io: Io, message_id: []const u8, sink: types.Sink) types.FetchError!void {
        const c: *CountingFetcher = @ptrCast(@alignCast(ctx));
        if (c.stop.load(.acquire)) return error.ServerDisabled;
        var limited: LimitedSink = .{ .outer = sink, .limiter = c.limiter, .io = io };
        const wrapped: types.Sink = .{ .ctx = &limited, .line_fn = LimitedSink.line };
        c.inner.body(io, message_id, wrapped) catch |err| {
            if (err == error.ArticleNotFound) _ = c.answered.fetchAdd(1, .monotonic);
            return err;
        };
        _ = c.answered.fetchAdd(1, .monotonic);
    }
};

const testing = std.testing;

test "an unlimited limiter never waits and a limited one holds the rate" {
    var l: Limiter = .init(testing.io);
    const t0 = Io.Clock.now(.awake, testing.io);
    for (0..1000) |_| try l.wait(1 << 20);
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, testing.io)).toMilliseconds() < 500);

    l.setRate(100_000);
    const t1 = Io.Clock.now(.awake, testing.io);
    for (0..10) |_| try l.wait(10_000); // 100 kB at 100 kB/s: the first call is free, 0.9 s for the rest
    const ms = t1.durationTo(Io.Clock.now(.awake, testing.io)).toMilliseconds();
    try testing.expect(ms >= 800 and ms < 1500);

    l.setRate(0);
    const t2 = Io.Clock.now(.awake, testing.io);
    try l.wait(1 << 30);
    try testing.expect(t2.durationTo(Io.Clock.now(.awake, testing.io)).toMilliseconds() < 100);
}
