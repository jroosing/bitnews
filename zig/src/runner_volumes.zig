//! Which deferred PAR2 recovery volumes to fetch after the first pass
//! (internal/usenet/runner volumeplan.go).
//!
//! bitnews never opens a PAR2 file: `par2` verifies and repairs. This module
//! only chooses WHICH volumes to download once `par2` has said how many
//! recovery blocks it is short of. It works purely from the NZB file names and
//! counts: `nzb.File.kind` and `recovery_blocks` come from the name
//! `name.volNN+MM.par2`, and the number of blocks needed is an input. Where
//! that number comes from (the post-processing stage that ran `par2`) is a seam
//! of Z4: `Runner.fetchDeferred(id, min_blocks)`.
//!
//! Algorithm. Choosing the cheapest set of volumes with at least T blocks is a
//! covering knapsack. `planVolumes` solves it exactly by dynamic programming
//! over the blocks covered (capped at T = min(needed + margin, total blocks)),
//! which takes O(volumes x T) time and memory, and as the table of that size
//! is only small for realistic sets, it uses it while volumes x T is at most
//! about four million cells. Larger inputs (hostile names claiming huge block
//! counts) use a greedy planner: volumes by ascending bytes per block until T
//! is covered, then redundant volumes (most expensive first) are dropped. The
//! greedy plan is always valid but not always the cheapest.
//!
//! Among plans of equal bytes the one with fewer volumes wins, then the one
//! that prefers earlier files.
const std = @import("std");
const Allocator = std.mem.Allocator;
const nzb = @import("nzb.zig");

/// The number of recovery blocks fetched beyond what the damage needs, in
/// case one of the fetched volumes turns out damaged.
pub const default_margin: i64 = 1;

/// Bounds the size of the exact planner's table (volumes times target blocks).
/// Beyond it `planVolumes` falls back to the greedy planner.
pub const exact_cell_limit: u64 = 1 << 22;

/// A candidate recovery volume: an index into the caller's files, the blocks
/// it holds and what it costs to fetch.
const Volume = struct {
    index: u32,
    blocks: u64,
    bytes: i64,
};

pub const Plan = struct {
    /// Indexes into the files passed in, ascending.
    picks: []const u32,
    /// The sum of the blocks of the picks.
    covered: u64,
    /// False when all volumes together cannot cover what is needed.
    ok: bool,
};

fn satAdd64(a: u64, b: u64) u64 {
    return std.math.add(u64, a, b) catch std.math.maxInt(u64);
}

fn satAddI64(a: i64, b: i64) i64 {
    return std.math.add(i64, a, b) catch std.math.maxInt(i64);
}

/// The usable volumes of `files`, in file order. Files that are not recovery
/// volumes, or that claim no blocks, are skipped.
fn volumesOf(a: Allocator, files: []const nzb.File) Allocator.Error![]Volume {
    var out: std.ArrayList(Volume) = .empty;
    for (files, 0..) |f, i| {
        if (!f.deferred() or f.recovery_blocks == 0) continue;
        try out.append(a, .{ .index = @intCast(i), .blocks = f.recovery_blocks, .bytes = f.bytes() });
    }
    return out.items;
}

