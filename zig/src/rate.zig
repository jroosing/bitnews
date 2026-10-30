//! Transfer rates and progress reports (internal/core/rate.go and
//! throttle.go). Times are unix milliseconds, like the rest of the daemon.
//!
//! `RateWindow` averages a rate over a sliding window the way NZBGet's speed
//! meter does, `ProgressThrottle` limits how often a Download reports.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;

/// The span transfer rates are averaged over: long enough that a quiet second
/// (a choked peer, a slow Article) does not read as a stop, short enough to
/// follow a change of speed within a few seconds.
pub const default_window_ms: i64 = 10_000;
/// The longest window: an hour of slots is 29 KiB.
pub const max_window_ms: i64 = 3_600_000;

/// The average rate over a sliding window: a ring of one-second slots, each
/// holding the bytes received in its second. `add` takes cumulative byte
/// counts at any pace (every Article of a fast Usenet download, or once a
/// second from a torrent's counters) and spreads each increase over the time
/// since the previous count, so a reading on a second boundary is not counted
/// as the next second's. The memory is one number per second of the window.
/// Not safe for concurrent use; the owner locks. The slots belong to the
/// allocator given to `init`.
pub const RateWindow = struct {
    /// Bytes received in a second, at index sec % slots.len.
    slots: []f64,
    started: bool = false,
    /// The first `add`.
    first: i64 = 0,
    /// The latest `add`.
    last_at: i64 = 0,
    last_total: i64 = 0,
    /// The unix second of the newest slot in use.
    newest: i64 = 0,

    /// A window over `span_ms`, rounded up to whole seconds, at least one and
    /// at most `max_window_ms`.
    pub fn init(gpa: Allocator, span_ms: i64) Allocator.Error!RateWindow {
        const n: usize = @intCast(std.math.clamp(@divFloor(span_ms +| 999, 1000), 1, max_window_ms / 1000));
        const slots = try gpa.alloc(f64, n);
        @memset(slots, 0);
        return .{ .slots = slots };
    }

    pub fn deinit(w: *RateWindow, gpa: Allocator) void {
        gpa.free(w.slots);
        w.* = undefined;
    }

    /// Records the cumulative byte count at `at_ms`. A count lower than the
    /// last one (a counter that started over) adds nothing.
    pub fn add(w: *RateWindow, at_ms: i64, total: i64) void {
        if (!w.started) {
            w.started = true;
            w.first = at_ms;
            w.last_at = at_ms;
            w.last_total = total;
            w.newest = second(at_ms);
            return;
        }
        const to = @max(at_ms, w.last_at);
        const delta = total -| w.last_total;
        const from = w.last_at;
        w.last_at = to;
        w.last_total = total;
        w.advance(second(to));
        if (delta > 0) w.spread(from, to, @floatFromInt(delta));
    }

    /// Bytes per second over the window ending at `now_ms`: the bytes in its
    /// slots divided by the time they cover, at least a second, so the first
    /// burst of a transfer does not read as a spike. Zero before any bytes and
    /// once nothing came for a whole window.
    pub fn rate(w: *RateWindow, now_ms: i64) i64 {
        if (!w.started) return 0;
        w.advance(second(now_ms));
        var sum: f64 = 0;
        for (w.slots) |b| sum += b;
        const n: i64 = @intCast(w.slots.len);
        const start = @max(w.first, (w.newest - n + 1) *| 1000);
        const covered = @max(now_ms -| start, 1000);
        const per_second = sum / (@as(f64, @floatFromInt(covered)) / 1000.0);
        if (!(per_second > 0)) return 0;
        if (per_second >= @as(f64, @floatFromInt(std.math.maxInt(i64)))) return std.math.maxInt(i64);
        return @intFromFloat(per_second);
    }

    fn second(ms: i64) i64 {
        return @divFloor(ms, 1000);
    }

    fn index(w: *const RateWindow, sec: i64) usize {
        return @intCast(@mod(sec, @as(i64, @intCast(w.slots.len))));
    }

    /// Shares `bytes` received between `from` and `to` among the slots of the
    /// seconds that interval covers, in proportion to the overlap. Only the
    /// seconds still in the window get their share; the rest has aged out.
    fn spread(w: *RateWindow, from: i64, to: i64, bytes: f64) void {
        const dur = to - from;
        const to_sec = second(to);
        if (dur <= 0) {
            w.slots[w.index(to_sec)] += bytes;
            return;
        }
        const n: i64 = @intCast(w.slots.len);
        var sec = @max(second(from), to_sec - n + 1);
        while (sec <= to_sec) : (sec += 1) {
            const start = @max(from, sec *| 1000);
            const end = @min(to, (sec +| 1) *| 1000);
            if (end > start) {
                w.slots[w.index(sec)] += bytes * @as(f64, @floatFromInt(end - start)) / @as(f64, @floatFromInt(dur));
            }
        }
    }

    /// Moves the ring to second `sec`, clearing the slots of the seconds that
    /// pass out of the window.
    fn advance(w: *RateWindow, sec: i64) void {
        const n: i64 = @intCast(w.slots.len);
        var s = w.newest +| 1;
        const stop = @min(sec, w.newest +| n);
        while (s <= stop) : (s += 1) w.slots[w.index(s)] = 0;
        w.newest = @max(w.newest, sec);
    }
};

