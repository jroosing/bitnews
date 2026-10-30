//! Byte-range bookkeeping of one file (internal/usenet/filewriter ranges.go).
//!
//! Ranges are half-open and zero-based: `Range{ .start, .end }` covers the
//! offsets start <= i < end. A `RangeSet` is a sorted list of non-empty,
//! disjoint and non-adjacent ranges. A `Tracker` holds the three sets of one
//! file: `done` (written from a good copy), `damaged` (written from a copy
//! whose CRC32 did not match) and `lost` (declared permanently unavailable).
//! They never overlap each other.
//!
//! Memory: a set owns its list (the allocator is passed to each call, the
//! writer's `gpa`). Every mutating call first makes room for the worst case
//! and only then changes anything, so an `error.OutOfMemory` leaves the set
//! as it was. The number of ranges is bounded by the number of distinct
//! parts written, which the NZB bounds.
const std = @import("std");
const Allocator = std.mem.Allocator;

pub const Range = struct {
    start: i64,
    end: i64,

    pub fn len(r: Range) i64 {
        return r.end - r.start;
    }

    pub fn eql(a: Range, b: Range) bool {
        return a.start == b.start and a.end == b.end;
    }
};

/// The total number of bytes covered by `rs`.
pub fn sum(rs: []const Range) i64 {
    var n: i64 = 0;
    for (rs) |r| n += r.len();
    return n;
}

pub const RangeSet = struct {
    items: std.ArrayList(Range) = .empty,

    pub fn deinit(s: *RangeSet, a: Allocator) void {
        s.items.deinit(a);
        s.* = .{};
    }

    pub fn slice(s: RangeSet) []const Range {
        return s.items.items;
    }

    /// Room for `n` more ranges, so that `n` calls of `add` or `remove`
    /// (each grows the set by at most one range) cannot fail.
    pub fn reserve(s: *RangeSet, a: Allocator, n: usize) Allocator.Error!void {
        try s.items.ensureUnusedCapacity(a, n);
    }

    /// The index of the first range whose end is >= `pos`.
    fn firstEndAtLeast(rs: []const Range, pos: i64) usize {
        var lo: usize = 0;
        var hi: usize = rs.len;
        while (lo < hi) {
            const mid = lo + (hi - lo) / 2;
            if (rs[mid].end >= pos) hi = mid else lo = mid + 1;
        }
        return lo;
    }

    /// The index of the first range whose end is > `pos`.
    fn firstEndAbove(rs: []const Range, pos: i64) usize {
        var lo: usize = 0;
        var hi: usize = rs.len;
        while (lo < hi) {
            const mid = lo + (hi - lo) / 2;
            if (rs[mid].end > pos) hi = mid else lo = mid + 1;
        }
        return lo;
    }

    /// The index of the first range that starts at or after `pos`.
    fn firstStartAtLeast(rs: []const Range, pos: i64) usize {
        var lo: usize = 0;
        var hi: usize = rs.len;
        while (lo < hi) {
            const mid = lo + (hi - lo) / 2;
            if (rs[mid].start >= pos) hi = mid else lo = mid + 1;
        }
        return lo;
    }

    /// The index of the first range that starts after `pos`.
    fn firstStartAbove(rs: []const Range, pos: i64) usize {
        var lo: usize = 0;
        var hi: usize = rs.len;
        while (lo < hi) {
            const mid = lo + (hi - lo) / 2;
            if (rs[mid].start > pos) hi = mid else lo = mid + 1;
        }
        return lo;
    }

    /// Inserts `r`, merging it with every range it overlaps or touches. Needs
    /// room for one range (see `reserve`).
    pub fn addAssumeCapacity(s: *RangeSet, r_in: Range) void {
        if (r_in.len() <= 0) return;
        var r = r_in;
        const rs = s.items.items;
        // The first range that overlaps or touches r from the left, and the
        // first one that starts strictly after r.end.
        const i = firstEndAtLeast(rs, r.start);
        const j = firstStartAbove(rs, r.end);
        if (i == j) {
            s.items.insertAssumeCapacity(i, r);
            return;
        }
        r.start = @min(r.start, rs[i].start);
        r.end = @max(r.end, rs[j - 1].end);
        rs[i] = r;
        s.items.replaceRangeAssumeCapacity(i + 1, j - (i + 1), &.{});
    }

    pub fn add(s: *RangeSet, a: Allocator, r: Range) Allocator.Error!void {
        if (r.len() <= 0) return;
        try s.reserve(a, 1);
        s.addAssumeCapacity(r);
    }

    /// Deletes `r` from the set, splitting a range that straddles it. Needs
    /// room for one range (see `reserve`).
    pub fn removeAssumeCapacity(s: *RangeSet, r: Range) void {
        if (r.len() <= 0) return;
        const rs = s.items.items;
        const i = firstEndAbove(rs, r.start);
        const j = firstStartAtLeast(rs, r.end);
        if (i >= j) return;
        var keep: [2]Range = undefined;
        var n: usize = 0;
        if (rs[i].start < r.start) {
            keep[n] = .{ .start = rs[i].start, .end = r.start };
            n += 1;
        }
        if (rs[j - 1].end > r.end) {
            keep[n] = .{ .start = r.end, .end = rs[j - 1].end };
            n += 1;
        }
        s.items.replaceRangeAssumeCapacity(i, j - i, keep[0..n]);
    }

    pub fn remove(s: *RangeSet, a: Allocator, r: Range) Allocator.Error!void {
        if (r.len() <= 0) return;
        try s.reserve(a, 1);
        s.removeAssumeCapacity(r);
    }

    /// The parts of `r` that the set does not cover, in order, appended to
    /// `out`.
    pub fn gaps(s: RangeSet, a: Allocator, r: Range, out: *std.ArrayList(Range)) Allocator.Error!void {
        if (r.len() <= 0) return;
        const rs = s.items.items;
        var pos = r.start;
        var i = firstEndAbove(rs, r.start);
        while (i < rs.len and rs[i].start < r.end) : (i += 1) {
            if (rs[i].start > pos) try out.append(a, .{ .start = pos, .end = rs[i].start });
            pos = @max(pos, rs[i].end);
        }
        if (pos < r.end) try out.append(a, .{ .start = pos, .end = r.end });
    }

    /// The ranges of `s` that `other` does not cover, appended to `out`.
    pub fn minus(s: RangeSet, a: Allocator, other: RangeSet, out: *std.ArrayList(Range)) Allocator.Error!void {
        for (s.items.items) |r| try other.gaps(a, r, out);
    }

    /// Whether every byte of `r` is in the set.
    pub fn covers(s: RangeSet, r: Range) bool {
        if (r.len() <= 0) return true;
        const rs = s.items.items;
        const i = firstEndAbove(rs, r.start);
        return i < rs.len and rs[i].start <= r.start and rs[i].end >= r.end;
    }

    pub fn clone(s: RangeSet, a: Allocator) Allocator.Error!RangeSet {
        return .{ .items = try s.items.clone(a) };
    }
};