/// Picks the recovery volumes to fetch (indexes into `files`) so that their
/// blocks cover needed + margin, at the lowest cost in bytes. Only deferred
/// files with recovery blocks are candidates. A negative margin counts as 0.
///
/// `ok` is false when all volumes together cannot cover `needed` (the margin
/// is best effort: when the volumes cover `needed` but not needed + margin,
/// all of them are picked). When `needed` is not positive nothing has to be
/// fetched: no picks, covered 0, ok true. Everything is allocated in `a`.
pub fn planVolumes(a: Allocator, files: []const nzb.File, needed: i64, margin: i64) Allocator.Error!Plan {
    if (needed <= 0) return .{ .picks = &.{}, .covered = 0, .ok = true };
    const vols = try volumesOf(a, files);
    var total: u64 = 0;
    for (vols) |v| total = satAdd64(total, v.blocks);
    if (total < @as(u64, @intCast(needed))) return .{ .picks = &.{}, .covered = 0, .ok = false };
    const target = @min(satAdd64(@intCast(needed), @intCast(@max(margin, 0))), total);

    const chosen = if (@as(u64, vols.len) *| target <= exact_cell_limit)
        try planExact(a, vols, @intCast(target))
    else
        try planGreedy(a, vols, target);

    var picks = try a.alloc(u32, chosen.len);
    var covered: u64 = 0;
    for (chosen, 0..) |v, i| {
        picks[i] = v.index;
        covered = satAdd64(covered, v.blocks);
    }
    std.mem.sort(u32, picks, {}, std.sort.asc(u32));
    return .{ .picks = picks, .covered = covered, .ok = true };
}

/// Orders plans: fewer bytes first, then fewer volumes.
const Cost = struct {
    bytes: i64 = 0,
    count: u32 = 0,

    fn less(c: Cost, o: Cost) bool {
        if (c.bytes != o.bytes) return c.bytes < o.bytes;
        return c.count < o.count;
    }
};

const unreachable_bytes = std.math.maxInt(i64);

/// The dynamic program behind `planVolumes`. best[j] is the cheapest cost to
/// cover at least j blocks (j capped at target); take[i][j] records whether
/// volume i improved best[j] so the plan can be rebuilt. The target is
/// reachable: the volumes' blocks sum to at least target.
fn planExact(a: Allocator, vols: []const Volume, target: usize) Allocator.Error![]const Volume {
    const best = try a.alloc(Cost, target + 1);
    best[0] = .{};
    for (best[1..]) |*c| c.* = .{ .bytes = unreachable_bytes };
    const width = target + 1;
    const take = try a.alloc(bool, vols.len * width);
    @memset(take, false);
    for (vols, 0..) |v, i| {
        // Descending j keeps this a 0/1 knapsack: best[from] is still the
        // state before volume i.
        var j = target;
        while (j >= 1) : (j -= 1) {
            const from: usize = if (v.blocks >= j) 0 else j - @as(usize, @intCast(v.blocks));
            if (best[from].bytes == unreachable_bytes) continue;
            const cand: Cost = .{ .bytes = satAddI64(best[from].bytes, v.bytes), .count = best[from].count + 1 };
            if (cand.less(best[j])) {
                best[j] = cand;
                take[i * width + j] = true;
            }
        }
    }
    var chosen: std.ArrayList(Volume) = .empty;
    var j = target;
    var i = vols.len;
    while (i > 0 and j > 0) {
        i -= 1;
        if (take[i * width + j]) {
            try chosen.append(a, vols[i]);
            j = if (vols[i].blocks >= j) 0 else j - @as(usize, @intCast(vols[i].blocks));
        }
    }
    return chosen.items;
}

/// The fallback of `planVolumes` for inputs too large for `planExact`.
fn planGreedy(a: Allocator, vols: []const Volume, target: u64) Allocator.Error![]const Volume {
    const order = try a.dupe(Volume, vols);
    // Ascending bytes per block (float64: exactness does not matter for a
    // heuristic). Stable, so ties keep the file order.
    std.mem.sort(Volume, order, {}, struct {
        fn less(_: void, x: Volume, y: Volume) bool {
            return @as(f64, @floatFromInt(x.bytes)) / @as(f64, @floatFromInt(x.blocks)) <
                @as(f64, @floatFromInt(y.bytes)) / @as(f64, @floatFromInt(y.blocks));
        }
    }.less);
    var chosen: std.ArrayList(Volume) = .empty;
    var sum: u64 = 0;
    for (order) |v| {
        if (sum >= target) break;
        try chosen.append(a, v);
        sum = satAdd64(sum, v.blocks);
    }
    // Drop volumes that are redundant, most expensive first.
    std.mem.sort(Volume, chosen.items, {}, struct {
        fn less(_: void, x: Volume, y: Volume) bool {
            return x.bytes > y.bytes;
        }
    }.less);
    var kept: std.ArrayList(Volume) = .empty;
    for (chosen.items) |v| {
        if (sum - v.blocks >= target) {
            sum -= v.blocks;
            continue;
        }
        try kept.append(a, v);
    }
    return kept.items;
}

