//! The event bus (internal/core/bus.go): delivers Events to subscribers inside
//! the process. Publishing never blocks. Each subscriber has a bounded queue;
//! a full queue drops progress events silently and closes the subscriber with
//! `error.Resync` when it would lose a lifecycle event, so it refetches its
//! state (SSE clients reconnect).
//!
//! Memory: `publish` deep-copies the event once into a `Shared` that has its
//! own arena and a reference count (one per queue it sits in). A subscriber
//! gets an `Item` and releases it when done; the last release frees the
//! arena. Events that no subscriber took are freed by `publish` itself.
//!
//! Owners: the `Bus` is heap allocated by `init` and owned by the caller until
//! `deinit`, which needs every `Subscription` deinit'ed first. A `Subscription`
//! is owned by the thread that reads it (`next`, `deinit`); `close` may be
//! called from any thread. One mutex guards the subscriber list and every
//! queue; it is only held for a few pointer moves and one event copy.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const event = @import("event.zig");

pub const Event = event.Event;
pub const Publisher = event.Publisher;

/// Capacity of each subscriber's queue (SubscriberBuffer in bus.go).
pub const default_buffer = 256;

pub const Options = struct {
    buffer: usize = default_buffer,
};

/// Why a subscription was closed by the bus rather than by its owner.
pub const Reason = error{Resync};

pub const NextError = error{
    /// The subscription is closed and its queue is empty; see `reason`.
    Closed,
    Timeout,
    Canceled,
};

/// One event, shared by every queue it was put into.
const Shared = struct {
    arena: std.heap.ArenaAllocator,
    refs: std.atomic.Value(u32),
    event: Event,

    fn create(gpa: Allocator, ev: Event) Allocator.Error!*Shared {
        var arena: std.heap.ArenaAllocator = .init(gpa);
        const sh = arena.allocator().create(Shared) catch {
            arena.deinit();
            return error.OutOfMemory;
        };
        sh.arena = arena;
        sh.refs = .init(0);
        sh.event = clone(Event, sh.arena.allocator(), ev) catch {
            // `sh` lives in the arena: free through a copy of its state.
            const doomed = sh.arena;
            doomed.deinit();
            return error.OutOfMemory;
        };
        return sh;
    }

    fn unref(sh: *Shared) void {
        if (sh.refs.fetchSub(1, .acq_rel) == 1) {
            const doomed = sh.arena;
            doomed.deinit();
        }
    }
};

/// Deep copy of the shapes events are made of: strings, numbers, enums,
/// optionals, structs and unions of those.
fn clone(comptime T: type, a: Allocator, v: T) Allocator.Error!T {
    switch (@typeInfo(T)) {
        .pointer => |p| {
            comptime std.debug.assert(p.size == .slice and p.child == u8);
            return a.dupe(u8, v);
        },
        .optional => |o| return if (v) |x| try clone(o.child, a, x) else null,
        .@"struct" => |s| {
            var out: T = undefined;
            inline for (s.field_names, s.field_types) |name, ty| @field(out, name) = try clone(ty, a, @field(v, name));
            return out;
        },
        .@"union" => switch (v) {
            inline else => |payload, tag| return @unionInit(T, @tagName(tag), try clone(@TypeOf(payload), a, payload)),
        },
        else => return v,
    }
}

/// An event taken off a queue. `release` it when done with `event()`.
pub const Item = struct {
    shared: *Shared,

    pub fn event(i: Item) Event {
        return i.shared.event;
    }

    pub fn release(i: Item) void {
        i.shared.unref();
    }
};

