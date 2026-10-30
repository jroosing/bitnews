//! Which Downloads of one Queue should be Fetching (core.Select in
//! internal/core/schedule.go). Pure: it reads the Downloads and decides; the
//! Queue applies the result.
const std = @import("std");
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const order = @import("downloads_order.zig");

/// Download IDs, borrowed from the slice that was passed to `select`, each
/// list in rank order.
pub const Selection = struct {
    /// The chosen Downloads that are still Queued.
    activate: []const []const u8 = &.{},
    /// The Fetching Downloads that were not chosen.
    deactivate: []const []const u8 = &.{},
};

/// Decides which Downloads of one Queue should be Fetching. The caller passes
/// the Downloads of a single Queue, so the protocol's limit applies to them
/// alone.
///
/// Only Queued and Fetching Downloads are candidates, and a Download is active
/// when it is Fetching. A paused Download is never chosen. Force Downloads are
/// always chosen and do not count against `limit`. The others are ranked by
/// priority (high first), then queue position, added_at and ID, and the first
/// `limit` of them are chosen. A negative limit is treated as 0. Applying both
/// lists and calling `select` again gives two empty lists.
///
/// The result is allocated in `arena`; `downloads` is not changed.
pub fn select(arena: Allocator, downloads: []const dl.Download, limit: i64) Allocator.Error!Selection {
    var candidates: std.ArrayList(dl.Download) = .empty;
    for (downloads) |d| {
        if (d.state == .queued or d.state == .fetching) try candidates.append(arena, d);
    }
    std.mem.sort(dl.Download, candidates.items, {}, ranksBefore);

    var activate: std.ArrayList([]const u8) = .empty;
    var deactivate: std.ArrayList([]const u8) = .empty;
    var left: i64 = @max(limit, 0);
    for (candidates.items) |d| {
        var chosen = !d.paused;
        if (chosen and d.priority != .force) {
            chosen = left > 0;
            left -= 1;
        }
        if (chosen and d.state == .queued) {
            try activate.append(arena, d.id);
        } else if (!chosen and d.state == .fetching) {
            try deactivate.append(arena, d.id);
        }
    }
    return .{ .activate = activate.items, .deactivate = deactivate.items };
}

/// Priority first (the enum is declared highest first), then queue order.
fn ranksBefore(_: void, a: dl.Download, b: dl.Download) bool {
    const pa = @backingInt(a.priority);
    const pb = @backingInt(b.priority);
    if (pa != pb) return pa < pb;
    return order.compareQueueOrder(a, b) == .lt;
}

const testing = std.testing;

fn sd(id: []const u8, state: dl.State) dl.Download {
    return .{ .id = id, .name = id, .source_kind = .nzb, .state = state, .added_at = 1, .updated_at = 1 };
}

fn at(d: dl.Download, position: i64) dl.Download {
    var out = d;
    out.queue_position = position;
    return out;
}

fn withPriority(d: dl.Download, p: dl.Priority) dl.Download {
    var out = d;
    out.priority = p;
    return out;
}

fn pausedCopy(d: dl.Download) dl.Download {
    var out = d;
    out.paused = true;
    return out;
}

fn expectIds(want: []const []const u8, got: []const []const u8) !void {
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expectEqualStrings(w, g);
}

test "a queued Download within the limit is activated" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = try select(arena.allocator(), &.{sd("a", .queued)}, 1);
    try expectIds(&.{"a"}, got.activate);
    try expectIds(&.{}, got.deactivate);
}

test "at most limit Downloads are activated, and a negative limit is zero" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const ds = [_]dl.Download{ at(sd("a", .queued), 0), at(sd("b", .queued), 1), at(sd("c", .queued), 2) };
    try expectIds(&.{}, (try select(arena.allocator(), &ds, 0)).activate);
    try expectIds(&.{}, (try select(arena.allocator(), &ds, -3)).activate);
    try expectIds(&.{"a"}, (try select(arena.allocator(), &ds, 1)).activate);
    try expectIds(&.{ "a", "b", "c" }, (try select(arena.allocator(), &ds, 5)).activate);
}

test "a higher priority wins" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const ds = [_]dl.Download{
        withPriority(at(sd("low", .queued), 0), .low),
        withPriority(at(sd("normal", .queued), 1), .normal),
        withPriority(at(sd("high", .queued), 2), .high),
    };
    try expectIds(&.{ "high", "normal" }, (try select(arena.allocator(), &ds, 2)).activate);
}

test "priority ties go by queue position, then added_at, then ID" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    const by_position = [_]dl.Download{ at(sd("a", .queued), 2), at(sd("b", .queued), 0), at(sd("c", .queued), 1) };
    try expectIds(&.{ "b", "c", "a" }, (try select(a, &by_position, 3)).activate);

    var x = sd("a", .queued);
    x.added_at = 120_000;
    var y = sd("b", .queued);
    y.added_at = 0;
    var z = sd("c", .queued);
    z.added_at = 60_000;
    try expectIds(&.{ "b", "c", "a" }, (try select(a, &.{ x, y, z }, 3)).activate);

    try expectIds(&.{ "a", "b", "c" }, (try select(a, &.{ sd("c", .queued), sd("a", .queued), sd("b", .queued) }, 3)).activate);

    var old = at(sd("old", .queued), 1);
    old.added_at = 0;
    var new = at(sd("new", .queued), 0);
    new.added_at = 3_600_000;
    try expectIds(&.{ "new", "old" }, (try select(a, &.{ old, new }, 2)).activate);
}