/// The steps of `fetchDeferred`, so the decision can be tested without a
/// Runner.
pub const Steps = struct {
    ctx: *anyopaque,
    /// The deferred volumes not fetched yet, in NZB order. The indexes the
    /// other steps take are indexes into this list.
    list_fn: *const fn (ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror![]const nzb.File,
    undefer_fn: *const fn (ctx: *anyopaque, id: []const u8, indexes: []const u32) anyerror!void,
    /// Moves the Download from PostProcessing back to Fetching.
    requeue_fn: *const fn (ctx: *anyopaque, id: []const u8) anyerror!void,
};

/// Plans the cheapest sufficient volumes for `min_blocks`, un-defers them, and
/// requeues the Download. Returns false and changes nothing when all deferred
/// volumes together cannot cover `min_blocks`, or nothing is needed. If the
/// un-defer succeeds and the requeue fails, the error is returned: the volumes
/// stay un-deferred, which a retry finds already done. `margin` is the number
/// of blocks fetched beyond the minimum; zero means `default_margin`, a
/// negative value means none.
pub fn fetchDeferred(a: Allocator, steps: Steps, id: []const u8, min_blocks: i64, margin: i64) anyerror!bool {
    const files = try steps.list_fn(steps.ctx, a, id);
    const m = if (margin == 0) default_margin else margin;
    const plan = try planVolumes(a, files, min_blocks, m);
    if (!plan.ok or plan.picks.len == 0) return false;
    try steps.undefer_fn(steps.ctx, id, plan.picks);
    try steps.requeue_fn(steps.ctx, id);
    return true;
}

// ---- tests ----

const testing = std.testing;

/// A recovery volume of one Article. Comptime arguments: the segment lives in
/// static memory.
fn vol(comptime blocks: u32, comptime bytes: i64) nzb.File {
    return .{ .kind = .par2_volume, .recovery_blocks = blocks, .segments = &.{.{ .number = 1, .bytes = bytes, .message_id = "x" }} };
}

fn volRuntime(a: Allocator, blocks: u32, bytes: i64) !nzb.File {
    const seg = try a.create(nzb.Segment);
    seg.* = .{ .number = 1, .bytes = bytes, .message_id = "x" };
    return .{ .kind = .par2_volume, .recovery_blocks = blocks, .segments = seg[0..1] };
}

fn multiSeg(a: Allocator, blocks: u32, sizes: []const i64) !nzb.File {
    const segs = try a.alloc(nzb.Segment, sizes.len);
    for (segs, sizes, 1..) |*s, size, n| s.* = .{ .number = @intCast(n), .bytes = size, .message_id = "x" };
    return .{ .kind = .par2_volume, .recovery_blocks = blocks, .segments = segs };
}

const Case = struct {
    name: []const u8,
    files: []const nzb.File,
    needed: i64,
    margin: i64 = 0,
    picks: []const u32 = &.{},
    covered: u64 = 0,
    ok: bool,
};

test "planVolumes follows the table of the Go tests" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const cases = [_]Case{
        .{ .name = "exact cover", .files = &.{ vol(1, 100), vol(2, 200), vol(4, 400) }, .needed = 4, .picks = &.{2}, .covered = 4, .ok = true },
        .{ .name = "prefers fewer bytes over fewer volumes", .files = &.{ vol(4, 1000), vol(2, 100), vol(2, 100) }, .needed = 4, .picks = &.{ 1, 2 }, .covered = 4, .ok = true },
        .{ .name = "prefers fewer bytes over fewer blocks", .files = &.{ vol(8, 900), vol(3, 100), vol(3, 100) }, .needed = 5, .picks = &.{ 1, 2 }, .covered = 6, .ok = true },
        .{ .name = "margin adds blocks", .files = &.{ vol(1, 10), vol(1, 10), vol(1, 10) }, .needed = 1, .margin = 1, .picks = &.{ 0, 1 }, .covered = 2, .ok = true },
        .{ .name = "margin is best effort", .files = &.{ vol(2, 10), vol(1, 10) }, .needed = 3, .margin = 1, .picks = &.{ 0, 1 }, .covered = 3, .ok = true },
        .{ .name = "negative margin counts as zero", .files = &.{ vol(1, 10), vol(1, 10) }, .needed = 1, .margin = -5, .picks = &.{0}, .covered = 1, .ok = true },
        .{ .name = "impossible", .files = &.{ vol(1, 10), vol(2, 10) }, .needed = 4, .margin = 1, .ok = false },
        .{ .name = "single volume", .files = &.{vol(5, 10)}, .needed = 3, .margin = 1, .picks = &.{0}, .covered = 5, .ok = true },
        .{ .name = "zero needed fetches nothing", .files = &.{vol(5, 10)}, .needed = 0, .margin = 1, .ok = true },
        .{ .name = "no volumes", .files = &.{}, .needed = 1, .ok = false },
        .{ .name = "no volumes and nothing needed", .files = &.{}, .needed = 0, .ok = true },
        .{
            .name = "other kinds and empty volumes are ignored",
            .files = &.{
                .{ .kind = .data, .recovery_blocks = 9, .segments = &.{.{ .number = 1, .bytes = 1, .message_id = "x" }} },
                .{ .kind = .par2_index, .segments = &.{.{ .number = 1, .bytes = 1, .message_id = "x" }} },
                vol(0, 1),
                vol(2, 50),
            },
            .needed = 2,
            .picks = &.{3},
            .covered = 2,
            .ok = true,
        },
        .{ .name = "ties prefer fewer volumes", .files = &.{ vol(1, 50), vol(1, 50), vol(2, 100) }, .needed = 2, .picks = &.{2}, .covered = 2, .ok = true },
        .{ .name = "segment bytes are summed", .files = &.{ try multiSeg(a, 2, &.{ 100, 100 }), vol(2, 150) }, .needed = 2, .picks = &.{1}, .covered = 2, .ok = true },
    };
    for (cases) |c| {
        const got = try planVolumes(a, c.files, c.needed, c.margin);
        errdefer std.debug.print("case: {s}\n", .{c.name});
        try testing.expectEqual(c.ok, got.ok);
        try testing.expectEqualSlices(u32, c.picks, got.picks);
        try testing.expectEqual(c.covered, got.covered);
    }
}