/// The bookkeeping of one file: `done`, `damaged` and `lost` never overlap
/// each other.
pub const Tracker = struct {
    done: RangeSet = .{},
    damaged: RangeSet = .{},
    lost: RangeSet = .{},

    pub fn deinit(t: *Tracker, a: Allocator) void {
        t.done.deinit(a);
        t.damaged.deinit(a);
        t.lost.deinit(a);
    }

    /// Makes room for one `markGood`, one `markDamaged` of `pieces` pieces
    /// or one `markLost` of `pieces` pieces, so those cannot fail.
    pub fn reserve(t: *Tracker, a: Allocator, pieces: usize) Allocator.Error!void {
        try t.done.reserve(a, pieces + 1);
        try t.damaged.reserve(a, pieces + 1);
        try t.lost.reserve(a, pieces + 1);
    }

    /// Records `r` as correctly written and clears any damaged or lost state
    /// inside it. Needs `reserve(a, 0)`.
    pub fn markGood(t: *Tracker, r: Range) void {
        t.done.addAssumeCapacity(r);
        t.damaged.removeAssumeCapacity(r);
        t.lost.removeAssumeCapacity(r);
    }

    /// The parts of `r` that a damaged copy may be written to: those not
    /// already holding a good copy.
    pub fn damageable(t: Tracker, a: Allocator, r: Range, out: *std.ArrayList(Range)) Allocator.Error!void {
        try t.done.gaps(a, r, out);
    }

    /// Records the given pieces (from `damageable`) as damaged. Needs
    /// `reserve(a, pieces.len)`.
    pub fn markDamaged(t: *Tracker, pieces: []const Range) void {
        for (pieces) |p| {
            t.damaged.addAssumeCapacity(p);
            t.lost.removeAssumeCapacity(p);
        }
    }

    /// The parts of `r` that are neither done nor damaged, which `markLost`
    /// would record.
    pub fn lostPieces(t: Tracker, a: Allocator, r: Range, out: *std.ArrayList(Range)) Allocator.Error!void {
        var undamaged: std.ArrayList(Range) = .empty;
        defer undamaged.deinit(a);
        try t.damaged.gaps(a, r, &undamaged);
        for (undamaged.items) |g| try t.done.gaps(a, g, out);
    }

    /// Records the given pieces (from `lostPieces`) as permanently
    /// unavailable. Needs `reserve(a, pieces.len)`.
    pub fn markLost(t: *Tracker, pieces: []const Range) void {
        for (pieces) |p| t.lost.addAssumeCapacity(p);
    }

    /// Every byte of [0, size) that is neither done nor damaged, appended to
    /// `out`.
    pub fn missing(t: Tracker, a: Allocator, size: i64, out: *std.ArrayList(Range)) Allocator.Error!void {
        var undamaged: std.ArrayList(Range) = .empty;
        defer undamaged.deinit(a);
        try t.damaged.gaps(a, .{ .start = 0, .end = size }, &undamaged);
        for (undamaged.items) |g| try t.done.gaps(a, g, out);
    }

    /// Whether no byte is in more than one of the sets.
    pub fn disjoint(t: Tracker) bool {
        return setsDisjoint(&.{ t.done.slice(), t.damaged.slice(), t.lost.slice() });
    }
};

