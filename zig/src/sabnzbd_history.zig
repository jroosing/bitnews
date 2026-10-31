//! SABnzbd history JSON and history-wide helpers (M4 / Z5).
const std = @import("std");

const download = @import("download.zig");
const sabnzbd_status = @import("sabnzbd_status.zig");

/// History entry `status` wire string for a Download shown in history.
pub fn historyStatus(d: download.Download) []const u8 {
    return sabnzbd_status.sabStatus(d, null);
}

/// SABnzbd `nzo_id` for one history slot from a Download.
pub fn historySlotNzoID(arena: std.mem.Allocator, d: download.Download) []const u8 {
    return sabnzbd_status.nzoID(arena, d.id);
}

/// Shown history slot count (`noofslots` in API JSON).
pub fn noOfSlots(len: usize) i64 {
    return @intCast(len);
}

test "noOfSlots zero length is zero" {
    try std.testing.expectEqual(@as(i64, 0), noOfSlots(0));
}

test "historySlotNzoID maps download id to nzo_id wire string" {
    const testing = std.testing;
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const d = download.Download{
        .id = "abc",
        .name = "n",
        .source_kind = .nzb,
        .state = .ready,
        .added_at = 0,
        .updated_at = 0,
    };
    try testing.expectEqualStrings("SABnzbd_nzo_abc", historySlotNzoID(arena.allocator(), d));
}

test "historyStatus ready returns Completed" {
    const d = download.Download{
        .id = "x",
        .name = "n",
        .source_kind = .nzb,
        .state = .ready,
        .added_at = 1,
        .updated_at = 1,
    };
    try std.testing.expectEqualStrings("Completed", historyStatus(d));
}