fn randomVolumes(a: Allocator, r: std.Random, n: usize) ![]nzb.File {
    const files = try a.alloc(nzb.File, n);
    for (files) |*f| f.* = try volRuntime(a, 1 + r.uintLessThan(u32, 6), 1 + r.intRangeLessThan(i64, 0, 30));
    return files;
}

fn costOf(files: []const nzb.File, picks: []const u32) Cost {
    var c: Cost = .{};
    for (picks) |i| {
        c.bytes += files[i].bytes();
        c.count += 1;
    }
    return c;
}

/// The cheapest subset with at least `target` blocks, by trying them all.
fn bruteForce(files: []const nzb.File, target: u64) ?Cost {
    var best: ?Cost = null;
    var mask: u32 = 0;
    while (mask < @as(u32, 1) << @intCast(files.len)) : (mask += 1) {
        var c: Cost = .{};
        var blocks: u64 = 0;
        for (files, 0..) |f, i| {
            if (mask & (@as(u32, 1) << @intCast(i)) != 0) {
                blocks += f.recovery_blocks;
                c.bytes += f.bytes();
                c.count += 1;
            }
        }
        if (blocks >= target and (best == null or c.less(best.?))) best = c;
    }
    return best;
}

test "planVolumes matches a brute force on random volumes" {
    var prng: std.Random.DefaultPrng = .init(1);
    const r = prng.random();
    for (0..500) |_| {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const a = arena.allocator();
        const files = try randomVolumes(a, r, r.uintLessThan(usize, 11));
        const needed = r.intRangeLessThan(i64, 0, 30);
        const margin = r.intRangeLessThan(i64, 0, 3);
        var total: u64 = 0;
        for (files) |f| total += f.recovery_blocks;

        const got = try planVolumes(a, files, needed, margin);
        if (needed > 0 and total < needed) {
            try testing.expect(!got.ok);
            continue;
        }
        try testing.expect(got.ok);
        if (needed == 0) {
            try testing.expectEqual(0, got.picks.len);
            continue;
        }
        const target = @min(@as(u64, @intCast(needed + margin)), total);
        const want = bruteForce(files, target).?;
        const have = costOf(files, got.picks);
        try testing.expectEqual(want.bytes, have.bytes);
        try testing.expectEqual(want.count, have.count);
        var sum: u64 = 0;
        for (got.picks) |i| sum += files[i].recovery_blocks;
        try testing.expectEqual(sum, got.covered);
        try testing.expect(got.covered >= target);
        try testing.expect(std.sort.isSorted(u32, got.picks, {}, std.sort.asc(u32)));
    }
}