/// Whether no byte is in more than one of the sets (each is sorted and
/// disjoint on its own). A merge over the ranges of all sets by start.
pub fn setsDisjoint(sets: []const []const Range) bool {
    var idx: [8]usize = @splat(0);
    std.debug.assert(sets.len <= idx.len);
    var prev_end: i64 = std.math.minInt(i64);
    while (true) {
        var best: ?usize = null;
        for (sets, 0..) |s, k| {
            if (idx[k] >= s.len) continue;
            if (best == null or s[idx[k]].start < sets[best.?][idx[best.?]].start) best = k;
        }
        const k = best orelse return true;
        const x = sets[k][idx[k]];
        if (x.start < prev_end) return false;
        prev_end = x.end;
        idx[k] += 1;
    }
}

// ---- tests ----

const testing = std.testing;

fn expectSet(want: []const Range, got: RangeSet) !void {
    try testing.expectEqual(want.len, got.slice().len);
    for (want, got.slice()) |w, g| try testing.expect(w.eql(g));
}

fn rg(a: i64, b: i64) Range {
    return .{ .start = a, .end = b };
}

test "add merges overlapping and touching ranges and ignores empty ones" {
    const a = testing.allocator;
    var s: RangeSet = .{};
    defer s.deinit(a);
    try s.add(a, rg(10, 20));
    try s.add(a, rg(30, 40));
    try s.add(a, rg(5, 5)); // empty
    try s.add(a, rg(7, 3)); // negative length
    try expectSet(&.{ rg(10, 20), rg(30, 40) }, s);
    try s.add(a, rg(20, 30)); // touches both
    try expectSet(&.{rg(10, 40)}, s);
    try s.add(a, rg(0, 12));
    try expectSet(&.{rg(0, 40)}, s);
    try s.add(a, rg(50, 60));
    try s.add(a, rg(45, 49)); // separate, not adjacent
    try expectSet(&.{ rg(0, 40), rg(45, 49), rg(50, 60) }, s);
    try s.add(a, rg(0, 100));
    try expectSet(&.{rg(0, 100)}, s);
}

test "remove splits ranges that straddle the removed one" {
    const a = testing.allocator;
    var s: RangeSet = .{};
    defer s.deinit(a);
    try s.add(a, rg(0, 100));
    try s.remove(a, rg(40, 60));
    try expectSet(&.{ rg(0, 40), rg(60, 100) }, s);
    try s.remove(a, rg(0, 10));
    try expectSet(&.{ rg(10, 40), rg(60, 100) }, s);
    try s.remove(a, rg(30, 70));
    try expectSet(&.{ rg(10, 30), rg(70, 100) }, s);
    try s.remove(a, rg(30, 70)); // nothing there
    try s.remove(a, rg(5, 5)); // empty
    try expectSet(&.{ rg(10, 30), rg(70, 100) }, s);
    try s.remove(a, rg(0, 1000));
    try expectSet(&.{}, s);
}

