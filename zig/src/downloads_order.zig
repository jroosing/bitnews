//! The order of a Queue and the moves within it (internal/core/queueorder.go).
//! A move never touches the Downloads: it computes the new dense positions
//! 0..n-1 and the caller stores the ones that changed.
const std = @import("std");
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");

pub const Move = enum {
    top,
    bottom,
    up,
    down,

    pub fn parse(s: []const u8) ?Move {
        return std.meta.stringToEnum(Move, s);
    }
};

pub const Position = struct {
    id: []const u8,
    position: i64,
};

/// Orders Downloads of one Queue by queue_position, then added_at, then ID, so
/// the order is total even when positions are duplicated or have gaps.
pub fn compareQueueOrder(a: dl.Download, b: dl.Download) std.math.Order {
    return std.math.order(a.queue_position, b.queue_position)
        .differ() orelse std.math.order(a.added_at, b.added_at)
        .differ() orelse std.mem.order(u8, a.id, b.id);
}

fn lessThan(_: void, a: dl.Download, b: dl.Download) bool {
    return compareQueueOrder(a, b) == .lt;
}

/// The position of every Download of `queue` after `id` is moved. The result
/// holds one entry per Download, in the new order (so entry i has position i).
/// `queue` is not changed. error.NotFound when no Download has the ID.
pub fn reposition(arena: Allocator, queue: []const dl.Download, id: []const u8, move: Move) error{ NotFound, OutOfMemory }![]Position {
    const ordered = try arena.dupe(dl.Download, queue);
    std.mem.sort(dl.Download, ordered, {}, lessThan);
    const i = for (ordered, 0..) |d, k| {
        if (std.mem.eql(u8, d.id, id)) break k;
    } else return error.NotFound;
    const n = ordered.len;
    const target: usize = switch (move) {
        .top => 0,
        .bottom => n - 1,
        .up => i -| 1,
        .down => @min(i + 1, n - 1),
    };
    const moved = ordered[i];
    // Shift the Downloads between the old and the new place by one.
    if (target < i) {
        std.mem.copyBackwards(dl.Download, ordered[target + 1 .. i + 1], ordered[target..i]);
    } else {
        std.mem.copyForwards(dl.Download, ordered[i..target], ordered[i + 1 .. target + 1]);
    }
    ordered[target] = moved;

    const out = try arena.alloc(Position, n);
    for (ordered, out, 0..) |d, *p, k| p.* = .{ .id = d.id, .position = @intCast(k) };
    return out;
}

/// The new position of `id` in the result of `reposition`.
pub fn positionOf(positions: []const Position, id: []const u8) ?i64 {
    for (positions) |p| if (std.mem.eql(u8, p.id, id)) return p.position;
    return null;
}

const testing = std.testing;

fn dlAt(id: []const u8, pos: i64) dl.Download {
    return .{ .id = id, .name = id, .source_kind = .nzb, .state = .queued, .queue_position = pos, .added_at = 1, .updated_at = 1 };
}

/// a, b, c, d at positions 0..3, listed out of order.
fn sampleQueue() [4]dl.Download {
    return .{ dlAt("c", 2), dlAt("a", 0), dlAt("d", 3), dlAt("b", 1) };
}

fn expectPositions(want: []const struct { []const u8, i64 }, got: []const Position) !void {
    try testing.expectEqual(want.len, got.len);
    for (want) |w| try testing.expectEqual(w[1], positionOf(got, w[0]).?);
}

test "top puts the Download first and keeps the others in order" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const q = sampleQueue();
    const got = try reposition(arena.allocator(), &q, "c", .top);
    try expectPositions(&.{ .{ "c", 0 }, .{ "a", 1 }, .{ "b", 2 }, .{ "d", 3 } }, got);
}

test "bottom puts the Download last" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const q = sampleQueue();
    const got = try reposition(arena.allocator(), &q, "b", .bottom);
    try expectPositions(&.{ .{ "a", 0 }, .{ "c", 1 }, .{ "d", 2 }, .{ "b", 3 } }, got);
}