/// The throttle interval the design asks for: at most one progress event per
/// second per Download.
pub const default_progress_interval_ms: i64 = 1000;

/// Limits how often progress is reported per Download. Engines call `allow` on
/// every progress update and publish a progress event only when it returns
/// true. They call `flush` when a Download stops fetching or is removed, so its
/// next report passes at once and the throttle forgets the Download. Safe for
/// concurrent use. The keys are copies owned by the throttle (allocated with
/// `gpa`); `deinit` frees them.
pub const ProgressThrottle = struct {
    gpa: Allocator,
    io: Io,
    interval_ms: i64,
    mu: Io.Mutex = .init,
    /// When each Download last passed.
    last: std.StringHashMapUnmanaged(i64) = .empty,

    pub fn init(gpa: Allocator, io: Io, interval_ms: i64) ProgressThrottle {
        return .{ .gpa = gpa, .io = io, .interval_ms = interval_ms };
    }

    pub fn deinit(t: *ProgressThrottle) void {
        var it = t.last.keyIterator();
        while (it.next()) |k| t.gpa.free(k.*);
        t.last.deinit(t.gpa);
        t.* = undefined;
    }

    /// Whether the progress of Download `id` should be published at `now_ms`.
    /// True for the first report, once the interval has passed since the last
    /// report that passed, and always for the final value: `done` equal to a
    /// known (non-zero) `total`. When memory for a new Download runs out the
    /// report passes (it is not remembered, so the next one passes too).
    pub fn allow(t: *ProgressThrottle, id: []const u8, now_ms: i64, done: i64, total: i64) bool {
        t.mu.lockUncancelable(t.io);
        defer t.mu.unlock(t.io);
        if (total > 0 and done == total) {
            t.forget(id);
            return true;
        }
        if (t.last.getPtr(id)) |last| {
            if (now_ms -| last.* < t.interval_ms) return false;
            last.* = now_ms;
            return true;
        }
        const key = t.gpa.dupe(u8, id) catch return true;
        t.last.putNoClobber(t.gpa, key, now_ms) catch {
            t.gpa.free(key);
            return true;
        };
        return true;
    }

    /// Forgets Download `id`, so its next report passes at once. It also frees
    /// the memory kept for Downloads that never reach their total.
    pub fn flush(t: *ProgressThrottle, id: []const u8) void {
        t.mu.lockUncancelable(t.io);
        defer t.mu.unlock(t.io);
        t.forget(id);
    }

    fn forget(t: *ProgressThrottle, id: []const u8) void {
        if (t.last.fetchRemove(id)) |kv| t.gpa.free(kv.key);
    }
};