test "gaps, minus and covers" {
    const a = testing.allocator;
    var s: RangeSet = .{};
    defer s.deinit(a);
    try s.add(a, rg(10, 20));
    try s.add(a, rg(30, 40));
    var out: std.ArrayList(Range) = .empty;
    defer out.deinit(a);
    try s.gaps(a, rg(0, 50), &out);
    try testing.expectEqual(@as(usize, 3), out.items.len);
    try testing.expect(out.items[0].eql(rg(0, 10)) and out.items[1].eql(rg(20, 30)) and out.items[2].eql(rg(40, 50)));
    out.clearRetainingCapacity();
    try s.gaps(a, rg(12, 18), &out);
    try testing.expectEqual(@as(usize, 0), out.items.len);
    try testing.expect(s.covers(rg(12, 18)) and s.covers(rg(10, 20)) and !s.covers(rg(10, 21)) and !s.covers(rg(19, 31)));
    try testing.expect(s.covers(rg(5, 5)));

    var other: RangeSet = .{};
    defer other.deinit(a);
    try other.add(a, rg(15, 35));
    out.clearRetainingCapacity();
    try s.minus(a, other, &out);
    try testing.expectEqual(@as(usize, 2), out.items.len);
    try testing.expect(out.items[0].eql(rg(10, 15)) and out.items[1].eql(rg(35, 40)));
}

test "an allocation failure leaves the set as it was" {
    const a = testing.allocator;
    var s: RangeSet = .{};
    defer s.deinit(a);
    try s.add(a, rg(0, 10));
    try s.add(a, rg(20, 30));
    // Fill to the capacity, then fail the growth.
    while (s.items.items.len < s.items.capacity) {
        const k: i64 = @intCast(s.items.items.len);
        try s.add(a, rg(100 * k, 100 * k + 10));
    }
    const before = try s.clone(a);
    var copy = before;
    defer copy.deinit(a);
    var fa: testing.FailingAllocator = .init(a, .{ .fail_index = 0 });
    try testing.expectError(error.OutOfMemory, s.add(fa.allocator(), rg(5000, 5010)));
    try testing.expectError(error.OutOfMemory, s.remove(fa.allocator(), rg(2, 3)));
    try expectSet(copy.slice(), s);
}

// ---- the tracker against a per-byte model ----

const Cell = enum(u8) { none, done, damaged, lost };

fn modelRanges(a: Allocator, model: []const Cell, want: Cell) Allocator.Error!std.ArrayList(Range) {
    var out: std.ArrayList(Range) = .empty;
    errdefer out.deinit(a);
    var i: usize = 0;
    while (i < model.len) {
        if (model[i] != want) {
            i += 1;
            continue;
        }
        var j = i;
        while (j < model.len and model[j] == want) j += 1;
        try out.append(a, rg(@intCast(i), @intCast(j)));
        i = j;
    }
    return out;
}

fn expectSlices(want: []const Range, got: []const Range) !void {
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expect(w.eql(g));
}