pub const Subscription = struct {
    bus: *Bus,
    /// Waiters sleep here, on the bus mutex.
    cond: Io.Condition = .init,
    // Everything below is guarded by `bus.mutex`.
    ring: []*Shared,
    head: usize = 0,
    len: usize = 0,
    closed: bool = false,
    resync: bool = false,

    /// Waits for the next event. After the bus or the owner closed the
    /// subscription, the events still queued come out first, then `Closed`.
    pub fn next(sub: *Subscription, timeout_ms: ?u32) NextError!Item {
        const bus = sub.bus;
        const io = bus.io;
        const timeout: Io.Timeout = if (timeout_ms) |ms|
            (Io.Timeout{ .duration = .{ .raw = .fromMilliseconds(ms), .clock = .awake } }).toDeadline(io)
        else
            .none;
        bus.mutex.lockUncancelable(io);
        defer bus.mutex.unlock(io);
        while (true) {
            if (sub.len > 0) {
                const sh = sub.ring[sub.head];
                sub.head = (sub.head + 1) % sub.ring.len;
                sub.len -= 1;
                return .{ .shared = sh };
            }
            if (sub.closed) return error.Closed;
            sub.cond.waitTimeout(io, &bus.mutex, timeout) catch |err| switch (err) {
                error.Timeout => return error.Timeout,
                error.Canceled => return error.Canceled,
            };
        }
    }

    /// Ends the subscription. Safe to call more than once and from any
    /// thread; it keeps the reason if the bus closed the subscription first.
    pub fn close(sub: *Subscription) void {
        const bus = sub.bus;
        bus.mutex.lockUncancelable(bus.io);
        defer bus.mutex.unlock(bus.io);
        bus.closeLocked(sub, false);
    }

    /// Why the bus closed the subscription: `error.Resync` when it missed a
    /// lifecycle event, null while open and after the owner closed it.
    pub fn reason(sub: *Subscription) ?Reason {
        const bus = sub.bus;
        bus.mutex.lockUncancelable(bus.io);
        defer bus.mutex.unlock(bus.io);
        return if (sub.resync) error.Resync else null;
    }

    /// Closes, drops what is still queued and frees the subscription. No
    /// other thread may be inside `next`.
    pub fn deinit(sub: *Subscription) void {
        const bus = sub.bus;
        bus.mutex.lockUncancelable(bus.io);
        bus.closeLocked(sub, false);
        while (sub.len > 0) {
            sub.ring[sub.head].unref();
            sub.head = (sub.head + 1) % sub.ring.len;
            sub.len -= 1;
        }
        bus.live -= 1;
        bus.mutex.unlock(bus.io);
        bus.gpa.free(sub.ring);
        bus.gpa.destroy(sub);
    }
};

pub const Bus = struct {
    gpa: Allocator,
    io: Io,
    options: Options,
    mutex: Io.Mutex = .init,
    // Guarded by `mutex`.
    subs: std.ArrayList(*Subscription) = .empty,
    /// Subscriptions not yet deinit'ed.
    live: usize = 0,
    closed: bool = false,

    /// `gpa` must be thread-safe: any thread publishes.
    pub fn init(gpa: Allocator, io: Io, options: Options) Allocator.Error!*Bus {
        std.debug.assert(options.buffer > 0);
        const b = try gpa.create(Bus);
        b.* = .{ .gpa = gpa, .io = io, .options = options };
        return b;
    }

    /// Closes every subscription; all must be deinit'ed before.
    pub fn deinit(b: *Bus) void {
        b.close();
        std.debug.assert(b.live == 0);
        b.subs.deinit(b.gpa);
        b.gpa.destroy(b);
    }

    /// Closes every subscription (without a reason) and refuses new ones:
    /// later `subscribe` calls return an already closed subscription.
    pub fn close(b: *Bus) void {
        b.mutex.lockUncancelable(b.io);
        defer b.mutex.unlock(b.io);
        b.closed = true;
        while (b.subs.items.len > 0) b.closeLocked(b.subs.items[b.subs.items.len - 1], false);
    }

    /// A new subscriber that receives every event published from now on.
    pub fn subscribe(b: *Bus) Allocator.Error!*Subscription {
        const sub = try b.gpa.create(Subscription);
        errdefer b.gpa.destroy(sub);
        const ring = try b.gpa.alloc(*Shared, b.options.buffer);
        errdefer b.gpa.free(ring);
        sub.* = .{ .bus = b, .ring = ring };

        b.mutex.lockUncancelable(b.io);
        defer b.mutex.unlock(b.io);
        if (b.closed) {
            sub.closed = true;
        } else {
            try b.subs.append(b.gpa, sub);
        }
        b.live += 1;
        return sub;
    }

    pub fn publisher(b: *Bus) Publisher {
        return .{ .ctx = b, .publish_fn = publishFn };
    }

    fn publishFn(ctx: *anyopaque, e: Event) void {
        const b: *Bus = @ptrCast(@alignCast(ctx));
        b.publish(e);
    }

    /// Hands `e` to every subscriber and never blocks. A subscriber whose
    /// queue is full misses `e`: silently if it is a progress event,
    /// otherwise it is closed with `error.Resync`. Strings in `e` are
    /// borrowed for the call.
    pub fn publish(b: *Bus, e: Event) void {
        const io = b.io;
        b.mutex.lockUncancelable(io);
        defer b.mutex.unlock(io);
        if (b.subs.items.len == 0) return;
        const lifecycle = !e.kind().isProgress();

        const sh = Shared.create(b.gpa, e) catch {
            // Nobody can be told, so every subscriber that cannot afford to
            // miss it must refetch.
            if (lifecycle) while (b.subs.items.len > 0) b.closeLocked(b.subs.items[b.subs.items.len - 1], true);
            return;
        };
        // Every queue that takes it adds a reference below; nobody can
        // release before the mutex is dropped.
        var i: usize = b.subs.items.len;
        while (i > 0) {
            i -= 1;
            const sub = b.subs.items[i];
            if (sub.len < sub.ring.len) {
                sub.ring[(sub.head + sub.len) % sub.ring.len] = sh;
                sub.len += 1;
                _ = sh.refs.fetchAdd(1, .monotonic);
                sub.cond.signal(io);
            } else if (lifecycle) {
                b.closeLocked(sub, true);
            }
        }
        if (sh.refs.load(.monotonic) == 0) {
            const doomed = sh.arena;
            doomed.deinit();
        }
    }

    /// Detaches `sub` and wakes its reader. Closing twice changes nothing,
    /// including the reason. The caller holds `mutex`.
    fn closeLocked(b: *Bus, sub: *Subscription, resync: bool) void {
        if (sub.closed) return;
        sub.closed = true;
        sub.resync = resync;
        for (b.subs.items, 0..) |s, i| if (s == sub) {
            _ = b.subs.swapRemove(i);
            break;
        };
        sub.cond.broadcast(b.io);
    }
};