const testing = std.testing;

/// 2026-01-01T00:00:00Z.
const t0: i64 = 1767225600000;

fn at(ms: i64) i64 {
    return t0 + ms;
}

test "the rate averages over the last five seconds" {
    var w: RateWindow = try .init(testing.allocator, 5000);
    defer w.deinit(testing.allocator);
    try testing.expectEqual(0, w.rate(t0));
    for (0..8) |i| w.add(at(@as(i64, @intCast(i)) * 1000), @as(i64, @intCast(i)) * 1000);
    try testing.expectEqual(1000, w.rate(at(7000)));
    try testing.expectEqual(0, w.rate(at(30_000)));
}

test "the rate follows a speed change" {
    var w: RateWindow = try .init(testing.allocator, 5000);
    defer w.deinit(testing.allocator);
    var total: i64 = 0;
    for (0..10) |i| { // 1000 B/s for 10 s
        total += 1000;
        w.add(at(@as(i64, @intCast(i)) * 1000), total);
    }
    for (10..20) |i| { // then 4000 B/s
        total += 4000;
        w.add(at(@as(i64, @intCast(i)) * 1000), total);
    }
    try testing.expectEqual(4000, w.rate(at(19_000)));
}

test "a quiet second lowers the average, it does not zero it" {
    var w: RateWindow = try .init(testing.allocator, default_window_ms);
    defer w.deinit(testing.allocator);
    const totals = [_]i64{ 0, 2000, 4000, 4000, 8000 }; // nothing in second 3
    for (totals, 0..) |total, i| w.add(at(@as(i64, @intCast(i)) * 1000), total);
    try testing.expectEqual(2000, w.rate(at(4000)));
    w.add(at(5000), 8000);
    try testing.expectEqual(1600, w.rate(at(5000)));
}

test "samples every few milliseconds keep the rate steady, never zero" {
    var w: RateWindow = try .init(testing.allocator, default_window_ms);
    defer w.deinit(testing.allocator);
    var total: i64 = 0;
    for (0..1000) |i| { // a sample every 10 ms for 10 s at 1 MB/s
        total += 10_000;
        const now = at(@as(i64, @intCast(i)) * 10);
        w.add(now, total);
        if (i > 0 and i % 100 == 0) {
            try testing.expectApproxEqAbs(1_000_000.0, @as(f64, @floatFromInt(w.rate(now))), 20_000.0);
        }
    }
}

test "a sparse reading is spread over the time it covers" {
    var w: RateWindow = try .init(testing.allocator, 4000);
    defer w.deinit(testing.allocator);
    // Half past a second.
    for (0..21) |i| w.add(at(500 + @as(i64, @intCast(i)) * 1000), @as(i64, @intCast(i)) * 3000);
    try testing.expectApproxEqAbs(3000.0, @as(f64, @floatFromInt(w.rate(at(500 + 20_000)))), 1.0);
}

test "the first burst does not read as a spike" {
    var w: RateWindow = try .init(testing.allocator, default_window_ms);
    defer w.deinit(testing.allocator);
    w.add(t0, 0);
    w.add(at(10), 1_000_000);
    try testing.expect(w.rate(at(10)) <= 1_000_000);
}

test "a gap of an hour costs no more work than the window" {
    var w: RateWindow = try .init(testing.allocator, default_window_ms);
    defer w.deinit(testing.allocator);
    w.add(t0, 0);
    w.add(at(3_600_000), 3600 * 1000);
    w.add(at(3_601_000), 3600 * 1000 + 1000);
    try testing.expectApproxEqAbs(1000.0, @as(f64, @floatFromInt(w.rate(at(3_601_000)))), 1.0);
}