/// Drives good, damaged and lost operations through a Tracker and compares
/// every step with a model with one cell per byte (Go: FuzzRanges). `ops` is
/// read three bytes at a time: operation, start, end.
fn runModel(ops: []const u8) !void {
    const a = testing.allocator;
    const size = 64;
    var model: [size]Cell = @splat(.none);
    var t: Tracker = .{};
    defer t.deinit(a);

    var k: usize = 0;
    while (k + 3 <= ops.len) : (k += 3) {
        const op = ops[k] % 3;
        var x: i64 = ops[k + 1] % (size + 1);
        var y: i64 = ops[k + 2] % (size + 1);
        if (x > y) std.mem.swap(i64, &x, &y);
        const range = rg(x, y);

        var pieces: std.ArrayList(Range) = .empty;
        defer pieces.deinit(a);
        switch (op) {
            0 => { // good copy
                for (@as(usize, @intCast(x))..@as(usize, @intCast(y))) |i| model[i] = .done;
                try t.reserve(a, 0);
                t.markGood(range);
            },
            1 => { // damaged copy: only bytes without a good copy
                var want: std.ArrayList(Range) = .empty;
                defer want.deinit(a);
                for (@as(usize, @intCast(x))..@as(usize, @intCast(y))) |i| {
                    if (model[i] != .done) {
                        model[i] = .damaged;
                        try appendMerged(a, &want, rg(@intCast(i), @intCast(i + 1)));
                    }
                }
                try t.damageable(a, range, &pieces);
                try expectSlices(want.items, pieces.items);
                try t.reserve(a, pieces.items.len);
                t.markDamaged(pieces.items);
            },
            else => { // lost
                for (@as(usize, @intCast(x))..@as(usize, @intCast(y))) |i| {
                    if (model[i] == .none) model[i] = .lost;
                }
                try t.lostPieces(a, range, &pieces);
                try t.reserve(a, pieces.items.len);
                t.markLost(pieces.items);
            },
        }

        inline for (.{ .{ &t.done, Cell.done }, .{ &t.damaged, Cell.damaged }, .{ &t.lost, Cell.lost } }) |pair| {
            var want = try modelRanges(a, &model, pair[1]);
            defer want.deinit(a);
            try expectSlices(want.items, pair[0].slice());
        }

        var want_missing: std.ArrayList(Range) = .empty;
        defer want_missing.deinit(a);
        for (model, 0..) |c, i| {
            if (c == .none or c == .lost) try appendMerged(a, &want_missing, rg(@intCast(i), @intCast(i + 1)));
        }
        var got_missing: std.ArrayList(Range) = .empty;
        defer got_missing.deinit(a);
        try t.missing(a, size, &got_missing);
        try expectSlices(want_missing.items, got_missing.items);
        try testing.expect(t.disjoint());
    }
}

/// Appends `x`, merging it with the last range when they touch.
fn appendMerged(a: Allocator, list: *std.ArrayList(Range), x: Range) Allocator.Error!void {
    if (list.items.len > 0 and list.items[list.items.len - 1].end == x.start) {
        list.items[list.items.len - 1].end = x.end;
        return;
    }
    try list.append(a, x);
}

test "the tracker follows a per-byte model" {
    try runModel(&.{ 0, 3, 10, 1, 5, 20, 2, 0, 63, 0, 8, 9 });
    try runModel(&.{ 1, 0, 64, 0, 10, 30, 1, 5, 50 });
    try runModel(&.{});
    // A longer pseudo-random run.
    var prng: std.Random.DefaultPrng = .init(42);
    var ops: [3 * 400]u8 = undefined;
    prng.random().bytes(&ops);
    try runModel(&ops);
}

fn fuzzTracker(_: void, smith: *testing.Smith) !void {
    var buf: [3 * 64]u8 = undefined;
    const n = smith.slice(&buf);
    try runModel(buf[0..n]);
}

fn seedOf(comptime bytes: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, bytes.len)) ++ bytes.*);
}

test "fuzz: the tracker follows a per-byte model for any operations" {
    const corpus = [_][]const u8{
        seedOf(&.{ 0, 3, 10, 1, 5, 20, 2, 0, 63, 0, 8, 9 }),
        seedOf(&.{ 1, 0, 64, 0, 10, 30, 1, 5, 50 }),
        seedOf(&.{}),
        seedOf(&.{ 2, 0, 64, 1, 10, 20, 0, 15, 17, 2, 0, 64 }),
    };
    for (corpus) |input| {
        var smith: testing.Smith = .{ .in = input };
        try fuzzTracker({}, &smith);
    }
    try testing.fuzz({}, fuzzTracker, .{ .corpus = &corpus });
}

test "setsDisjoint finds overlaps between sets" {
    const a = testing.allocator;
    var x: RangeSet = .{};
    defer x.deinit(a);
    var y: RangeSet = .{};
    defer y.deinit(a);
    try x.add(a, rg(0, 10));
    try x.add(a, rg(20, 30));
    try y.add(a, rg(10, 20));
    try testing.expect(setsDisjoint(&.{ x.slice(), y.slice() }));
    try y.add(a, rg(29, 31));
    try testing.expect(!setsDisjoint(&.{ x.slice(), y.slice() }));
}
