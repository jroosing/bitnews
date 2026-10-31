//! Live cache of facts the bus does not persist, for the SABnzbd adapter
//! (Go `internal/daemon/tracker.go`). Starts empty: a Download already in a
//! stage after a restart shows no stage until that stage is published again.
//!
//! `gpa` owns every map key. `apply` copies the download id out of the event
//! before returning; the event's buffers are not retained. The maps are
//! guarded by a spin mutex so the event bus and HTTP readers can share one
//! `Live` (this cache is built with an allocator only, no `Io`). A new key
//! is refused once the two maps together hold 10000 keys.
const std = @import("std");
const event = @import("event.zig");

/// Hostile-flood cap: owned keys across both maps. An insert that would pass
/// this is dropped; in-place updates and deletes still run.
const max_keys: u32 = 10_000;

pub const Live = struct {
    gpa: std.mem.Allocator,
    /// Event-bus writers and HTTP readers share the maps. `std.atomic.Mutex`
    /// because `init` has no `Io` (`std.Io.Mutex` needs one to lock).
    mu: std.atomic.Mutex = .unlocked,
    /// Download id (`gpa`) -> post-processing stage.
    stages: std.StringHashMapUnmanaged(event.Stage) = .empty,
    /// Download id (`gpa`) -> bytes per second while fetching.
    rates: std.StringHashMapUnmanaged(i64) = .empty,

    pub fn init(gpa: std.mem.Allocator) Live {
        return .{ .gpa = gpa };
    }

    pub fn deinit(self: *Live) void {
        self.lock();
        self.freeMaps();
        self.unlock();
    }

    pub fn apply(self: *Live, ev: event.Event) void {
        self.lock();
        defer self.unlock();
        switch (ev) {
            .postprocess_stage => |p| self.putStage(p.id, p.stage),
            .download_progress => |p| self.putRate(p.id, p.bytes_per_second),
            .download_updated => |d| {
                if (d.state != .post_processing) self.deleteStage(d.id);
                if (d.state != .fetching) self.deleteRate(d.id);
            },
            .ready => |r| self.deleteStage(r.id),
            .failed => |f| {
                self.deleteStage(f.id);
                self.deleteRate(f.id);
            },
            .fetched => |id| self.deleteRate(id),
            .removed => |id| {
                self.deleteStage(id);
                self.deleteRate(id);
            },
            else => {},
        }
    }

    /// Post-processing stage of a Download, if known.
    pub fn stage(self: *Live, id: []const u8) ?event.Stage {
        self.lock();
        defer self.unlock();
        return self.stages.get(id);
    }

    /// Current rate of one Download being fetched, 0 when unknown.
    pub fn rate(self: *Live, id: []const u8) i64 {
        self.lock();
        defer self.unlock();
        return self.rates.get(id) orelse 0;
    }

    /// Sum of the rates of the Downloads being fetched.
    pub fn speed(self: *Live) i64 {
        self.lock();
        defer self.unlock();
        var sum: i64 = 0;
        var it = self.rates.valueIterator();
        while (it.next()) |r| sum += r.*;
        return sum;
    }

    /// Drops every cached stage and rate. The bus calls this after a
    /// subscription was dropped and events were missed.
    pub fn reset(self: *Live) void {
        self.lock();
        defer self.unlock();
        self.freeMaps();
    }

    fn lock(self: *Live) void {
        while (!self.mu.tryLock()) std.atomic.spinLoopHint();
    }

    fn unlock(self: *Live) void {
        self.mu.unlock();
    }

    fn full(self: *const Live) bool {
        return self.stages.count() + self.rates.count() >= max_keys;
    }

    fn putStage(self: *Live, id: []const u8, stage_name: event.Stage) void {
        if (self.stages.getPtr(id)) |slot| {
            slot.* = stage_name;
            return;
        }
        if (self.full()) return;
        const key = self.gpa.dupe(u8, id) catch return;
        self.stages.putNoClobber(self.gpa, key, stage_name) catch {
            self.gpa.free(key);
            return;
        };
    }

    fn putRate(self: *Live, id: []const u8, bytes_per_second: i64) void {
        if (self.rates.getPtr(id)) |slot| {
            slot.* = bytes_per_second;
            return;
        }
        if (self.full()) return;
        const key = self.gpa.dupe(u8, id) catch return;
        self.rates.putNoClobber(self.gpa, key, bytes_per_second) catch {
            self.gpa.free(key);
            return;
        };
    }

    fn deleteStage(self: *Live, id: []const u8) void {
        const kv = self.stages.fetchRemove(id) orelse return;
        self.gpa.free(kv.key);
    }

    fn deleteRate(self: *Live, id: []const u8) void {
        const kv = self.rates.fetchRemove(id) orelse return;
        self.gpa.free(kv.key);
    }

    fn freeMaps(self: *Live) void {
        freeKeys(event.Stage, self.gpa, &self.stages);
        freeKeys(i64, self.gpa, &self.rates);
    }
};

fn freeKeys(comptime V: type, gpa: std.mem.Allocator, map: *std.StringHashMapUnmanaged(V)) void {
    var it = map.keyIterator();
    while (it.next()) |k| gpa.free(k.*);
    map.deinit(gpa);
    map.* = .empty;
}

const testing = std.testing;

test "Live copies postprocess_stage download id on apply" {
    var live = Live.init(testing.allocator);
    defer live.deinit();

    var id_buf: [3]u8 = .{ 'a', 'b', 'c' };
    const ev: event.Event = .{ .postprocess_stage = .{ .id = id_buf[0..], .stage = .verifying } };
    live.apply(ev);
    try testing.expectEqual(event.Stage.verifying, live.stage("abc").?);

    id_buf[0] = 'x';
    try testing.expectEqual(event.Stage.verifying, live.stage("abc").?);
    try testing.expect(live.stage("xbc") == null);
}