const testing = std.testing;

fn newBus(buffer: usize) !*Bus {
    return Bus.init(testing.allocator, testing.io, .{ .buffer = buffer });
}

fn expectRemoved(sub: *Subscription, id: []const u8) !void {
    const it = try sub.next(1000);
    defer it.release();
    try testing.expectEqualStrings(id, it.event().removed);
}

test "a subscriber receives the published events in order" {
    const bus = try newBus(8);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();

    bus.publisher().publish(.{ .removed = "a" });
    bus.publisher().publish(.{ .fetched = "b" });

    try expectRemoved(sub, "a");
    const it = try sub.next(1000);
    defer it.release();
    try testing.expectEqualStrings("b", it.event().fetched);
}

test "the bus keeps its own copy of the strings" {
    const bus = try newBus(8);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();

    var buf = "gone".*;
    bus.publish(.{ .ready = .{ .id = &buf, .output_path = "/data/out" } });
    @memset(&buf, 'x');

    const it = try sub.next(1000);
    defer it.release();
    try testing.expectEqualStrings("gone", it.event().ready.id);
    try testing.expectEqualStrings("/data/out", it.event().ready.output_path);
}

test "a Download snapshot is copied deeply and shared by every subscriber" {
    const bus = try newBus(8);
    defer bus.deinit();
    const a = try bus.subscribe();
    defer a.deinit();
    const b = try bus.subscribe();
    defer b.deinit();

    var name = "movie".*;
    bus.publish(.{ .download_added = .{ .id = "d1", .name = &name, .source_kind = .nzb, .state = .queued, .added_at = 1, .updated_at = 2 } });
    @memset(&name, 'x');

    const ia = try a.next(1000);
    defer ia.release();
    const ib = try b.next(1000);
    defer ib.release();
    try testing.expectEqualStrings("movie", ia.event().download_added.name);
    try testing.expect(ia.shared == ib.shared);
}

test "a subscriber that is not draining holds no reference after deinit" {
    const bus = try newBus(8);
    defer bus.deinit();
    const sub = try bus.subscribe();
    bus.publish(.{ .removed = "queued and never read" });
    sub.deinit(); // leak check: the queued event is freed
}

test "publishing with no subscribers keeps nothing" {
    const bus = try newBus(8);
    defer bus.deinit();
    bus.publish(.{ .removed = "nobody listens" });
}

test "progress events are dropped when the subscriber is full" {
    const bus = try newBus(4);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();

    for (0..4) |_| bus.publish(.{ .download_progress = .{ .id = "a", .done_bytes = 1, .total_bytes = 2 } });
    bus.publish(.{ .download_progress = .{ .id = "dropped", .done_bytes = 1, .total_bytes = 2 } });

    for (0..4) |_| {
        const it = try sub.next(1000);
        defer it.release();
        try testing.expectEqualStrings("a", it.event().download_progress.id);
    }
    bus.publish(.{ .fetched = "b" });
    const it = try sub.next(1000);
    defer it.release();
    try testing.expectEqualStrings("b", it.event().fetched);
    try testing.expectEqual(null, sub.reason());
}

test "a lifecycle overflow closes the subscriber with Resync after its queued events" {
    const bus = try newBus(4);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();
    for (0..4) |_| bus.publish(.{ .fetched = "a" });

    bus.publish(.{ .ready = .{ .id = "a", .output_path = "/x" } }); // cannot be delivered

    for (0..4) |_| {
        const it = try sub.next(1000);
        defer it.release();
        try testing.expectEqualStrings("a", it.event().fetched);
    }
    try testing.expectError(error.Closed, sub.next(1000));
    try testing.expectEqual(error.Resync, sub.reason().?);
}