test "up and down swap with the neighbour and stop at the ends" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const q = sampleQueue();
    try expectPositions(&.{ .{ "a", 0 }, .{ "c", 1 }, .{ "b", 2 }, .{ "d", 3 } }, try reposition(arena.allocator(), &q, "c", .up));
    try expectPositions(&.{ .{ "a", 0 }, .{ "c", 1 }, .{ "b", 2 }, .{ "d", 3 } }, try reposition(arena.allocator(), &q, "b", .down));
    try expectPositions(&.{ .{ "a", 0 }, .{ "b", 1 }, .{ "c", 2 }, .{ "d", 3 } }, try reposition(arena.allocator(), &q, "a", .up));
    try expectPositions(&.{ .{ "a", 0 }, .{ "b", 1 }, .{ "c", 2 }, .{ "d", 3 } }, try reposition(arena.allocator(), &q, "d", .down));
}

test "gaps and duplicated positions are renumbered densely" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    // 5, 5, 9, 9 are ties: added_at and then the ID decide.
    const q = [_]dl.Download{ dlAt("d", 9), dlAt("b", 5), dlAt("c", 9), dlAt("a", 5) };
    const got = try reposition(arena.allocator(), &q, "a", .up);
    try expectPositions(&.{ .{ "a", 0 }, .{ "b", 1 }, .{ "c", 2 }, .{ "d", 3 } }, got);
}

test "an unknown Download is not found, and the input is not changed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const q = sampleQueue();
    for (std.enums.values(Move)) |m| try testing.expectError(error.NotFound, reposition(arena.allocator(), &q, "nope", m));
    _ = try reposition(arena.allocator(), &q, "d", .top);
    try testing.expectEqualStrings("c", q[0].id);
    try testing.expectEqualStrings("b", q[3].id);
    try testing.expectError(error.NotFound, reposition(arena.allocator(), &.{}, "a", .top));
}

test "every move gives each Download a unique position from zero" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var prng: std.Random.DefaultPrng = .init(3);
    const rng = prng.random();
    for (0..500) |_| {
        const n = 1 + rng.uintLessThan(usize, 10);
        const q = try arena.allocator().alloc(dl.Download, n);
        for (q, 0..) |*d, i| {
            const id = try std.fmt.allocPrint(arena.allocator(), "d{d:0>2}", .{i});
            d.* = dlAt(id, rng.intRangeLessThan(i64, -5, 15));
        }
        const id = q[rng.uintLessThan(usize, n)].id;
        const move = rng.enumValue(Move);
        const got = try reposition(arena.allocator(), q, id, move);
        try testing.expectEqual(n, got.len);
        const seen = try arena.allocator().alloc(bool, n);
        @memset(seen, false);
        for (q) |d| {
            const p: usize = @intCast(positionOf(got, d.id).?);
            try testing.expect(p < n and !seen[p]);
            seen[p] = true;
        }
    }
}

test "queue order is position, then added_at, then ID" {
    var a = dlAt("a", 1);
    var b = dlAt("b", 0);
    b.added_at = 3600_000;
    const c = dlAt("c", 1);
    try testing.expectEqual(std.math.Order.lt, compareQueueOrder(b, a));
    try testing.expectEqual(std.math.Order.lt, compareQueueOrder(a, c));
    try testing.expectEqual(std.math.Order.eq, compareQueueOrder(a, a));
    a.added_at = 0;
    try testing.expectEqual(std.math.Order.lt, compareQueueOrder(a, c));
    try testing.expectEqual(std.math.Order.gt, compareQueueOrder(c, a));
}

test "Move.parse knows the four moves" {
    try testing.expectEqual(Move.top, Move.parse("top").?);
    try testing.expectEqual(Move.down, Move.parse("down").?);
    try testing.expect(Move.parse("sideways") == null);
    try testing.expect(Move.parse("") == null);
}