test "Fetching Downloads that still fit are kept" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = try select(arena.allocator(), &.{ at(sd("fetching", .fetching), 0), at(sd("waiting", .queued), 1) }, 1);
    try expectIds(&.{}, got.activate);
    try expectIds(&.{}, got.deactivate);
}

test "a higher priority arrival swaps out a Fetching Download" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = try select(arena.allocator(), &.{
        at(sd("fetching", .fetching), 0),
        withPriority(at(sd("urgent", .queued), 1), .high),
    }, 1);
    try expectIds(&.{"urgent"}, got.activate);
    try expectIds(&.{"fetching"}, got.deactivate);
}

test "a paused Download is never chosen and does not use a slot" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = try select(arena.allocator(), &.{
        pausedCopy(at(sd("paused-queued", .queued), 0)),
        pausedCopy(at(sd("paused-fetching", .fetching), 1)),
        pausedCopy(withPriority(at(sd("paused-force", .queued), 2), .force)),
        at(sd("next", .queued), 3),
    }, 1);
    try expectIds(&.{"next"}, got.activate);
    try expectIds(&.{"paused-fetching"}, got.deactivate);
}

test "force Downloads ignore the limit" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const ds = [_]dl.Download{
        at(sd("normal", .queued), 0),
        withPriority(at(sd("force-1", .queued), 5), .force),
        withPriority(at(sd("force-2", .queued), 6), .force),
        withPriority(at(sd("force-3", .fetching), 7), .force),
    };
    const none = try select(arena.allocator(), &ds, 0);
    try expectIds(&.{ "force-1", "force-2" }, none.activate);
    try expectIds(&.{}, none.deactivate);
    const one = try select(arena.allocator(), &ds, 1);
    try expectIds(&.{ "force-1", "force-2", "normal" }, one.activate);
    try expectIds(&.{}, one.deactivate);
}

test "Fetching Downloads beyond the limit are deactivated" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = try select(arena.allocator(), &.{
        at(sd("a", .fetching), 0), at(sd("b", .fetching), 1), at(sd("c", .fetching), 2),
    }, 1);
    try expectIds(&.{}, got.activate);
    try expectIds(&.{ "b", "c" }, got.deactivate);
}

test "Downloads that are neither Queued nor Fetching are ignored" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var ds: std.ArrayList(dl.Download) = .empty;
    for ([_]dl.State{ .added, .fetching_metadata, .fetched, .post_processing, .ready, .failed }, 0..) |s, i| {
        try ds.append(arena.allocator(), withPriority(at(sd(@tagName(s), s), @intCast(i)), .high));
    }
    try ds.append(arena.allocator(), at(sd("queued", .queued), 10));
    const got = try select(arena.allocator(), ds.items, 1);
    try expectIds(&.{"queued"}, got.activate);
    try expectIds(&.{}, got.deactivate);
}

test "select does not change its input" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const ds = [_]dl.Download{ at(sd("b", .queued), 1), at(sd("a", .queued), 0) };
    _ = try select(arena.allocator(), &ds, 1);
    try testing.expectEqualStrings("b", ds[0].id);
    try testing.expectEqualStrings("a", ds[1].id);
}

fn setState(ds: []dl.Download, ids: []const []const u8, from: dl.State, to: dl.State) !void {
    for (ids) |id| {
        const d = for (ds) |*d| {
            if (std.mem.eql(u8, d.id, id)) break d;
        } else return error.TestUnexpectedResult;
        try testing.expectEqual(from, d.state);
        d.state = to;
    }
}

test "random queues hold the invariants and reach a fixed point" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var prng: std.Random.DefaultPrng = .init(1);
    const rng = prng.random();
    const states = [_]dl.State{ .queued, .queued, .fetching, .fetching, .added, .fetched, .ready, .failed };

    for (0..2000) |_| {
        const n = rng.uintLessThan(usize, 15);
        const ds = try a.alloc(dl.Download, n);
        for (ds, 0..) |*d, i| {
            d.* = sd(try std.fmt.allocPrint(a, "d{d:0>2}", .{i}), states[rng.uintLessThan(usize, states.len)]);
            d.priority = rng.enumValue(dl.Priority);
            d.queue_position = rng.intRangeLessThan(i64, 0, 5);
            d.paused = rng.uintLessThan(u8, 4) == 0;
            d.added_at = rng.intRangeLessThan(i64, 0, 3) * 60_000;
        }
        const limit = rng.intRangeLessThan(i64, -2, 7);
        var want_slots: i64 = 0;
        for (ds) |d| {
            if ((d.state == .queued or d.state == .fetching) and !d.paused and d.priority != .force) want_slots += 1;
        }
        want_slots = @min(want_slots, @max(limit, 0));

        const got = try select(a, ds, limit);
        const after = try a.dupe(dl.Download, ds);
        try setState(after, got.activate, .queued, .fetching);
        try setState(after, got.deactivate, .fetching, .queued);

        var got_slots: i64 = 0;
        for (after) |d| {
            if (!(d.state == .queued or d.state == .fetching)) continue;
            const active = d.state == .fetching;
            if (d.paused) try testing.expect(!active);
            if (d.priority == .force and !d.paused) try testing.expect(active);
            if (d.priority != .force and active) got_slots += 1;
        }
        try testing.expectEqual(want_slots, got_slots);

        const again = try select(a, after, limit);
        try testing.expectEqual(0, again.activate.len);
        try testing.expectEqual(0, again.deactivate.len);
    }
}