test "close is idempotent, stops delivery and spares the others" {
    const bus = try newBus(8);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();
    const other = try bus.subscribe();
    defer other.deinit();

    sub.close();
    sub.close();
    bus.publish(.{ .removed = "a" });

    try testing.expectError(error.Closed, sub.next(1000));
    try testing.expectEqual(null, sub.reason());
    try expectRemoved(other, "a");
}

test "closing after a resync keeps the resync reason" {
    const bus = try newBus(2);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();
    for (0..3) |_| bus.publish(.{ .fetched = "a" });

    sub.close();

    try testing.expectEqual(error.Resync, sub.reason().?);
}

test "closing the bus closes every subscription, and later ones start closed" {
    const bus = try newBus(8);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();

    bus.close();
    const late = try bus.subscribe();
    defer late.deinit();
    bus.publish(.{ .removed = "a" });

    try testing.expectError(error.Closed, sub.next(1000));
    try testing.expectError(error.Closed, late.next(1000));
    try testing.expectEqual(null, sub.reason());
}

test "next times out when nothing arrives" {
    const bus = try newBus(8);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();
    try testing.expectError(error.Timeout, sub.next(20));
}

const Waiter = struct {
    sub: *Subscription,
    result: ?NextError = null,

    fn run(w: *Waiter) void {
        if (w.sub.next(5000)) |it| it.release() else |err| w.result = err;
    }
};

test "closing from another thread wakes a waiting reader" {
    const bus = try newBus(8);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();
    var w: Waiter = .{ .sub = sub };
    var fut = try testing.io.concurrent(Waiter.run, .{&w});
    try testing.io.sleep(.fromMilliseconds(50), .awake);

    sub.close();
    fut.await(testing.io);

    try testing.expectEqual(error.Closed, w.result.?);
}

test "a publish wakes a waiting reader" {
    const bus = try newBus(8);
    defer bus.deinit();
    const sub = try bus.subscribe();
    defer sub.deinit();
    var w: Waiter = .{ .sub = sub };
    var fut = try testing.io.concurrent(Waiter.run, .{&w});
    try testing.io.sleep(.fromMilliseconds(50), .awake);

    bus.publish(.{ .removed = "a" });
    fut.await(testing.io);

    try testing.expectEqual(null, w.result);
}

const Hammer = struct {
    bus: *Bus,

    fn publish(h: Hammer) void {
        for (0..500) |_| {
            h.bus.publish(.{ .ready = .{ .id = "a", .output_path = "/p" } });
            h.bus.publish(.{ .download_progress = .{ .id = "a", .done_bytes = 1, .total_bytes = 2 } });
        }
    }

    fn churn(h: Hammer) void {
        for (0..200) |_| {
            const sub = h.bus.subscribe() catch return;
            sub.close();
            sub.close();
            sub.deinit();
        }
    }

    fn drain(h: Hammer) void {
        for (0..100) |_| {
            const sub = h.bus.subscribe() catch return;
            defer sub.deinit();
            for (0..20) |_| {
                const it = sub.next(1) catch continue;
                it.release();
            }
        }
    }
};

test "publish is race free with subscribe, read and close" {
    const bus = try newBus(16);
    defer bus.deinit();
    var group: Io.Group = .init;
    defer group.cancel(testing.io);
    const h: Hammer = .{ .bus = bus };
    for (0..4) |_| {
        try group.concurrent(testing.io, Hammer.publish, .{h});
        try group.concurrent(testing.io, Hammer.churn, .{h});
        try group.concurrent(testing.io, Hammer.drain, .{h});
    }
    try group.await(testing.io);
}

const Stuck = struct {
    bus: *Bus,
    done: std.atomic.Value(bool) = .init(false),

    fn publishAll(s: *Stuck) void {
        for (0..2000) |i| {
            if (i % 2 == 0) {
                s.bus.publish(.{ .download_progress = .{ .id = "a", .done_bytes = 1, .total_bytes = 2 } });
            } else {
                s.bus.publish(.{ .fetched = "a" });
            }
        }
        s.done.store(true, .release);
    }
};

test "a subscriber that never reads does not block publishers or the others" {
    const bus = try newBus(16);
    defer bus.deinit();
    const stuck = try bus.subscribe();
    defer stuck.deinit();
    const reader = try bus.subscribe();
    defer reader.deinit();

    var s: Stuck = .{ .bus = bus };
    var fut = try testing.io.concurrent(Stuck.publishAll, .{&s});
    var got: usize = 0;
    while (!s.done.load(.acquire)) {
        const it = reader.next(5) catch |err| switch (err) {
            error.Timeout => continue,
            error.Closed => break,
            error.Canceled => return error.Canceled,
        };
        it.release();
        got += 1;
    }
    fut.await(testing.io);
    try testing.expectEqual(error.Resync, stuck.reason().?);
}
