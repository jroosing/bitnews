//! Timestamps in JSON: what Go's encoding/json writes for a time.Time that
//! the store produced (UTC, millisecond resolution): RFC 3339 with the
//! fraction trimmed of trailing zeros and left out when zero, "Z" for UTC.
const std = @import("std");
const Io = std.Io;

/// Writes `unix_ms` as "2026-10-29T21:11:41.873Z" (no quotes).
pub fn writeRfc3339(w: *Io.Writer, unix_ms: i64) Io.Writer.Error!void {
    const secs = @divFloor(unix_ms, 1000);
    const ms: u32 = @intCast(@mod(unix_ms, 1000));
    const epoch: std.time.epoch.EpochSeconds = .{ .secs = @intCast(@max(secs, 0)) };
    const day = epoch.getEpochDay().calculateYearDay();
    const md = day.calculateMonthDay();
    const ds = epoch.getDaySeconds();
    try w.print("{d:0>4}-{d:0>2}-{d:0>2}T{d:0>2}:{d:0>2}:{d:0>2}", .{
        day.year,             md.month.numeric(),      md.day_index + 1,
        ds.getHoursIntoDay(), ds.getMinutesIntoHour(), ds.getSecondsIntoMinute(),
    });
    if (ms != 0) {
        var frac: [3]u8 = undefined;
        _ = std.fmt.bufPrint(&frac, "{d:0>3}", .{ms}) catch unreachable;
        var n: usize = 3;
        while (frac[n - 1] == '0') n -= 1;
        try w.print(".{s}", .{frac[0..n]});
    }
    try w.writeByte('Z');
}

const testing = std.testing;

fn render(ms: i64) ![]u8 {
    var aw: Io.Writer.Allocating = .init(testing.allocator);
    errdefer aw.deinit();
    try writeRfc3339(&aw.writer, ms);
    return aw.toOwnedSlice();
}

test "timestamps are Go's RFC 3339 for a UTC time" {
    // time.UnixMilli(ms).UTC() marshalled by encoding/json
    const cases = [_]struct { ms: i64, want: []const u8 }{
        .{ .ms = 0, .want = "1970-01-01T00:00:00Z" },
        .{ .ms = 1793365901873, .want = "2026-10-30T13:11:41.873Z" },
        .{ .ms = 1793365901870, .want = "2026-10-30T13:11:41.87Z" },
        .{ .ms = 1793365901800, .want = "2026-10-30T13:11:41.8Z" },
        .{ .ms = 1793365901001, .want = "2026-10-30T13:11:41.001Z" },
        .{ .ms = 1793365901000, .want = "2026-10-30T13:11:41Z" },
        .{ .ms = 951782400000, .want = "2000-02-29T00:00:00Z" },
    };
    for (cases) |c| {
        const got = try render(c.ms);
        defer testing.allocator.free(got);
        try testing.expectEqualStrings(c.want, got);
    }
}
