//! SABnzbd queue JSON and queue-wide helpers (M4 / Z5).
const std = @import("std");

const download = @import("download.zig");
const sabnzbd_status = @import("sabnzbd_status.zig");

/// SABnzbd `nzo_id` for one queue slot from a Download.
pub fn queueSlotNzoID(arena: std.mem.Allocator, d: download.Download) []const u8 {
    return sabnzbd_status.nzoID(arena, d.id);
}

/// SABnzbd queue slot `filename` from a Download.
pub fn queueSlotFilename(d: download.Download) []const u8 {
    return d.name;
}

/// SABnzbd queue slot `priority` label from a Download (view.PriorityLabel).
pub fn queueSlotPriority(d: download.Download) []const u8 {
    return switch (d.priority) {
        .force => "Force",
        .high => "High",
        .low => "Low",
        .normal => "Normal",
    };
}

/// Queue-wide seconds left at global rate; 0 when rate unknown or nothing left.
pub fn queueStatus(paused: bool, downloading: bool) []const u8 {
    if (paused) return "Paused";
    if (downloading) return "Downloading";
    return "Idle";
}

pub fn queueTimeLeft(left: i64, bps: i64) i64 {
    if (bps <= 0 or left <= 0) return 0;
    return @divTrunc(left, bps);
}

pub fn page(n: i64, start: i64, limit: i64) struct { lo: i64, hi: i64 } {
    const count: i64 = @max(n, @as(i64, 0));
    const lo: i64 = @min(start, count);
    var hi: i64 = count;
    if (limit > 0) hi = @min(lo + limit, count);
    return .{ .lo = lo, .hi = hi };
}

test "page clamps start and applies positive limit" {
    const r = page(10, 2, 3);
    try std.testing.expectEqual(@as(i64, 2), r.lo);
    try std.testing.expectEqual(@as(i64, 5), r.hi);
}

test "queueTimeLeft is left divided by bps when both positive" {
    try std.testing.expectEqual(@as(i64, 1), queueTimeLeft(1024, 1024));
}

test "queueStatus is Idle when not paused and not downloading" {
    try std.testing.expectEqualStrings("Idle", queueStatus(false, false));
}

test "queueSlotFilename is download name for queue slot" {
    const d = download.Download{
        .id = "x",
        .name = "Show.S01E01",
        .source_kind = .nzb,
        .state = .queued,
        .added_at = 0,
        .updated_at = 0,
    };
    try std.testing.expectEqualStrings("Show.S01E01", queueSlotFilename(d));
}

test "queueSlotNzoID maps download id to nzo_id wire string" {
    const testing = std.testing;
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const d = download.Download{
        .id = "abc",
        .name = "n",
        .source_kind = .nzb,
        .state = .queued,
        .added_at = 0,
        .updated_at = 0,
    };
    try testing.expectEqualStrings("SABnzbd_nzo_abc", queueSlotNzoID(arena.allocator(), d));
}

test "queueSlotPriority is High for download priority high" {
    const d = download.Download{
        .id = "x",
        .name = "n",
        .source_kind = .nzb,
        .state = .queued,
        .priority = .high,
        .added_at = 0,
        .updated_at = 0,
    };
    try std.testing.expectEqualStrings("High", queueSlotPriority(d));
}