test "the greedy planner is valid, has no redundant volume and never beats the optimum" {
    var prng: std.Random.DefaultPrng = .init(3);
    const r = prng.random();
    for (0..500) |_| {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const a = arena.allocator();
        const files = try randomVolumes(a, r, 1 + r.uintLessThan(usize, 10));
        var total: u64 = 0;
        for (files) |f| total += f.recovery_blocks;
        const target = 1 + r.uintLessThan(u64, total);

        const chosen = try planGreedy(a, try volumesOf(a, files), target);
        var c: Cost = .{};
        var sum: u64 = 0;
        for (chosen, 0..) |v, i| {
            for (chosen[0..i]) |w| try testing.expect(w.index != v.index);
            sum += v.blocks;
            c.bytes += v.bytes;
            c.count += 1;
        }
        try testing.expect(sum >= target);
        for (chosen) |v| try testing.expect(sum - v.blocks < target);
        const opt = bruteForce(files, target).?;
        try testing.expect(!c.less(opt));
    }
}

test "a huge input falls back to the greedy planner" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    // 3 volumes x 1.5 million blocks exceeds the exact planner's table.
    const files = [_]nzb.File{ vol(2_000_000, 100), vol(2_000_000, 50), vol(10, 5) };
    const p = try planVolumes(a, &files, 1_500_000, 1);
    try testing.expect(p.ok);
    try testing.expectEqualSlices(u32, &.{1}, p.picks);
    try testing.expectEqual(2_000_000, p.covered);
    const q = try planVolumes(a, &files, 5_000_000, 1);
    try testing.expect(!q.ok);
}

test "block counts never overflow" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const files = [_]nzb.File{ vol(std.math.maxInt(u32), 1), vol(std.math.maxInt(u32), 1) };
    const p = try planVolumes(arena.allocator(), &files, std.math.maxInt(i64), std.math.maxInt(i64));
    try testing.expect(!p.ok); // 2 * 2^32 blocks cannot cover 2^63
    const q = try planVolumes(arena.allocator(), &files, std.math.maxInt(u32), std.math.maxInt(i64));
    try testing.expect(q.ok);
    try testing.expectEqual(2, q.picks.len); // the margin asks for everything
}

