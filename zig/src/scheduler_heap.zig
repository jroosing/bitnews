//! A binary min-heap of Article indices (internal/usenet/scheduler/heap.go).
//! Hand-rolled over u32 so the hot path has no comparator indirection, and so
//! `push` is the only operation that can fail.
const std = @import("std");
const Allocator = std.mem.Allocator;

pub const MinHeap = struct {
    items: std.ArrayList(u32) = .empty,

    pub fn deinit(h: *MinHeap, gpa: Allocator) void {
        h.items.deinit(gpa);
    }

    pub fn len(h: *const MinHeap) usize {
        return h.items.items.len;
    }

    /// The smallest value; the heap must not be empty.
    pub fn peek(h: *const MinHeap) u32 {
        return h.items.items[0];
    }

    pub fn push(h: *MinHeap, gpa: Allocator, v: u32) Allocator.Error!void {
        try h.items.append(gpa, v);
        const s = h.items.items;
        var i = s.len - 1;
        while (i > 0) {
            const p = (i - 1) / 2;
            if (s[p] <= s[i]) break;
            std.mem.swap(u32, &s[p], &s[i]);
            i = p;
        }
    }

    /// Removes and returns the smallest value; the heap must not be empty.
    pub fn pop(h: *MinHeap) u32 {
        const s = h.items.items;
        const top = s[0];
        const n = s.len - 1;
        s[0] = s[n];
        h.items.shrinkRetainingCapacity(n);
        var i: usize = 0;
        while (true) {
            const l = 2 * i + 1;
            if (l >= n) break;
            var m = l;
            if (l + 1 < n and s[l + 1] < s[l]) m = l + 1;
            if (s[i] <= s[m]) break;
            std.mem.swap(u32, &s[i], &s[m]);
            i = m;
        }
        return top;
    }
};

test "pops in ascending order whatever the push order" {
    const gpa = std.testing.allocator;
    var h: MinHeap = .{};
    defer h.deinit(gpa);
    var prng: std.Random.DefaultPrng = .init(7);
    const rnd = prng.random();
    var want: [500]u32 = undefined;
    for (&want) |*w| {
        w.* = rnd.int(u32) % 1000; // duplicates on purpose
        try h.push(gpa, w.*);
    }
    std.mem.sort(u32, &want, {}, std.sort.asc(u32));
    for (want) |w| {
        try std.testing.expectEqual(w, h.peek());
        try std.testing.expectEqual(w, h.pop());
    }
    try std.testing.expectEqual(0, h.len());
}

test "interleaved pushes and pops keep the order" {
    const gpa = std.testing.allocator;
    var h: MinHeap = .{};
    defer h.deinit(gpa);
    try h.push(gpa, 5);
    try h.push(gpa, 3);
    try std.testing.expectEqual(3, h.pop());
    try h.push(gpa, 1);
    try h.push(gpa, 4);
    try std.testing.expectEqual(1, h.pop());
    try std.testing.expectEqual(4, h.pop());
    try std.testing.expectEqual(5, h.pop());
}