test "a counter that starts over adds nothing, and a clock that steps back is ignored" {
    var w: RateWindow = try .init(testing.allocator, 5000);
    defer w.deinit(testing.allocator);
    w.add(t0, 0);
    w.add(at(1000), 1000);
    w.add(at(2000), 500); // lower than the last count
    w.add(at(1500), 900); // earlier than the last add: counted at 2000
    // 1000 bytes in the first second, 400 at the clamped time: 1400 over two seconds.
    try testing.expectEqual(700, w.rate(at(2000)));
}

test "the window is at least one second" {
    var w: RateWindow = try .init(testing.allocator, 0);
    defer w.deinit(testing.allocator);
    try testing.expectEqual(1, w.slots.len);
    var big: RateWindow = try .init(testing.allocator, 4001);
    defer big.deinit(testing.allocator);
    try testing.expectEqual(5, big.slots.len);
    var huge: RateWindow = try .init(testing.allocator, std.math.maxInt(i64));
    defer huge.deinit(testing.allocator);
    try testing.expectEqual(3600, huge.slots.len);
}

test "fuzz: any sequence of readings keeps the rate sane and never crashes" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var w: RateWindow = try .init(testing.allocator, smith.value(i64));
            defer w.deinit(testing.allocator);
            for (0..16) |_| {
                const now = smith.value(i64);
                if (smith.value(bool)) w.add(now, smith.value(i64));
                try testing.expect(w.rate(now) >= 0);
            }
        }
    }.one, .{ .corpus = &.{
        "\x08\x00\x00\x00\x00\x00\x00\x00",
        "\xff\xff\xff\xff\xff\xff\xff\x7f\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00",
    } });
}

test "fuzz: readings in a realistic range never read negative or above the bytes seen" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var w: RateWindow = try .init(testing.allocator, 5000);
            defer w.deinit(testing.allocator);
            var now: i64 = t0;
            var total: i64 = 0;
            for (0..32) |_| {
                now += smith.valueRangeAtMost(i64, 0, 20_000);
                total += smith.valueRangeAtMost(i64, 0, 1 << 20);
                w.add(now, total);
                const r = w.rate(now);
                try testing.expect(r >= 0 and r <= total);
            }
        }
    }.one, .{ .corpus = &.{
        "\x01\x02\x03\x04",
        "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff",
    } });
}

test "the throttle allows at most one report per interval per Download" {
    var th: ProgressThrottle = .init(testing.allocator, testing.io, 1000);
    defer th.deinit();
    try testing.expect(th.allow("a", t0, 10, 100));
    try testing.expect(!th.allow("a", at(999), 20, 100));
    try testing.expect(th.allow("a", at(1000), 30, 100));
    try testing.expect(!th.allow("a", at(1000), 40, 100));
}

test "the throttle tracks Downloads independently" {
    var th: ProgressThrottle = .init(testing.allocator, testing.io, 1000);
    defer th.deinit();
    try testing.expect(th.allow("a", t0, 10, 100));
    try testing.expect(th.allow("b", t0, 10, 100));
    try testing.expect(!th.allow("a", t0, 20, 100));
    try testing.expect(!th.allow("b", t0, 20, 100));
}

test "the throttle always allows the final value, and an unknown total is not final" {
    var th: ProgressThrottle = .init(testing.allocator, testing.io, 1000);
    defer th.deinit();
    try testing.expect(th.allow("a", t0, 10, 100));
    try testing.expect(th.allow("a", t0, 100, 100));
    try testing.expect(th.allow("z", t0, 0, 0));
    try testing.expect(!th.allow("z", t0, 0, 0));
}

test "flush makes the next report pass and leaves other Downloads alone" {
    var th: ProgressThrottle = .init(testing.allocator, testing.io, 1000);
    defer th.deinit();
    try testing.expect(th.allow("a", t0, 10, 100));
    try testing.expect(th.allow("b", t0, 10, 100));
    th.flush("a");
    try testing.expect(th.allow("a", t0, 20, 100));
    try testing.expect(!th.allow("a", t0, 30, 100));
    try testing.expect(!th.allow("b", t0, 20, 100));
    th.flush("never-seen");
}