/// Records the order of the steps' calls.
const Fake = struct {
    log: std.ArrayList([]const u8) = .empty,
    files: []const nzb.File,
    list_err: ?anyerror = null,
    undefer_err: ?anyerror = null,
    requeue_err: ?anyerror = null,
    undeferred: []const u32 = &.{},

    fn steps(f: *Fake) Steps {
        return .{ .ctx = f, .list_fn = list, .undefer_fn = undefer, .requeue_fn = requeue };
    }

    fn note(f: *Fake, what: []const u8) void {
        f.log.append(testing.allocator, what) catch @panic("oom");
    }

    fn list(ctx: *anyopaque, _: Allocator, _: []const u8) anyerror![]const nzb.File {
        const f: *Fake = @ptrCast(@alignCast(ctx));
        f.note("list");
        if (f.list_err) |e| return e;
        return f.files;
    }

    fn undefer(ctx: *anyopaque, _: []const u8, idx: []const u32) anyerror!void {
        const f: *Fake = @ptrCast(@alignCast(ctx));
        f.note("undefer");
        f.undeferred = idx;
        if (f.undefer_err) |e| return e;
    }

    fn requeue(ctx: *anyopaque, _: []const u8) anyerror!void {
        const f: *Fake = @ptrCast(@alignCast(ctx));
        f.note("requeue");
        if (f.requeue_err) |e| return e;
    }

    fn expectLog(f: *Fake, want: []const []const u8) !void {
        defer f.log.deinit(testing.allocator);
        try testing.expectEqual(want.len, f.log.items.len);
        for (want, f.log.items) |w, g| try testing.expectEqualStrings(w, g);
    }
};

test "fetchDeferred undefers then requeues" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var f: Fake = .{ .files = &.{ vol(4, 400), vol(2, 100), vol(2, 100) } };
    try testing.expect(try fetchDeferred(arena.allocator(), f.steps(), "d1", 3, 0));
    // 3 blocks + the default margin of 1 = 4, cheapest as the two small ones.
    try testing.expectEqualSlices(u32, &.{ 1, 2 }, f.undeferred);
    try f.expectLog(&.{ "list", "undefer", "requeue" });
}

test "fetchDeferred margins: none and explicit" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var f: Fake = .{ .files = &.{ vol(1, 10), vol(1, 10), vol(1, 10) } };
    try testing.expect(try fetchDeferred(arena.allocator(), f.steps(), "d", 1, -1));
    try testing.expectEqualSlices(u32, &.{0}, f.undeferred);
    f.log.deinit(testing.allocator);
    var g: Fake = .{ .files = &.{ vol(1, 10), vol(1, 10), vol(1, 10) } };
    _ = try fetchDeferred(arena.allocator(), g.steps(), "d", 1, 2);
    try testing.expectEqualSlices(u32, &.{ 0, 1, 2 }, g.undeferred);
    g.log.deinit(testing.allocator);
}

test "fetchDeferred changes nothing when the volumes cannot cover the need" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var f: Fake = .{ .files = &.{ vol(1, 10), vol(2, 10) } };
    try testing.expect(!try fetchDeferred(arena.allocator(), f.steps(), "d1", 4, 0));
    try f.expectLog(&.{"list"});
    var g: Fake = .{ .files = &.{vol(1, 10)} };
    try testing.expect(!try fetchDeferred(arena.allocator(), g.steps(), "d1", 0, 0));
    try g.expectLog(&.{"list"});
}

test "fetchDeferred errors propagate and stop the later steps" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var f: Fake = .{ .files = &.{vol(2, 10)}, .list_err = error.Boom };
    try testing.expectError(error.Boom, fetchDeferred(a, f.steps(), "d", 1, 0));
    try f.expectLog(&.{"list"});
    var g: Fake = .{ .files = &.{vol(2, 10)}, .undefer_err = error.Boom };
    try testing.expectError(error.Boom, fetchDeferred(a, g.steps(), "d", 1, 0));
    try g.expectLog(&.{ "list", "undefer" });
    var h: Fake = .{ .files = &.{vol(2, 10)}, .requeue_err = error.Boom };
    try testing.expectError(error.Boom, fetchDeferred(a, h.steps(), "d", 1, 0));
    try h.expectLog(&.{ "list", "undefer", "requeue" });
}
